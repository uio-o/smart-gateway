// Package entry implements the entry node: it accepts client traffic, resolves
// a route through session affinity and health, and forwards the request.
package entry

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/uio-o/smart-gateway/internal/config"
	"github.com/uio-o/smart-gateway/internal/forward"
	"github.com/uio-o/smart-gateway/internal/route"
	"github.com/uio-o/smart-gateway/internal/sticky"
)

// Server is the entry node HTTP handler.
type Server struct {
	cfg atomic.Pointer[config.Config]

	tracker  *route.Tracker
	selector *route.Selector
	sessions *sticky.Table
	fwd      *forward.HTTPForwarder
	log      *slog.Logger

	audit   *auditWriter
	limiter *limiter

	mu         sync.Mutex
	inflight   int
	httpServer *http.Server
	addr       string

	// fingerprintPeek is how many request body bytes feed affinity.
	fingerprintPeek int
}

// Options configures the entry server.
type Options struct {
	Config *config.Config
	Logger *slog.Logger
}

// New creates an entry server.
func New(opts Options) (*Server, error) {
	if opts.Config == nil {
		return nil, fmt.Errorf("config is required")
	}
	if opts.Config.Role != config.RoleEntry {
		return nil, fmt.Errorf("entry server requires role=entry")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	cfg := opts.Config
	s := &Server{
		tracker: route.NewTracker(route.DefaultOptions()),
		sessions: sticky.New(sticky.Config{
			Enabled:       cfg.Sticky.Enabled,
			Hold:          time.Duration(cfg.Sticky.HoldMinutes) * time.Minute,
			FailThreshold: cfg.Sticky.FailThreshold,
		}),
		fwd:             forward.NewHTTPForwarder(),
		log:             opts.Logger,
		fingerprintPeek: 512,
	}
	if cfg.Sticky.HoldMinutes <= 0 {
		s.sessions = sticky.New(sticky.DefaultConfig())
	}
	s.selector = route.NewSelector(s.tracker)
	s.cfg.Store(cfg)

	if cfg.Logging.AuditPath != "" {
		w, err := newAuditWriter(cfg.Logging.AuditPath)
		if err != nil {
			return nil, err
		}
		s.audit = w
	}
	if cfg.Limits.RatePerMinute > 0 {
		s.limiter = newLimiter(cfg.Limits.RatePerMinute)
	}
	return s, nil
}

// Config returns the current configuration.
func (s *Server) Config() *config.Config { return s.cfg.Load() }

// Tracker exposes health statistics.
func (s *Server) Tracker() *route.Tracker { return s.tracker }

// Sessions exposes the affinity table.
func (s *Server) Sessions() *sticky.Table { return s.sessions }

// Close releases resources held by the server.
func (s *Server) Close() {
	s.mu.Lock()
	hs := s.httpServer
	s.httpServer = nil
	s.mu.Unlock()

	if hs != nil {
		// Drain in-flight requests with a bounded deadline so a streaming
		// response cannot keep the process alive forever.
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		if err := hs.Shutdown(ctx); err != nil {
			s.log.Warn("entry shutdown incomplete", "error", err)
			_ = hs.Close()
		}
		cancel()
	}

	s.fwd.Close()
	if s.audit != nil {
		_ = s.audit.Close()
	}
}

// Reload swaps in a new configuration.
func (s *Server) Reload(cfg *config.Config) error {
	if cfg == nil {
		return fmt.Errorf("config is nil")
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	if cfg.Role != config.RoleEntry {
		return fmt.Errorf("cannot reload a %s config into an entry server", cfg.Role)
	}
	s.cfg.Store(cfg)
	s.sessions = sticky.New(sticky.Config{
		Enabled:       cfg.Sticky.Enabled,
		Hold:          time.Duration(cfg.Sticky.HoldMinutes) * time.Minute,
		FailThreshold: cfg.Sticky.FailThreshold,
	})
	s.log.Info("configuration reloaded", "version", cfg.Version)
	return nil
}

// ServeHTTP handles one client request.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg.Load()

	// Operational endpoints are served locally and never forwarded.
	switch r.URL.Path {
	case "/health", "/_gateway/health":
		s.handleHealth(w)
		return
	case "/_gateway/routes", "/_gateway/stats":
		s.handleStats(w)
		return
	}

	if s.limiter != nil {
		if !s.limiter.allow(clientIP(r)) {
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
	}

	if cfg.Limits.MaxConcurrent > 0 {
		s.mu.Lock()
		if s.inflight >= cfg.Limits.MaxConcurrent {
			s.mu.Unlock()
			http.Error(w, "too many concurrent requests", http.StatusServiceUnavailable)
			return
		}
		s.inflight++
		s.mu.Unlock()
		defer func() {
			s.mu.Lock()
			s.inflight--
			s.mu.Unlock()
		}()
	}

	svc, ok := s.matchService(cfg, r)
	if !ok {
		http.Error(w, "no service configured for this request", http.StatusNotFound)
		return
	}
	if err := forward.CheckRequest(r, svc); err != nil {
		status := http.StatusForbidden
		var pe *forward.PolicyError
		if errors.As(err, &pe) {
			status = pe.Status
		}
		http.Error(w, err.Error(), status)
		return
	}

	// Read the body up to the service limit so policy checks, affinity and
	// forwarding all operate on the same bytes exactly once.
	body, err := s.peekBody(r, svc)
	if err != nil {
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		return
	}

	ip := clientIP(r)
	key := sticky.SessionKey(r, body, ip)
	assigned := s.sessions.Lookup(key)

	decision := s.selector.Select(cfg, assigned, nil)
	if decision.RouteName == "" {
		http.Error(w, "no route available for this service", http.StatusServiceUnavailable)
		return
	}

	rt, ok := cfg.RouteByName(decision.RouteName)
	if !ok {
		http.Error(w, "selected route disappeared", http.StatusServiceUnavailable)
		return
	}

	upstream, err := s.resolveUpstream(cfg, rt, svc)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	if key != "" {
		s.sessions.Assign(key, decision.RouteName)
	}

	start := time.Now()
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	ferr := s.fwd.ServeHTTP(rec, r, forward.Plan{
		RouteName: decision.RouteName,
		Upstream:  upstream,
		Service:   svc,
	})
	elapsed := time.Since(start)

	ok = ferr == nil && rec.status < 500
	s.tracker.Record(decision.RouteName, route.Sample{
		OK:      ok,
		Latency: elapsed,
		At:      time.Now(),
	})
	if !ok {
		msg := "upstream failure"
		if ferr != nil {
			msg = ferr.Error()
		} else {
			msg = fmt.Sprintf("status %d", rec.status)
		}
		s.tracker.SetLastError(decision.RouteName, msg)
		if key != "" {
			s.sessions.NoteFailure(key)
		}
	} else if key != "" {
		s.sessions.NoteSuccess(key)
	}

	s.writeAudit(auditRecord{
		Time:      time.Now().UTC().Format(time.RFC3339Nano),
		ClientIP:  ip,
		Method:    r.Method,
		Path:      r.URL.Path,
		Service:   svc.Name,
		Route:     decision.RouteName,
		Reason:    decision.Reason,
		Failover:  decision.FailoverFrom,
		Session:   key,
		Status:    rec.status,
		DurationM: elapsed.Milliseconds(),
		BytesOut:  rec.written,
	})
}

// matchService picks the service whose prefix matches the request path.
func (s *Server) matchService(cfg *config.Config, r *http.Request) (config.Service, bool) {
	var best config.Service
	found := false
	for _, svc := range cfg.Services {
		prefix := strings.TrimSuffix(svc.PathPrefix, "/")
		if prefix == "" {
			// A service with no prefix matches anything, as a last resort.
			if !found {
				best, found = svc, true
			}
			continue
		}
		if r.URL.Path == prefix || strings.HasPrefix(r.URL.Path, prefix+"/") {
			if !found || len(prefix) > len(strings.TrimSuffix(best.PathPrefix, "/")) {
				best, found = svc, true
			}
		}
	}
	return best, found
}

// resolveUpstream resolves the final target URL for a route.
//
// A route with no hops forwards straight to the service. A route with hops
// enters through the first hop, which is expected to relay onwards.
func (s *Server) resolveUpstream(cfg *config.Config, rt config.Route, svc config.Service) (string, error) {
	if len(rt.Hops) == 0 {
		return svc.Upstream, nil
	}
	first := rt.Hops[0]
	if first.URL != "" {
		return first.URL, nil
	}
	node, ok := cfg.NodeByName(first.Node)
	if !ok || node.Address == "" {
		return "", fmt.Errorf("route %q first hop %q has no address", rt.Name, first.Node)
	}
	return "http://" + node.Address, nil
}

// peekBody reads the request body up to the service limit and restores it so
// the forwarded request carries exactly the bytes that were checked.
//
// It returns a bounded prefix for affinity fingerprinting.
func (s *Server) peekBody(r *http.Request, svc config.Service) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	data, err := forward.ReadAllLimited(r.Body, svc.BodyLimit())
	if err != nil {
		return nil, err
	}
	_ = r.Body.Close()

	prefix := data
	if len(prefix) > s.fingerprintPeek {
		prefix = prefix[:s.fingerprintPeek]
	}
	r.Body = io.NopCloser(bytes.NewReader(data))
	r.ContentLength = int64(len(data))
	return prefix, nil
}

// handleHealth reports local liveness without touching upstreams.
func (s *Server) handleHealth(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":    "ok",
		"role":      "entry",
		"version":   s.cfg.Load().Version,
		"sessions":  s.sessions.Size(),
		"time_unix": time.Now().Unix(),
	})
}

