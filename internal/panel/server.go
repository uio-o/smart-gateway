package panel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/uio-o/smart-gateway/internal/store"
)

// Server exposes the control plane over HTTP.
type Server struct {
	store *store.Store
	// token is the operator credential for the panel API and UI.
	token string
	// mountPrefix lets the panel be served under a sub path.
	mountPrefix string
	// agentDir, when set, is searched first for agent binaries.
	agentDir string
	// releaseBase is the origin a binary download is proxied from.
	releaseBase string

	downloadClient *http.Client
}

// Options configures the panel server.
type Options struct {
	Store *store.Store
	// AdminToken authenticates operator requests. It is required.
	AdminToken string
	// MountPrefix is prepended to every panel route.
	MountPrefix string
	// AgentDir is a local directory holding agent binaries, checked before
	// the release origin. Useful where nodes cannot reach GitHub.
	AgentDir string
	// ReleaseBase is the origin prefix agent binaries are fetched from.
	// Defaults to the project's GitHub releases.
	ReleaseBase string
}

// New creates a panel server.
func New(opts Options) (*Server, error) {
	if opts.Store == nil {
		return nil, errors.New("store is required")
	}
	if strings.TrimSpace(opts.AdminToken) == "" {
		return nil, errors.New("admin token is required")
	}
	prefix := strings.TrimSuffix(opts.MountPrefix, "/")
	releaseBase := strings.TrimSuffix(opts.ReleaseBase, "/")
	if releaseBase == "" {
		releaseBase = defaultReleaseBase
	}
	return &Server{
		store:          opts.Store,
		token:          opts.AdminToken,
		mountPrefix:    prefix,
		agentDir:       opts.AgentDir,
		releaseBase:    releaseBase,
		downloadClient: &http.Client{Timeout: 10 * time.Minute},
	}, nil
}

// defaultReleaseBase points at the project's own release assets.
const defaultReleaseBase = "https://github.com/uio-o/smart-gateway/releases/latest/download"

// Handler returns the panel HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Operator UI and the bootstrap installer. Both are unauthenticated pages
	// that contain no secrets: the operator types the token, and the installer
	// is a generic script whose values arrive as arguments.
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/install.sh", s.handleInstallScript)
	mux.HandleFunc("/download/", s.handleDownload)

	// Agent facing endpoints. Agents authenticate with their own token.
	mux.HandleFunc("/api/agent/config", s.wrap(s.handleAgentConfig, authAgent))
	mux.HandleFunc("/api/agent/heartbeat", s.wrap(s.handleAgentHeartbeat, authAgent))

	// Operator facing endpoints.
	mux.HandleFunc("/api/summary", s.wrap(s.handleSummary, authAdmin))
	mux.HandleFunc("/api/nodes", s.wrap(s.handleNodes, authAdmin))
	mux.HandleFunc("/api/nodes/", s.wrap(s.handleNodeItem, authAdmin))
	mux.HandleFunc("/api/services", s.wrap(s.handleServices, authAdmin))
	mux.HandleFunc("/api/services/", s.wrap(s.handleServiceItem, authAdmin))
	mux.HandleFunc("/api/routes", s.wrap(s.handleRoutes, authAdmin))
	mux.HandleFunc("/api/routes/", s.wrap(s.handleRouteItem, authAdmin))
	mux.HandleFunc("/api/tokens", s.wrap(s.handleTokens, authAdmin))
	mux.HandleFunc("/api/tokens/", s.wrap(s.handleTokenItem, authAdmin))
	mux.HandleFunc("/api/settings", s.wrap(s.handleSettings, authAdmin))
	mux.HandleFunc("/api/render", s.wrap(s.handleRender, authAdmin))

	// Liveness is public so an uptime check can run without credentials.
	mux.HandleFunc("/healthz", s.handleHealthz)
	handler := http.Handler(mux)
	if s.mountPrefix != "" {
		// The stripped prefix is registered on a fresh mux. Registering the
		// prefix on the inner mux would clash with the catch-all UI route.
		outer := http.NewServeMux()
		outer.Handle(s.mountPrefix+"/", http.StripPrefix(s.mountPrefix, mux))
		handler = outer
	}
	return handler
}

type authMode int

const (
	authAdmin authMode = iota
	authAgent
)

