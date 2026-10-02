package probe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/uio-o/smart-gateway/internal/config"
	"github.com/uio-o/smart-gateway/internal/route"
)

func cfgFor(upstream string, probeCfg config.Probe) *config.Config {
	return &config.Config{
		Role:   config.RoleEntry,
		Listen: "127.0.0.1:8080",
		Services: []config.Service{
			{Name: "svc", Upstream: upstream, HealthPath: "/health"},
		},
		Routes: []config.Route{
			{Name: "r1", Service: "svc"},
		},
		Routing: config.Routing{Mode: "static", Static: "r1"},
		Probe:   probeCfg,
	}
}

func TestMeasureHealthyUpstream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			t.Errorf("probe path = %s, want /health", r.URL.Path)
		}
		if r.Header.Get("X-Smart-Gateway-Probe") != "1" {
			t.Error("probe marker header missing")
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()

	cfg := cfgFor(srv.URL, config.Probe{Enabled: true, Path: "/health"})
	p := New(3 * time.Second)

	res := p.Measure(context.Background(), cfg, cfg.Routes[0])
	if !res.OK {
		t.Fatalf("expected healthy probe, got error %q", res.Error)
	}
	if res.Status != http.StatusOK {
		t.Fatalf("status = %d", res.Status)
	}
	if res.Latency <= 0 {
		t.Fatal("latency should be measured")
	}
}

func TestMeasureServerErrorIsUnhealthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfg := cfgFor(srv.URL, config.Probe{Enabled: true})
	p := New(3 * time.Second)

	res := p.Measure(context.Background(), cfg, cfg.Routes[0])
	if res.OK {
		t.Fatal("5xx response must not be reported healthy")
	}
	if res.Error == "" {
		t.Fatal("expected an error description")
	}
}

func TestMeasureClientErrorCountsAsReachable(t *testing.T) {
	// A 404 still proves the network path and the upstream process are alive.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	cfg := cfgFor(srv.URL, config.Probe{Enabled: true})
	p := New(3 * time.Second)

	res := p.Measure(context.Background(), cfg, cfg.Routes[0])
	if !res.OK {
		t.Fatalf("4xx should count as reachable, got %q", res.Error)
	}
}

func TestMeasureUnreachableUpstream(t *testing.T) {
	// Port 1 is reserved and will refuse immediately.
	cfg := cfgFor("http://127.0.0.1:1", config.Probe{Enabled: true})
	p := New(1 * time.Second)

	res := p.Measure(context.Background(), cfg, cfg.Routes[0])
	if res.OK {
		t.Fatal("connection refused must report failure")
	}
	if res.Error == "" {
		t.Fatal("expected an error description")
	}
}

func TestMeasureTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := cfgFor(srv.URL, config.Probe{Enabled: true})
	p := New(200 * time.Millisecond)

	res := p.Measure(context.Background(), cfg, cfg.Routes[0])
	if res.OK {
		t.Fatal("slow upstream must report failure within the timeout")
	}
}

func TestResolveProbeURLUsesFirstHop(t *testing.T) {
	cfg := &config.Config{
		Role: config.RoleEntry,
		Services: []config.Service{
			{Name: "svc", Upstream: "http://10.0.0.9:8084", HealthPath: "/health"},
		},
		Nodes: []config.Node{
			{Name: "tokyo", Address: "1.1.1.1:3001"},
		},
	}
	r := config.Route{
		Name:    "via-tokyo",
		Service: "svc",
		Hops:    []config.Hop{{Node: "tokyo"}},
	}
	got, err := resolveProbeURL(cfg, r, cfg.Services[0])
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "http://1.1.1.1:3001/health" {
		t.Fatalf("probe url = %q, want the first hop", got)
	}
}

func TestResolveProbeURLDirectService(t *testing.T) {
	cfg := &config.Config{
		Role: config.RoleEntry,
		Services: []config.Service{
			{Name: "svc", Upstream: "http://10.0.0.9:8084", HealthPath: "health"},
		},
	}
	r := config.Route{Name: "direct", Service: "svc"}
	got, err := resolveProbeURL(cfg, r, cfg.Services[0])
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "http://10.0.0.9:8084/health" {
		t.Fatalf("probe url = %q", got)
	}
}

func TestRunRecordsSamplesUntilCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := cfgFor(srv.URL, config.Probe{
		Enabled:         true,
		IntervalSeconds: 1,
		TimeoutSeconds:  2,
		Samples:         5,
	})
	tracker := route.NewTracker(route.DefaultOptions())
	p := New(2 * time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.Run(ctx, func() *config.Config { return cfg }, tracker)
		close(done)
	}()

	time.Sleep(1500 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not stop after cancellation")
	}

	st := tracker.Stats("r1")
	if st.Samples == 0 {
		t.Fatal("expected at least one recorded sample")
	}
	if !st.Healthy {
		t.Fatal("route should be healthy after successful probes")
	}
}

func TestOnceUnknownRoute(t *testing.T) {
	cfg := cfgFor("http://127.0.0.1:1", config.Probe{Enabled: true})
	p := New(time.Second)
	if _, ok := p.Once(context.Background(), cfg, "nope"); ok {
		t.Fatal("unknown route should report not found")
	}
}
