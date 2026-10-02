package route

import (
	"testing"
	"time"

	"github.com/uio-o/smart-gateway/internal/config"
)

func baseConfig() *config.Config {
	return &config.Config{
		Role:   config.RoleEntry,
		Listen: "127.0.0.1:8080",
		Services: []config.Service{
			{Name: "svc", Upstream: "http://127.0.0.1:8084"},
		},
		Nodes: []config.Node{
			{Name: "tokyo", Address: "1.1.1.1:3001"},
			{Name: "malibu", Address: "2.2.2.2:3001"},
		},
		Routes: []config.Route{
			{Name: "tokyo", Service: "svc", Hops: []config.Hop{{Node: "tokyo"}}},
			{Name: "malibu", Service: "svc", Hops: []config.Hop{{Node: "malibu"}}},
		},
		Routing: config.Routing{Mode: "primary_backup", Primary: "tokyo", Backup: "malibu"},
		Sticky:  config.Sticky{Enabled: true},
	}
}

func recordOK(t *Tracker, name string, ms int) {
	t.Record(name, Sample{OK: true, Latency: time.Duration(ms) * time.Millisecond, At: time.Now()})
}

func recordFail(t *Tracker, name string) {
	t.Record(name, Sample{OK: false, Latency: 500 * time.Millisecond, At: time.Now()})
}

func TestStatsEmptyIsHealthy(t *testing.T) {
	tr := NewTracker(DefaultOptions())
	st := tr.Stats("unknown")
	if !st.Healthy {
		t.Fatal("a route with no samples should be usable on first deploy")
	}
	if st.Samples != 0 {
		t.Fatalf("samples = %d, want 0", st.Samples)
	}
}

func TestConsecutiveFailuresMarkUnhealthy(t *testing.T) {
	tr := NewTracker(DefaultOptions())
	recordOK(tr, "tokyo", 100)
	for i := 0; i < 3; i++ {
		recordFail(tr, "tokyo")
	}
	st := tr.Stats("tokyo")
	if st.Healthy {
		t.Fatal("route should be unhealthy after reaching failure threshold")
	}
	if st.ConsecFails != 3 {
		t.Fatalf("consecutive failures = %d, want 3", st.ConsecFails)
	}
}

func TestSuccessRateBelowThresholdMarksUnhealthy(t *testing.T) {
	tr := NewTracker(DefaultOptions())
	// 1 success, 4 failures = 20% success over a window of 20.
	recordOK(tr, "r", 100)
	for i := 0; i < 4; i++ {
		recordFail(tr, "r")
	}
	st := tr.Stats("r")
	if st.Healthy {
		t.Fatal("route should be unhealthy when success rate is below minimum")
	}
}

func TestManualDisableOverridesHealthyStats(t *testing.T) {
	tr := NewTracker(DefaultOptions())
	recordOK(tr, "r", 50)
	tr.SetDisabled("r", true)
	if tr.Stats("r").Healthy {
		t.Fatal("manually disabled route must not be healthy")
	}
	tr.SetDisabled("r", false)
	if !tr.Stats("r").Healthy {
		t.Fatal("re-enabled route should be healthy again")
	}
}

func TestScorePrefersFastReliableRoute(t *testing.T) {
	tr := NewTracker(DefaultOptions())
	for i := 0; i < 5; i++ {
		recordOK(tr, "fast", 40)
		recordOK(tr, "slow", 600)
	}
	fast := tr.Stats("fast")
	slow := tr.Stats("slow")
	if fast.Score <= slow.Score {
		t.Fatalf("fast score %.2f should exceed slow score %.2f", fast.Score, slow.Score)
	}
}

func TestStickyKeepsHealthyAssignment(t *testing.T) {
	cfg := baseConfig()
	tr := NewTracker(DefaultOptions())
	for i := 0; i < 5; i++ {
		recordOK(tr, "tokyo", 120)
		recordOK(tr, "malibu", 60)
	}
	sel := NewSelector(tr)

	// A new session resolves to the primary route.
	d := sel.Select(cfg, "", nil)
	if d.RouteName != "tokyo" {
		t.Fatalf("initial route = %q, want tokyo", d.RouteName)
	}

	// The same session keeps its route even though malibu is faster.
	d = sel.Select(cfg, "tokyo", nil)
	if d.RouteName != "tokyo" {
		t.Fatalf("sticky route = %q, want tokyo", d.RouteName)
	}
	if d.FailoverFrom != "" {
		t.Fatalf("no failover expected, got %q", d.FailoverFrom)
	}
}

