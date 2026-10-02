package forward

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/uio-o/smart-gateway/internal/config"
)

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}

func TestJoinUpstreamPreservesQuery(t *testing.T) {
	svc := config.Service{Upstream: "http://127.0.0.1:8084"}
	got, err := joinUpstream(svc.Upstream, mustURL(t, "/v1/messages?beta=true"), svc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Path != "/v1/messages" {
		t.Fatalf("path = %q", got.Path)
	}
	if got.RawQuery != "beta=true" {
		t.Fatalf("query = %q", got.RawQuery)
	}
}

func TestJoinUpstreamAppliesBasePath(t *testing.T) {
	svc := config.Service{Upstream: "http://127.0.0.1:8084/api"}
	got, err := joinUpstream(svc.Upstream, mustURL(t, "/v1/messages"), svc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Path != "/api/v1/messages" {
		t.Fatalf("path = %q, want /api/v1/messages", got.Path)
	}
}

func TestJoinUpstreamStripPrefix(t *testing.T) {
	svc := config.Service{
		Upstream:    "http://127.0.0.1:8084",
		PathPrefix:  "/v1/",
		StripPrefix: true,
	}
	got, err := joinUpstream(svc.Upstream, mustURL(t, "/v1/messages"), svc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Path != "/messages" {
		t.Fatalf("path = %q, want /messages", got.Path)
	}
}

func TestJoinUpstreamRejectsBadBase(t *testing.T) {
	svc := config.Service{Upstream: "http://"}
	if _, err := joinUpstream(svc.Upstream, mustURL(t, "/x"), svc); err == nil {
		t.Fatal("expected error for upstream without host")
	}
}

func TestCheckRequestMethodWhitelist(t *testing.T) {
	svc := config.Service{AllowMethods: []string{"POST"}}
	req := httptest.NewRequest(http.MethodGet, "/v1/messages", nil)
	if err := CheckRequest(req, svc); err == nil {
		t.Fatal("GET should be rejected when only POST is allowed")
	}
	req = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	if err := CheckRequest(req, svc); err != nil {
		t.Fatalf("POST should be allowed: %v", err)
	}
}

func TestCheckRequestPathWhitelist(t *testing.T) {
	svc := config.Service{AllowPaths: []string{"/v1/messages", "/v1/*"}}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	if err := CheckRequest(req, svc); err != nil {
		t.Fatalf("exact path should be allowed: %v", err)
	}
	req = httptest.NewRequest(http.MethodPost, "/v1/models", nil)
	if err := CheckRequest(req, svc); err != nil {
		t.Fatalf("prefix path should be allowed: %v", err)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/admin/users", nil)
	if err := CheckRequest(req, svc); err == nil {
		t.Fatal("path outside whitelist must be rejected")
	}
}

func TestCheckRequestBodyLimit(t *testing.T) {
	svc := config.Service{MaxBodyBytes: 10}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(strings.Repeat("x", 50)))
	req.ContentLength = 50
	if err := CheckRequest(req, svc); err == nil {
		t.Fatal("oversized body must be rejected")
	}
}

func TestIsWebSocketUpgrade(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade, keep-alive")
	if !IsWebSocketUpgrade(req) {
		t.Fatal("expected websocket upgrade to be detected")
	}

	plain := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	if IsWebSocketUpgrade(plain) {
		t.Fatal("plain request must not be treated as upgrade")
	}
}

func TestServeHTTPForwardsHeaderAndBody(t *testing.T) {
	var gotAuth, gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		w.Header().Set("X-Upstream", "yes")
		_, _ = w.Write([]byte("hello"))
	}))
	defer upstream.Close()

	f := NewHTTPForwarder()
	defer f.Close()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("body"))
	req.Header.Set("Authorization", "Bearer client-key")

	err := f.ServeHTTP(rec, req, Plan{
		RouteName: "r1",
		Upstream:  upstream.URL,
		Service:   config.Service{SupportsSSE: true},
	})
	if err != nil {
		t.Fatalf("forward failed: %v", err)
	}

	if gotAuth != "Bearer client-key" {
		t.Fatalf("authorization header not forwarded, got %q", gotAuth)
	}
	if gotPath != "/v1/messages" {
		t.Fatalf("upstream path = %q", gotPath)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec.Body.String() != "hello" {
		t.Fatalf("body = %q", rec.Body.String())
	}
	if rec.Header().Get("X-Upstream") != "yes" {
		t.Fatal("upstream response header not relayed")
	}
}

func TestServeHTTPStreamsWithoutBuffering(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("upstream has no flusher")
			return
		}
		for _, chunk := range []string{"data: one\n\n", "data: two\n\n"} {
			_, _ = w.Write([]byte(chunk))
			fl.Flush()
			time.Sleep(10 * time.Millisecond)
		}
	}))
	defer upstream.Close()

	f := NewHTTPForwarder()
	defer f.Close()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	if err := f.ServeHTTP(rec, req, Plan{
		RouteName: "r1",
		Upstream:  upstream.URL,
		Service:   config.Service{SupportsSSE: true},
	}); err != nil {
		t.Fatalf("forward failed: %v", err)
	}
	if !strings.Contains(rec.Body.String(), "data: one") {
		t.Fatalf("stream body missing first chunk: %q", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "data: two") {
		t.Fatalf("stream body missing second chunk: %q", rec.Body.String())
	}
}

func TestFlushIntervalDefaultsToStreamingWhenSSE(t *testing.T) {
	if got := flushInterval(config.Service{SupportsSSE: true}); got != -1 {
		t.Fatalf("flush interval = %v, want -1 for SSE", got)
	}
	if got := flushInterval(config.Service{}); got <= 0 {
		t.Fatalf("flush interval = %v, want positive for non-SSE", got)
	}
}

func TestReadAllLimited(t *testing.T) {
	data, err := ReadAllLimited(strings.NewReader("abcdef"), 10)
	if err != nil || string(data) != "abcdef" {
		t.Fatalf("unexpected result %q err %v", data, err)
	}
	if _, err := ReadAllLimited(strings.NewReader("abcdef"), 3); err == nil {
		t.Fatal("expected error when limit exceeded")
	}
}