// handleStats reports per-route health for the panel and for operators.
func (s *Server) handleStats(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"routes":   s.tracker.AllStats(),
		"version":  s.cfg.Load().Version,
		"sessions": s.sessions.Size(),
	})
}

// statusRecorder captures the response status and byte count for auditing
// while preserving streaming and protocol upgrade behaviour.
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written int64
	wrote   bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wrote {
		r.status = code
		r.wrote = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wrote {
		r.wrote = true
	}
	n, err := r.ResponseWriter.Write(b)
	r.written += int64(n)
	return n, err
}

// Flush forwards flushes so streaming responses reach the client immediately.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack supports protocol upgrades such as WebSocket.
func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("response writer does not support hijacking")
	}
	return h.Hijack()
}

// clientIP extracts the caller address, honouring nothing by default.
//
// X-Forwarded-For is deliberately ignored: the entry node is expected to sit
// behind an operator controlled proxy, and trusting a client supplied header
// would let a caller spoof affinity keys.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// auditRecord is one JSON line in the audit log.
type auditRecord struct {
	Time      string `json:"time"`
	ClientIP  string `json:"client_ip"`
	Method    string `json:"method"`
	Path      string `json:"path"`
	Service   string `json:"service"`
	Route     string `json:"route"`
	Reason    string `json:"reason"`
	Failover  string `json:"failover_from,omitempty"`
	Session   string `json:"session,omitempty"`
	Status    int    `json:"status"`
	DurationM int64  `json:"duration_ms"`
	BytesOut  int64  `json:"bytes_out"`
}

