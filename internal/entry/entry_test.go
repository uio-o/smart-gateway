package entry

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/uio-o/smart-gateway/internal/config"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestServer builds an entry server pointing at the given upstreams.
func newTestServer(t *testing.T, cfg *config.Config) *Server {
	t.Helper()
	s, err := New(Options{Config: cfg, Logger: testLogger()})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

func entryConfig(upstream string) *config.Config {
	return &config.Config{
		Role:   config.RoleEntry,
		Listen: "127.0.0.1:0",
		Services: []config.Service{
			{
				Name:        "svc",
				Upstream:    upstream,
				PathPrefix:  "/v1/",
				AllowPaths:  []string{"/v1/messages", "/v1/*"},
				SupportsSSE: true,
			},
		},
		Routes: []config.Route{
			{Name: "direct", Service: "svc"},
		},
		Routing: config.Routing{Mode: "static", Static: "direct"},
		Sticky:  config.Sticky{Enabled: true, HoldMinutes: 60, FailThreshold: 3},
	}
}

func TestForwardsRequestAndRelaysHeaders(t *testing.T) {
	var gotPath, gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("X-Upstream", "yes")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	s := newTestServer(t, entryConfig(upstream.URL))

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"x"}`))
	req.Header.Set("Authorization", "Bearer client-key")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if gotPath != "/v1/messages" {
		t.Fatalf("upstream path = %q", gotPath)
	}
	if gotAuth != "Bearer client-key" {
		t.Fatalf("authorization not forwarded, got %q", gotAuth)
	}
	if rec.Header().Get("X-Upstream") != "yes" {
		t.Fatal("upstream header not relayed to client")
	}
}

func TestRejectsMethodOutsideWhitelist(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be called for a rejected method")
	}))
	defer upstream.Close()

	cfg := entryConfig(upstream.URL)
	cfg.Services[0].AllowMethods = []string{"POST"}
	s := newTestServer(t, cfg)

	req := httptest.NewRequest(http.MethodDelete, "/v1/messages", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestRejectsPathOutsideWhitelist(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be called for a rejected path")
	}))
	defer upstream.Close()

	cfg := entryConfig(upstream.URL)
	// Only a single path is allowed, so anything else must be refused. A lone
	// "*" matches inside one segment and must not open the whole tree.
	cfg.Services[0].AllowPaths = []string{"/v1/messages"}
	s := newTestServer(t, cfg)

	// Paths under the served prefix but outside the whitelist are forbidden.
	for _, path := range []string{"/v1/internal/debug", "/v1/models"} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("path %s status = %d, want 403", path, rec.Code)
		}
	}

	// A path outside every served prefix is unknown, so it is not found.
	req := httptest.NewRequest(http.MethodPost, "/other", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("path /other status = %d, want 404", rec.Code)
	}
}

func TestSingleStarDoesNotCrossSegments(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	cfg := entryConfig(upstream.URL)
	cfg.Services[0].AllowPaths = []string{"/v1/*"}
	s := newTestServer(t, cfg)

	// One segment below the prefix is allowed.
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a single segment below the prefix", rec.Code)
	}

	// A deeper path is not covered by a single star.
	req = httptest.NewRequest(http.MethodPost, "/v1/admin/users", nil)
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a nested path", rec.Code)
	}
}

func TestUnknownPrefixReturnsNotFound(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	cfg := entryConfig(upstream.URL)
	cfg.Services[0].PathPrefix = "/v1/"

	s := newTestServer(t, cfg)
	req := httptest.NewRequest(http.MethodGet, "/other/thing", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestOversizeBodyRejected(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()

	cfg := entryConfig(upstream.URL)
	cfg.Services[0].MaxBodyBytes = 16
	s := newTestServer(t, cfg)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(strings.Repeat("a", 64)))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
}

func TestHealthEndpointIsServedLocally(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("health must not be forwarded upstream")
	}))
	defer upstream.Close()

	s := newTestServer(t, entryConfig(upstream.URL))

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("health body is not JSON: %v", err)
	}
	if payload["status"] != "ok" {
		t.Fatalf("status = %v", payload["status"])
	}
}

func TestStatsEndpointReportsRoutes(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	s := newTestServer(t, entryConfig(upstream.URL))

	// Generate one recorded sample first.
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	s.ServeHTTP(httptest.NewRecorder(), req)

	statReq := httptest.NewRequest(http.MethodGet, "/_gateway/stats", nil)
	statRec := httptest.NewRecorder()
	s.ServeHTTP(statRec, statReq)

	if statRec.Code != http.StatusOK {
		t.Fatalf("status = %d", statRec.Code)
	}
	var payload struct {
		Routes   []map[string]any `json:"routes"`
		Sessions int              `json:"sessions"`
	}
	if err := json.Unmarshal(statRec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("stats body is not JSON: %v", err)
	}
	if len(payload.Routes) == 0 {
		t.Fatal("expected at least one route in stats")
	}
}

func TestStreamingResponseIsNotBuffered(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for _, part := range []string{"data: a\n\n", "data: b\n\n"} {
			_, _ = w.Write([]byte(part))
			fl.Flush()
			time.Sleep(5 * time.Millisecond)
		}
	}))
	defer upstream.Close()

	s := newTestServer(t, entryConfig(upstream.URL))
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "data: a") || !strings.Contains(body, "data: b") {
		t.Fatalf("streaming body incomplete: %q", body)
	}
}

func TestSessionAffinityKeepsSameRoute(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	s := newTestServer(t, entryConfig(upstream.URL))

	body := `{"model":"x","messages":[{"role":"user","content":"hello"}]}`
	send := func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
		req.RemoteAddr = "1.2.3.4:1234"
		req.Header.Set("User-Agent", "cli/1.0")
		s.ServeHTTP(httptest.NewRecorder(), req)
	}

	send()
	if s.Sessions().Size() != 1 {
		t.Fatalf("session table size = %d, want 1", s.Sessions().Size())
	}
	entries := s.Sessions().Entries()
	if entries[0].Route != "direct" {
		t.Fatalf("assigned route = %q, want direct", entries[0].Route)
	}

	send()
	if s.Sessions().Size() != 1 {
		t.Fatal("a repeat request must reuse the same session entry")
	}
}

// TestDisabledAffinityStaysDisabledWhenHoldIsUnset guards a regression where a
// config asking for affinity to be OFF got it switched back ON.
//
// The server used to replace the session table with sticky.DefaultConfig()
// whenever hold_minutes was unset, and that default has Enabled: true. So
// {"enabled": false, "hold_minutes": 0} produced a live session table, while
// the very same document applied through Reload() produced a disabled one —
// the configuration an operator sent was not the configuration in effect after
// a restart, and only for the first load.
func TestDisabledAffinityStaysDisabledWhenHoldIsUnset(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	cfg := entryConfig(upstream.URL)
	cfg.Sticky = config.Sticky{Enabled: false, HoldMinutes: 0, FailThreshold: 0}

	s := newTestServer(t, cfg)
	if got := s.Sessions().Size(); got != 0 {
		t.Fatalf("session table started with %d entries, want 0", got)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"x"}`))
	req.RemoteAddr = "1.2.3.4:1234"
	req.Header.Set("User-Agent", "cli/1.0")
	s.ServeHTTP(httptest.NewRecorder(), req)

	if got := s.Sessions().Size(); got != 0 {
		t.Fatalf("affinity recorded %d sessions although it was disabled", got)
	}

	// Applying the very same document through Reload must agree with the start.
	if err := s.Reload(cfg); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := s.Sessions().Size(); got != 0 {
		t.Fatalf("sessions after reload = %d, want 0", got)
	}
}

