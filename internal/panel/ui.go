package panel

import (
	_ "embed"
	"net/http"
)

// indexHTML is the operator UI. It is a single self-contained document so the
// panel has no build step and no static asset directory to ship.
//
//go:embed ui/index.html
var indexHTML []byte

// handleIndex serves the operator UI.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/ui" && r.URL.Path != "/ui/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// The UI is a single document that talks to the JSON API, so a strict CSP
	// with no external origins is enough.
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'")
	_, _ = w.Write(indexHTML)
}

// installScript is the node bootstrap script, embedded so the panel can serve
// it without a separate asset directory.
//
//go:embed ui/install.sh
var installScript []byte

// handleInstallScript serves the node bootstrap script.
//
// The script is generic: every panel specific value is passed as an argument,
// so the same file works for any deployment and no secret is baked in.
func (s *Server) handleInstallScript(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(installScript)
}