// wrap applies method limits and authentication to a handler.
func (s *Server) wrap(fn func(http.ResponseWriter, *http.Request) error, mode authMode) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := s.authenticate(r, mode); err != nil {
			writeError(w, http.StatusUnauthorized, err.Error())
			return
		}
		if err := fn(w, r); err != nil {
			status := http.StatusInternalServerError
			var he *httpError
			if errors.As(err, &he) {
				status = he.Status
			} else if errors.Is(err, store.ErrNotFound) {
				status = http.StatusNotFound
			}
			writeError(w, status, err.Error())
		}
	}
}

// authenticate checks the caller credential for the requested surface.
func (s *Server) authenticate(r *http.Request, mode authMode) error {
	token := bearerToken(r)
	if token == "" {
		return errors.New("missing bearer token")
	}
	if mode == authAdmin {
		if subtleEqual(token, s.token) {
			return nil
		}
		return errors.New("invalid panel token")
	}

	if subtleEqual(token, s.token) {
		return nil
	}
	ok, err := s.store.ValidToken(r.Context(), token)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("invalid agent token")
	}
	return nil
}

func bearerToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if after, ok := strings.CutPrefix(h, "Bearer "); ok {
			return strings.TrimSpace(after)
		}
	}
	return strings.TrimSpace(r.Header.Get("X-Panel-Token"))
}

// subtleEqual compares two secrets without an early exit.
func subtleEqual(a, b string) bool {
	ha := sha256.Sum256([]byte(a))
	hb := sha256.Sum256([]byte(b))
	return hex.EncodeToString(ha[:]) == hex.EncodeToString(hb[:])
}

// httpError carries an explicit status code.
type httpError struct {
	Status int
	Msg    string
}

func (e *httpError) Error() string { return e.Msg }

func badRequest(format string, args ...any) error {
	return &httpError{Status: http.StatusBadRequest, Msg: fmt.Sprintf(format, args...)}
}

func conflict(format string, args ...any) error {
	return &httpError{Status: http.StatusConflict, Msg: fmt.Sprintf(format, args...)}
}

// handleHealthz reports panel liveness.
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "role": "panel"})
}

// Summary is the aggregate view the dashboard renders.
type Summary struct {
	Nodes      []store.Node    `json:"nodes"`
	Services   []store.Service `json:"services"`
	Routes     []store.Route   `json:"routes"`
	Settings   store.Settings  `json:"settings"`
	ConfigHash string          `json:"config_hash"`
	Rendered   string          `json:"rendered_at"`
	Warnings   []string        `json:"warnings"`
}

// handleSummary returns the whole desired state in one round trip.
func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request) error {
	if r.Method != http.MethodGet {
		return &httpError{Status: http.StatusMethodNotAllowed, Msg: "method not allowed"}
	}
	ctx := r.Context()

	nodes, err := s.store.Nodes(ctx)
	if err != nil {
		return err
	}
	services, err := s.store.Services(ctx)
	if err != nil {
		return err
	}
	routes, err := s.store.Routes(ctx)
	if err != nil {
		return err
	}
	settings, err := s.store.Settings(ctx)
	if err != nil {
		return err
	}

	// A nil slice marshals to null, which forces every consumer to handle two
	// shapes for an empty list. Normalise to an empty slice instead.
	if nodes == nil {
		nodes = []store.Node{}
	}
	if services == nil {
		services = []store.Service{}
	}
	if routes == nil {
		routes = []store.Route{}
	}
	out := Summary{
		Nodes:    nodes,
		Services: services,
		Routes:   routes,
		Settings: settings,
	}
	if len(nodes) > 0 && len(services) > 0 && len(routes) > 0 {
		if cfg, err := Render(store.RoleEntry, "", nodes, services, routes, settings); err == nil {
			out.ConfigHash, _ = ConfigHash(cfg)
		} else {
			out.Warnings = append(out.Warnings, "entry configuration is not renderable: "+err.Error())
		}
	}
	for _, n := range nodes {
		if n.Role != store.RoleRelay || !n.Enabled {
			continue
		}
		if _, err := Render(store.RoleRelay, n.Name, nodes, services, routes, settings); err != nil {
			out.Warnings = append(out.Warnings, fmt.Sprintf("node %s: %v", n.Name, err))
		}
	}
	out.Rendered = time.Now().UTC().Format(time.RFC3339)
	writeJSON(w, http.StatusOK, out)
	return nil
}

