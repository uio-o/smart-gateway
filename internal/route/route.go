// Package route decides which path a request should take. It combines active
// probe results with passive failure signals and session affinity.
package route

import (
	"math"
	"sort"
	"sync"
	"time"

	"github.com/uio-o/smart-gateway/internal/config"
)

// Sample is one observation of a route.
type Sample struct {
	// OK reports whether the attempt succeeded.
	OK bool
	// Latency is the total time to first byte or completion.
	Latency time.Duration
	// At is when the attempt finished.
	At time.Time
}

// Stats summarises a route over a sliding window.
type Stats struct {
	RouteName    string    `json:"route_name"`
	Samples      int       `json:"samples"`
	SuccessRate  float64   `json:"success_rate"`
	P50MS        float64   `json:"p50_ms"`
	P95MS        float64   `json:"p95_ms"`
	LastError    string    `json:"last_error,omitempty"`
	LastOKAt     time.Time `json:"last_ok_at,omitempty"`
	LastFailAt   time.Time `json:"last_fail_at,omitempty"`
	ConsecFails  int       `json:"consecutive_failures"`
	Healthy      bool      `json:"healthy"`
	Score        float64   `json:"score"`
}

// Tracker records per-route observations.
type Tracker struct {
	mu       sync.RWMutex
	window   int
	samples  map[string][]Sample
	state    map[string]*routeState
	// thresholds
	failThreshold int
	minSuccess    float64
}

type routeState struct {
	consecutiveFails int
	lastErr          string
	lastOKAt         time.Time
	lastFailAt       time.Time
	// manualDisabled is set by the operator.
	manualDisabled bool
}

// Options tunes health evaluation.
type Options struct {
	// Window is the number of recent samples kept per route.
	Window int
	// FailThreshold is the consecutive failure count that marks a route down.
	FailThreshold int
	// MinSuccessRate is the success ratio below which a route is unhealthy.
	MinSuccessRate float64
}

// DefaultOptions returns conservative defaults: prefer to keep a working path
// instead of flapping between paths.
func DefaultOptions() Options {
	return Options{
		Window:         20,
		FailThreshold:  3,
		MinSuccessRate: 0.8,
	}
}

// NewTracker creates a tracker with the given options.
func NewTracker(opts Options) *Tracker {
	if opts.Window <= 0 {
		opts.Window = 20
	}
	if opts.FailThreshold <= 0 {
		opts.FailThreshold = 3
	}
	if opts.MinSuccessRate <= 0 {
		opts.MinSuccessRate = 0.8
	}
	return &Tracker{
		window:        opts.Window,
		samples:       map[string][]Sample{},
		state:         map[string]*routeState{},
		failThreshold: opts.FailThreshold,
		minSuccess:    opts.MinSuccessRate,
	}
}