func TestStickyReassignsWhenUnhealthy(t *testing.T) {
	cfg := baseConfig()
	tr := NewTracker(DefaultOptions())
	recordOK(tr, "malibu", 60)
	for i := 0; i < 3; i++ {
		recordFail(tr, "tokyo")
	}
	sel := NewSelector(tr)

	d := sel.Select(cfg, "tokyo", nil)
	if d.RouteName != "malibu" {
		t.Fatalf("route = %q, want malibu after tokyo became unhealthy", d.RouteName)
	}
	if d.FailoverFrom != "tokyo" {
		t.Fatalf("failover source = %q, want tokyo", d.FailoverFrom)
	}
}

func TestPrimaryBackupFallsBackWhenPrimaryDisabled(t *testing.T) {
	cfg := baseConfig()
	tr := NewTracker(DefaultOptions())
	sel := NewSelector(tr)

	src := staticHealth{"tokyo": false, "malibu": true}
	d := sel.Select(cfg, "", src)
	if d.RouteName != "malibu" {
		t.Fatalf("route = %q, want malibu when primary is disabled", d.RouteName)
	}
}

func TestStaticModeUsesConfiguredRoute(t *testing.T) {
	cfg := baseConfig()
	cfg.Routing = config.Routing{Mode: "static", Static: "malibu"}
	tr := NewTracker(DefaultOptions())
	sel := NewSelector(tr)

	d := sel.Select(cfg, "", nil)
	if d.RouteName != "malibu" {
		t.Fatalf("route = %q, want malibu", d.RouteName)
	}
}

func TestWeightedModeUsesWeightsAndHealth(t *testing.T) {
	cfg := baseConfig()
	cfg.Routing = config.Routing{
		Mode:    "weighted",
		Weights: map[string]int{"tokyo": 10, "malibu": 1},
	}
	tr := NewTracker(DefaultOptions())
	for i := 0; i < 5; i++ {
		recordOK(tr, "tokyo", 100)
		recordOK(tr, "malibu", 100)
	}
	sel := NewSelector(tr)

	d := sel.Select(cfg, "", nil)
	if d.RouteName != "tokyo" {
		t.Fatalf("route = %q, want tokyo given much higher weight", d.RouteName)
	}
}

func TestSelectReportsNoUsableRoute(t *testing.T) {
	cfg := baseConfig()
	tr := NewTracker(DefaultOptions())
	sel := NewSelector(tr)

	src := staticHealth{"tokyo": false, "malibu": false}
	d := sel.Select(cfg, "", src)
	if d.RouteName != "" {
		t.Fatalf("route = %q, want empty when nothing is usable", d.RouteName)
	}
}

func TestDisabledRouteExcludedFromSelection(t *testing.T) {
	cfg := baseConfig()
	disabled := false
	cfg.Routes[0].Enabled = &disabled
	tr := NewTracker(DefaultOptions())
	sel := NewSelector(tr)

	d := sel.Select(cfg, "", nil)
	if d.RouteName != "malibu" {
		t.Fatalf("route = %q, want malibu because tokyo is disabled", d.RouteName)
	}
}

func TestAllStatsSorted(t *testing.T) {
	tr := NewTracker(DefaultOptions())
	recordOK(tr, "zeta", 10)
	recordOK(tr, "alpha", 10)
	all := tr.AllStats()
	if len(all) != 2 {
		t.Fatalf("stats len = %d, want 2", len(all))
	}
	if all[0].RouteName != "alpha" || all[1].RouteName != "zeta" {
		t.Fatalf("stats not sorted: %v", []string{all[0].RouteName, all[1].RouteName})
	}
}

type staticHealth map[string]bool

func (s staticHealth) Enabled(routeName string) bool { return s[routeName] }