// handleAgentConfig returns the configuration an agent should run.
//
// The agent identifies itself by name; the panel renders the document for that
// role and node so a relay only receives the ports it actually needs.
func (s *Server) handleAgentConfig(w http.ResponseWriter, r *http.Request) error {
	if r.Method != http.MethodGet {
		return &httpError{Status: http.StatusMethodNotAllowed, Msg: "method not allowed"}
	}
	name := strings.TrimSpace(r.URL.Query().Get("node"))
	if name == "" {
		return badRequest("node query parameter is required")
	}
	ctx := r.Context()

	node, err := s.store.NodeByName(ctx, name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return &httpError{Status: http.StatusNotFound, Msg: fmt.Sprintf("unknown node %q", name)}
		}
		return err
	}
	if !node.Enabled {
		return &httpError{Status: http.StatusConflict, Msg: fmt.Sprintf("node %q is disabled", name)}
	}

	nodes, err := s.store.Nodes(ctx)
	if err != nil {
		return err
	}
	services, err := s.store.Services(ctx)
	if err != nil {
		return err
	}
	routes, err := s.store.Routes(ctx)
	if err != nil {
		return err
	}
	settings, err := s.store.Settings(ctx)
	if err != nil {
		return err
	}

	cfg, err := Render(node.Role, node.Name, nodes, services, routes, settings)
	if err != nil {
		return &httpError{Status: http.StatusConflict, Msg: err.Error()}
	}
	hash, err := ConfigHash(cfg)
	if err != nil {
		return err
	}

	// The listen address is node-local, so the panel only sends one when an
	// operator pinned it. Otherwise the agent keeps its own -listen value.

	writeJSON(w, http.StatusOK, map[string]any{
		"config": cfg,
		"hash":   hash,
		"role":   node.Role,
		"node":   node.Name,
	})
	return nil
}

// heartbeatRequest is the status an agent reports.
type heartbeatRequest struct {
	NodeName    string `json:"node_name"`
	OK          bool   `json:"ok"`
	Error       string `json:"error"`
	ActiveConns int64  `json:"active_conns"`
	ConfigHash  string `json:"config_hash"`
	Version     int64  `json:"version"`
}

// handleAgentHeartbeat records agent liveness.
func (s *Server) handleAgentHeartbeat(w http.ResponseWriter, r *http.Request) error {
	if r.Method != http.MethodPost {
		return &httpError{Status: http.StatusMethodNotAllowed, Msg: "method not allowed"}
	}
	var req heartbeatRequest
	if err := decodeJSON(r, &req); err != nil {
		return badRequest("%v", err)
	}
	if strings.TrimSpace(req.NodeName) == "" {
		return badRequest("node_name is required")
	}
	if err := s.store.UpdateNodeStatus(r.Context(), req.NodeName, req.OK, req.Error, req.ActiveConns, req.ConfigHash); err != nil {
		return err
	}
	settings, err := s.store.Settings(r.Context())
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":              true,
		"desired_version": settings.Version,
	})
	return nil
}

// handleRender returns a rendered configuration for inspection.
func (s *Server) handleRender(w http.ResponseWriter, r *http.Request) error {
	if r.Method != http.MethodGet {
		return &httpError{Status: http.StatusMethodNotAllowed, Msg: "method not allowed"}
	}
	ctx := r.Context()

	role := store.Role(strings.TrimSpace(r.URL.Query().Get("role")))
	name := strings.TrimSpace(r.URL.Query().Get("node"))
	if role == "" {
		role = store.RoleEntry
	}
	if role == store.RoleRelay && name == "" {
		return badRequest("node is required when rendering a relay configuration")
	}

	nodes, err := s.store.Nodes(ctx)
	if err != nil {
		return err
	}
	services, err := s.store.Services(ctx)
	if err != nil {
		return err
	}
	routes, err := s.store.Routes(ctx)
	if err != nil {
		return err
	}
	settings, err := s.store.Settings(ctx)
	if err != nil {
		return err
	}

	cfg, err := Render(role, name, nodes, services, routes, settings)
	if err != nil {
		return &httpError{Status: http.StatusConflict, Msg: err.Error()}
	}
	hash, _ := ConfigHash(cfg)

	// The config is returned as JSON so the UI can display it without a YAML
	// dependency in the browser.
	writeJSON(w, http.StatusOK, map[string]any{"config": cfg, "hash": hash})
	return nil
}

// checkPortsFree is a defensive helper used by node validation.
func checkPortsFree(_ context.Context, ports []int) error {
	seen := map[int]bool{}
	for _, p := range ports {
		if p <= 0 || p > 65535 {
			return fmt.Errorf("port %d is out of range", p)
		}
		if seen[p] {
			return fmt.Errorf("port %d is listed twice", p)
		}
		seen[p] = true
	}
	return nil
}
