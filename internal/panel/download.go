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

	// Accepted forms:
	//   /download/smart-gateway-agent-linux-amd64
	//   /download/v0.1.0/smart-gateway-agent-linux-amd64
	rel := strings.TrimPrefix(r.URL.Path, "/download/")
	rel = path.Clean("/" + rel)
	rel = strings.TrimPrefix(rel, "/")
	if rel == "" || strings.HasPrefix(rel, ".") || !strings.HasPrefix(rel, "smart-gateway-") {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	// A local directory wins so a node can be provisioned without internet.
	if s.agentDir != "" {
		local := filepath.Join(s.agentDir, filepath.Base(rel))
		if _, err := os.Stat(local); err == nil {
			w.Header().Set("Content-Type", "application/octet-stream")
			http.ServeFile(w, r, local)
			return
		}
	}

	if s.releaseBase == "" {
		http.Error(w, "no binary source is configured on this panel", http.StatusNotFound)
		return
	}

	url := fmt.Sprintf("%s/%s", strings.TrimSuffix(s.releaseBase, "/"), rel)
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