// auditWriter appends audit records to a file without blocking the request.
type auditWriter struct {
	mu   sync.Mutex
	file *os.File
	enc  *json.Encoder
}

func newAuditWriter(path string) (*auditWriter, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open audit log %s: %w", path, err)
	}
	return &auditWriter{file: f, enc: json.NewEncoder(f)}, nil
}

// Write appends one record.
func (a *auditWriter) Write(rec auditRecord) {
	a.mu.Lock()
	defer a.mu.Unlock()
	_ = a.enc.Encode(rec)
}

// Close closes the underlying file.
func (a *auditWriter) Close() error { return a.file.Close() }

func (s *Server) writeAudit(rec auditRecord) {
	if s.audit == nil {
		return
	}
	s.audit.Write(rec)
}

// Limiter is a simple fixed-window per-client rate limiter.

// RunProbes starts background probing for the entry node.
func (s *Server) RunProbes(ctx context.Context, p interface {
	Run(context.Context, func() *config.Config, *route.Tracker)
}) {
	go p.Run(ctx, s.Config, s.tracker)
}

// Attach records the HTTP server that serves this handler so Close can shut
// it down.
func (s *Server) Attach(hs *http.Server) {
	s.mu.Lock()
	s.httpServer = hs
	s.mu.Unlock()
}

// ActiveConnections reports how many requests are currently being served.
func (s *Server) ActiveConnections() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return int64(s.inflight)
}

// SetAddr records the address the server is actually listening on.
func (s *Server) SetAddr(addr string) {
	s.mu.Lock()
	s.addr = addr
	s.mu.Unlock()
}

// Addr reports the address the server was started on. It can differ from the
// current configuration after a reload that did not rebind the socket.
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}
