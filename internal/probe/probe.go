// Package probe actively measures each route so selection has fresh data.
package probe

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/uio-o/smart-gateway/internal/config"
	"github.com/uio-o/smart-gateway/internal/route"
)

// Result is a single probe outcome.
type Result struct {
	RouteName string
	OK        bool
	Latency   time.Duration
	Status    int
	Error     string
}

// Prober measures routes on a schedule.
type Prober struct {
	client  *http.Client
	timeout time.Duration
}

// New creates a prober.
func New(timeout time.Duration) *Prober {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &Prober{
		client: &http.Client{
			// Redirects are not followed: a probe should report the first hop.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Transport: &http.Transport{
				Proxy: nil,
				DialContext: (&net.Dialer{
					Timeout:   timeout,
					KeepAlive: 15 * time.Second,
				}).DialContext,
				MaxIdleConns:          16,
				IdleConnTimeout:       30 * time.Second,
				TLSHandshakeTimeout:   timeout,
				ResponseHeaderTimeout: timeout,
				DisableCompression:    true,
			},
		},
		timeout: timeout,
	}
}

// Measure probes one route through its first hop and returns the observation.
//
// The probe goes through the real chain, so the measurement reflects what a
// client request would actually experience rather than a synthetic ping.
func (p *Prober) Measure(ctx context.Context, cfg *config.Config, r config.Route) Result {
	res := Result{RouteName: r.Name}

	svc, ok := cfg.ServiceByName(r.Service)
	if !ok {
		res.Error = "service not found"
		return res
	}

	target, err := resolveProbeURL(cfg, r, svc)
	if err != nil {
		res.Error = err.Error()
		return res
	}

	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	// Identify the probe so operators can filter it out of upstream logs.
	req.Header.Set("User-Agent", "smart-gateway-probe/1")
	req.Header.Set("X-Smart-Gateway-Probe", "1")

	start := time.Now()
	resp, err := p.client.Do(req)
	res.Latency = time.Since(start)
	if err != nil {
		res.Error = trimErr(err)
		return res
	}
	defer func() { _ = resp.Body.Close() }()
	// Drain a little so the connection can be reused; the body is not needed.
	_, _ = io.CopyN(io.Discard, resp.Body, 512)

	res.Status = resp.StatusCode
	// Any response proves the path is reachable; 5xx still counts as unhealthy
	// because the upstream could not serve the request.
	res.OK = resp.StatusCode < 500
	if !res.OK {
		res.Error = "upstream returned " + http.StatusText(resp.StatusCode)
	}
	return res
}

// resolveProbeURL builds the URL a probe should request for a route.
func resolveProbeURL(cfg *config.Config, r config.Route, svc config.Service) (string, error) {
	base := strings.TrimSuffix(svc.Upstream, "/")
	path := svc.HealthPath
	if strings.TrimSpace(path) == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	// For a chain with a relay hop, the probe enters through the first hop so
	// the whole chain participates in the measurement.
	if len(r.Hops) > 0 {
		first := r.Hops[0]
		if first.URL != "" {
			return strings.TrimSuffix(first.URL, "/") + path, nil
		}
		if node, ok := cfg.NodeByName(first.Node); ok && node.Address != "" {
			scheme := "http"
			return scheme + "://" + node.Address + path, nil
		}
	}
	return base + path, nil
}

// trimErr shortens transport errors so audit records stay readable.
func trimErr(err error) string {
	msg := err.Error()
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return msg
}

// Run measures every enabled route on an interval until the context is done.
//
// It is safe to start exactly one Run per Prober.
func (p *Prober) Run(ctx context.Context, getCfg func() *config.Config, tracker *route.Tracker) {
	for {
		cfg := getCfg()
		if cfg != nil && cfg.Probe.Enabled {
			for _, r := range cfg.Routes {
				if !r.IsEnabled() {
					continue
				}
				res := p.Measure(ctx, cfg, r)
				tracker.Record(res.RouteName, route.Sample{
					OK:      res.OK,
					Latency: res.Latency,
					At:      time.Now(),
				})
				if !res.OK {
					tracker.SetLastError(res.RouteName, res.Error)
				}
			}
		}

		interval := 30 * time.Second
		if cfg != nil && cfg.Probe.Interval() > 0 {
			interval = cfg.Probe.Interval()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// Once measures a single route immediately, used by the panel test-now path.
func (p *Prober) Once(ctx context.Context, cfg *config.Config, routeName string) (Result, bool) {
	for _, r := range cfg.Routes {
		if r.Name != routeName {
			continue
		}
		return p.Measure(ctx, cfg, r), true
	}
	return Result{RouteName: routeName, Error: "route not found"}, false
}