func TestUpstreamFailureRecordedInStats(t *testing.T) {
	// Point at a closed port so the forward fails.
	cfg := entryConfig("http://127.0.0.1:1")
	s := newTestServer(t, cfg)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	st := s.Tracker().Stats("direct")
	if st.Samples == 0 {
		t.Fatal("failure should be recorded as a sample")
	}
	if st.Healthy {
		t.Fatal("route should be unhealthy after a transport failure")
	}
}

func TestRateLimitRejectsFlood(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	cfg := entryConfig(upstream.URL)
	cfg.Limits.RatePerMinute = 2
	s := newTestServer(t, cfg)

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
		req.RemoteAddr = "9.9.9.9:1111"
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d status = %d, want 200", i, rec.Code)
		}
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	req.RemoteAddr = "9.9.9.9:1111"
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
}

func TestReloadSwapsConfig(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	s := newTestServer(t, entryConfig(upstream.URL))

	next := entryConfig(upstream.URL)
	next.Version = 7
	if err := s.Reload(next); err != nil {
		t.Fatalf("reload failed: %v", err)
	}
	if s.Config().Version != 7 {
		t.Fatalf("version = %d, want 7", s.Config().Version)
	}
}

func TestReloadRejectsRelayConfig(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()

	s := newTestServer(t, entryConfig(upstream.URL))
	relay := &config.Config{
		Role:       config.RoleRelay,
		RelayPorts: []config.RelayMapping{{Port: 3001, Target: "127.0.0.1:8084"}},
	}
	if err := s.Reload(relay); err == nil {
		t.Fatal("reloading a relay config into an entry server must fail")
	}
}

func TestLimiterWindowBehaviour(t *testing.T) {
	l := newLimiter(2)
	if !l.allow("a") || !l.allow("a") {
		t.Fatal("first two requests should pass")
	}
	if l.allow("a") {
		t.Fatal("third request in the same window should be rejected")
	}
	if !l.allow("b") {
		t.Fatal("a different client must not be affected")
	}
}
