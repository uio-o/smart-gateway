package panel

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// handleDownload serves an agent binary.
//
// Binary distribution is deliberately simple: the panel proxies a GitHub
// release asset so an installer can fetch everything from one origin. An
// operator can point -agent-dir at a local directory for air gapped nodes.
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Accepted forms, mirroring install.sh --release:
	//   /download/smart-gateway-agent-linux-amd64
	//   /download/<release>/smart-gateway-agent-linux-amd64
	//
	// The release segment is optional and may be any tag the operator pinned.
	rel := strings.TrimPrefix(r.URL.Path, "/download/")
	rel = path.Clean("/" + rel)
	rel = strings.TrimPrefix(rel, "/")
	if rel == "" || strings.HasPrefix(rel, ".") {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	// Split an optional leading release tag from the asset file name. Only the
	// base name is ever joined to a directory, so a tag cannot traverse out of
	// agentDir, and a bare file name keeps working for the "latest" form.
	release, name := "", rel
	if i := strings.LastIndex(rel, "/"); i >= 0 {
		release, name = rel[:i], rel[i+1:]
	}
	if !strings.HasPrefix(name, "smart-gateway-") || strings.Contains(release, "..") {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	// A local directory wins so a node can be provisioned without internet.
	//
	// A pinned release is served only from its own subdirectory. Falling back to
	// the bare file name would silently hand out whatever version happens to be
	// staged while the operator believes they pinned a specific tag.
	if s.agentDir != "" {
		var candidates []string
		if release != "" {
			candidates = []string{filepath.Join(s.agentDir, release, name)}
		} else {
			candidates = []string{filepath.Join(s.agentDir, name)}
		}
		for _, local := range candidates {
			// filepath.Join has already cleaned the path; verify the result is
			// still inside agentDir before serving it.
			root, err := filepath.Abs(s.agentDir)
			if err != nil {
				break
			}
			abs, err := filepath.Abs(local)
			if err != nil || (abs != root && !strings.HasPrefix(abs, root+string(os.PathSeparator))) {
				continue
			}
			if _, err := os.Stat(abs); err == nil {
				w.Header().Set("Content-Type", "application/octet-stream")
				http.ServeFile(w, r, abs)
				return
			}
		}
	}

	if s.releaseBase == "" {
		http.Error(w, "no binary source is configured on this panel", http.StatusNotFound)
		return
	}

	base := strings.TrimSuffix(s.releaseBase, "/")
	url := base + "/" + name
	if release != "" {
		// A pinned release lives under releases/download/<tag>/, whereas the
		// default base points at releases/latest/download. Rewrite that
		// segment so a pinned request does not become latest/download/<tag>/.
		if strings.HasSuffix(base, "/latest/download") {
			url = strings.TrimSuffix(base, "/latest/download") + "/download/" + release + "/" + name
		} else {
			url = base + "/" + release + "/" + name
		}
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, url, nil)
	if err != nil {
		http.Error(w, "bad release url", http.StatusInternalServerError)
		return
	}
	resp, err := s.downloadClient.Do(req)
	if err != nil {
		http.Error(w, "release download failed", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		http.Error(w, fmt.Sprintf("release returned %d", resp.StatusCode), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	if n := resp.ContentLength; n > 0 {
		w.Header().Set("Content-Length", fmt.Sprint(n))
	}
	_, _ = io.Copy(w, resp.Body)
}