// Record appends an observation for a route.
func (t *Tracker) Record(routeName string, s Sample) {
	if routeName == "" {
		return
	}
	if s.At.IsZero() {
		s.At = time.Now()
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	hist := append(t.samples[routeName], s)
	if len(hist) > t.window {
		hist = hist[len(hist)-t.window:]
	}
	t.samples[routeName] = hist

	st := t.stateFor(routeName)
	if s.OK {
		st.consecutiveFails = 0
		st.lastOKAt = s.At
	} else {
		st.consecutiveFails++
		st.lastFailAt = s.At
	}
}

// SetLastError records the most recent error text for a route.
func (t *Tracker) SetLastError(routeName, message string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stateFor(routeName).lastErr = message
}

// SetDisabled lets the operator take a route out of rotation.
func (t *Tracker) SetDisabled(routeName string, disabled bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stateFor(routeName).manualDisabled = disabled
}

func (t *Tracker) stateFor(name string) *routeState {
	st, ok := t.state[name]
	if !ok {
		st = &routeState{}
		t.state[name] = st
	}
	return st
}

// Stats returns the current summary for a route.
func (t *Tracker) Stats(routeName string) Stats {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.statsLocked(routeName)
}

func (t *Tracker) statsLocked(routeName string) Stats {
	hist := t.samples[routeName]
	st := t.state[routeName]
	out := Stats{RouteName: routeName, Samples: len(hist)}
	if st != nil {
		out.ConsecFails = st.consecutiveFails
		out.LastError = st.lastErr
		out.LastOKAt = st.lastOKAt
		out.LastFailAt = st.lastFailAt
	}

	if len(hist) == 0 {
		// No data yet: treat as healthy so a fresh deployment can serve.
		out.Healthy = st == nil || !st.manualDisabled
		out.Score = 0
		return out
	}

	okCount := 0
	latencies := make([]float64, 0, len(hist))
	for _, s := range hist {
		if s.OK {
			okCount++
			latencies = append(latencies, float64(s.Latency.Milliseconds()))
		}
	}
	out.SuccessRate = float64(okCount) / float64(len(hist))
	out.P50MS = percentile(latencies, 50)
	out.P95MS = percentile(latencies, 95)

	out.Healthy = true
	if st != nil {
		if st.manualDisabled {
			out.Healthy = false
		}
		if st.consecutiveFails >= t.failThreshold {
			out.Healthy = false
		}
	}
	if out.SuccessRate < t.minSuccess {
		out.Healthy = false
	}
	out.Score = score(out)
	return out
}

// AllStats returns summaries for every tracked route.
func (t *Tracker) AllStats() []Stats {
	t.mu.RLock()
	defer t.mu.RUnlock()
	names := make([]string, 0, len(t.samples))
	seen := map[string]bool{}
	for name := range t.samples {
		names = append(names, name)
		seen[name] = true
	}
	for name := range t.state {
		if !seen[name] {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	out := make([]Stats, 0, len(names))
	for _, name := range names {
		out = append(out, t.statsLocked(name))
	}
	return out
}

// score blends success rate and latency into a 0..100 value. Lower latency and
// higher success both increase the score.
func score(s Stats) float64 {
	if s.Samples == 0 {
		return 0
	}
	success := s.SuccessRate * 100

	// Latency term: 0 ms -> 100, 1000 ms -> 0, clamped.
	lat := s.P95MS
	if lat <= 0 {
		lat = s.P50MS
	}
	latScore := 100 - (lat / 10.0)
	if latScore < 0 {
		latScore = 0
	}
	if latScore > 100 {
		latScore = 100
	}

	// Consecutive failures apply a decaying penalty on top of the ratio.
	penalty := math.Min(float64(s.ConsecFails)*5, 40)

	v := 0.5*success + 0.5*latScore - penalty
	if v < 0 {
		v = 0
	}
	if v > 100 {
		v = 100
	}
	return math.Round(v*100) / 100
}

// percentile computes a simple nearest-rank percentile.
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	cp := make([]float64, len(sorted))
	copy(cp, sorted)
	sort.Float64s(cp)
	rank := int(math.Ceil(p/100*float64(len(cp)))) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(cp) {
		rank = len(cp) - 1
	}
	return cp[rank]
}

// Selector chooses a route using the configured mode and current health.
type Selector struct {
	tracker *Tracker
}

// NewSelector creates a selector over a tracker.
func NewSelector(t *Tracker) *Selector { return &Selector{tracker: t} }

// Decision is the outcome of route selection.
type Decision struct {
	// RouteName is the chosen route.
	RouteName string
	// Reason explains the choice, for audit logs.
	Reason string
	// FailoverFrom is set when the decision replaced an unhealthy route.
	FailoverFrom string
}

// HealthSource answers whether a route may be used.
type HealthSource interface {
	// Enabled reports whether a route is enabled in configuration.
	Enabled(routeName string) bool
}

// Select picks a route for a request. stickyRoute is the previously assigned
// route for this session, or empty when the session is new.
//
// Selection always starts from the routes that are currently healthy. The
// configured mode only orders that healthy set, so a degraded primary can
// never shadow a working backup.
func (s *Selector) Select(cfg *config.Config, stickyRoute string, enabled HealthSource) Decision {
	routes := usableRoutes(cfg, enabled)
	if len(routes) == 0 {
		return Decision{Reason: "no usable route"}
	}

	// Session affinity: keep the assignment while the route stays healthy.
	if cfg.Sticky.Enabled && stickyRoute != "" && contains(routes, stickyRoute) {
		st := s.tracker.Stats(stickyRoute)
		if st.Healthy {
			return Decision{RouteName: stickyRoute, Reason: "sticky assignment healthy"}
		}
		return Decision{
			RouteName:    s.pickByMode(cfg, s.healthyOnly(routes)),
			Reason:       "sticky route unhealthy, reassigned",
			FailoverFrom: stickyRoute,
		}
	}

	return Decision{RouteName: s.pickByMode(cfg, s.healthyOnly(routes)), Reason: "initial assignment"}
}

// healthyOnly filters a route list down to routes that may currently serve
// traffic. When nothing has measurements yet every route is kept so a fresh
// deployment can still serve.
func (s *Selector) healthyOnly(routes []string) []string {
	var measured, healthy []string
	for _, name := range routes {
		st := s.tracker.Stats(name)
		if st.Samples == 0 {
			continue
		}
		measured = append(measured, name)
		if st.Healthy {
			healthy = append(healthy, name)
		}
	}
	if len(healthy) > 0 {
		return healthy
	}
	if len(measured) == 0 {
		return routes
	}
	// Every measured route is unhealthy: fall back to the best scoring one so
	// the operator still sees real upstream errors instead of a local refusal.
	return []string{s.healthiest(routes)}
}

func (s *Selector) pickByMode(cfg *config.Config, routes []string) string {
	switch cfg.Routing.Mode {
	case "static":
		if contains(routes, cfg.Routing.Static) {
			return cfg.Routing.Static
		}
		return routes[0]

	case "primary_backup":
		if cfg.Routing.Primary != "" && contains(routes, cfg.Routing.Primary) {
			return cfg.Routing.Primary
		}
		if cfg.Routing.Backup != "" && contains(routes, cfg.Routing.Backup) {
			return cfg.Routing.Backup
		}
		return routes[0]

	case "weighted":
		best, bestScore := "", math.Inf(-1)
		for _, name := range routes {
			w := float64(cfg.Routing.Weights[name])
			if w <= 0 {
				continue
			}
			// Weight scales the measured score; latency still breaks ties.
			v := s.tracker.Stats(name).Score * w
			if v > bestScore {
				best, bestScore = name, v
			}
		}
		if best != "" {
			return best
		}
		return routes[0]
	}

	// Unknown mode: fall back to the healthiest available route.
	return s.healthiest(routes)
}

func (s *Selector) healthiest(routes []string) string {
	best, bestScore := routes[0], math.Inf(-1)
	for _, name := range routes {
		v := s.tracker.Stats(name).Score
		if v > bestScore {
			best, bestScore = name, v
		}
	}
	return best
}

// usableRoutes lists enabled routes whose health permits use.
func usableRoutes(cfg *config.Config, enabled HealthSource) []string {
	var out []string
	for _, r := range cfg.Routes {
		if !r.IsEnabled() {
			continue
		}
		if enabled != nil && !enabled.Enabled(r.Name) {
			continue
		}
		out = append(out, r.Name)
	}
	return out
}

func contains(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}
