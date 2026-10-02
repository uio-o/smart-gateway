// Package panel implements the control plane: it stores desired state and
// renders it into an agent configuration.
package panel

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/uio-o/smart-gateway/internal/config"
	"github.com/uio-o/smart-gateway/internal/store"
)

// Render builds the configuration an agent of the given role should run.
//
// The generator is deliberately pure: given the same stored state it always
// produces the same document, so a config hash is a reliable change signal and
// an agent can safely restart from it.
//
// nodeName identifies which relay the configuration is for. It is ignored for
// entry nodes, which see the whole topology.
func Render(role store.Role, nodeName string, nodes []store.Node, services []store.Service, routes []store.Route, settings store.Settings) (*config.Config, error) {
	switch role {
	case store.RoleEntry:
		return renderEntry(nodes, services, routes, settings)
	case store.RoleRelay:
		return renderRelay(nodeName, nodes, services, routes, settings)
	default:
		return nil, fmt.Errorf("unknown role %q", role)
	}
}

// renderEntry builds the entry node configuration.
func renderEntry(nodes []store.Node, services []store.Service, routes []store.Route, settings store.Settings) (*config.Config, error) {
	cfg := &config.Config{
		Role: config.RoleEntry,
		// The bind address is a node-local fact, so the panel only sends one
		// when an operator explicitly pinned it. Otherwise the agent keeps its
		// own -listen value and validation fills the default.
		Listen:  strings.TrimSpace(settings.EntryListen),
		Version: settings.Version,
		Routing: config.Routing{
			Mode:    settings.Routing.Mode,
			Primary: settings.Routing.Primary,
			Backup:  settings.Routing.Backup,
			Static:  settings.Routing.Static,
			Weights: settings.Routing.Weights,
		},
		Sticky: config.Sticky{
			Enabled:       settings.Sticky.Enabled,
			HoldMinutes:   settings.Sticky.HoldMinutes,
			FailThreshold: settings.Sticky.FailThreshold,
		},
		Probe: config.Probe{
			Enabled:         settings.Probe.Enabled,
			IntervalSeconds: settings.Probe.IntervalSeconds,
			TimeoutSeconds:  settings.Probe.TimeoutSeconds,
			Samples:         settings.Probe.Samples,
		},
		Limits: config.Limits{
			MaxConcurrent: settings.Limits.MaxConcurrent,
			RatePerMinute: settings.Limits.RatePerMinute,
		},
		Logging: config.Logging{
			Level:     settings.LogLevel,
			AuditPath: settings.AuditPath,
		},
	}

	relayByName := map[string]store.Node{}
	for _, n := range nodes {
		if !n.Enabled || n.Role != store.RoleRelay {
			continue
		}
		if n.Address == "" {
			continue
		}
		relayByName[n.Name] = n
		cfg.Nodes = append(cfg.Nodes, config.Node{
			Name:    n.Name,
			Address: n.Address,
			Mode:    "tcp",
		})
	}

	serviceByName := map[string]store.Service{}
	for _, s := range services {
		if !s.Enabled {
			continue
		}
		serviceByName[s.Name] = s
		cfg.Services = append(cfg.Services, config.Service{
			Name:               s.Name,
			Upstream:           s.Upstream,
			HostOverride:       s.HostOverride,
			HealthPath:         s.HealthPath,
			PathPrefix:         s.PathPrefix,
			StripPrefix:        s.StripPrefix,
			AllowPaths:         s.AllowPaths,
			AllowMethods:       s.AllowMethods,
			MaxBodyBytes:       s.MaxBodyMB << 20,
			ReadTimeoutSeconds: s.ReadTimeoutSeconds,
			SupportsSSE:        s.SupportsSSE,
			SupportsWebSocket:  s.SupportsWebSocket,
		})
	}

	for _, r := range routes {
		if !r.Enabled {
			continue
		}
		if _, ok := serviceByName[r.Service]; !ok {
			continue
		}
		hops := make([]config.Hop, 0, len(r.NodeNames))
		usable := true
		for _, name := range r.NodeNames {
			n, ok := relayByName[name]
			if !ok {
				// A route whose hop is disabled is skipped rather than
				// silently rewritten, so the operator sees the route vanish.
				usable = false
				break
			}
			hops = append(hops, config.Hop{Node: n.Name, Mode: "tcp"})
		}
		if !usable {
			continue
		}
		cfg.Routes = append(cfg.Routes, config.Route{
			Name:    r.Name,
			Service: r.Service,
			Hops:    hops,
			Weight:  r.Weight,
		})
	}

	if err := pruneRouting(cfg); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// RelayMappingsFor computes the port mappings a specific relay node must run.
//
// For each enabled route that includes the node, the panel resolves the next
// hop after this node and assigns the route's port to it. The last hop in a
// chain forwards to the service origin.
func RelayMappingsFor(nodeName string, nodes []store.Node, services []store.Service, routes []store.Route) ([]config.RelayMapping, error) {
	serviceByName := map[string]store.Service{}
	for _, s := range services {
		if s.Enabled {
			serviceByName[s.Name] = s
		}
	}
	nodeByName := map[string]store.Node{}
	for _, n := range nodes {
		nodeByName[n.Name] = n
	}

	mappings := map[int]config.RelayMapping{}

	for _, r := range routes {
		if !r.Enabled {
			continue
		}
		idx := indexOf(r.NodeNames, nodeName)
		if idx < 0 {
			continue
		}
		port := r.Port
		if port <= 0 {
			return nil, fmt.Errorf("route %q has no relay port assigned", r.Name)
		}

		var target string
		if idx+1 < len(r.NodeNames) {
			next, ok := nodeByName[r.NodeNames[idx+1]]
			if !ok || !next.Enabled || next.Address == "" {
				continue
			}
			target = next.Address
		} else {
			svc, ok := serviceByName[r.Service]
			if !ok {
				continue
			}
			target = upstreamHostPort(svc.Upstream)
			if target == "" {
				return nil, fmt.Errorf("service %q upstream has no host", r.Service)
			}
		}

		if existing, ok := mappings[port]; ok && existing.Target != target {
			return nil, fmt.Errorf("port %d is mapped to both %q and %q", port, existing.Target, target)
		}
		mappings[port] = config.RelayMapping{Port: port, Target: target}
	}

	out := make([]config.RelayMapping, 0, len(mappings))
	for _, m := range mappings {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Port < out[j].Port })
	return out, nil
}

// renderRelay builds a relay node configuration.
//
// The caller supplies the mappings for the specific node, because a relay only
// knows about the routes that actually pass through it.
func renderRelay(nodeName string, nodes []store.Node, services []store.Service, routes []store.Route, settings store.Settings) (*config.Config, error) {
	mappings, err := RelayMappingsFor(nodeName, nodes, services, routes)
	if err != nil {
		return nil, err
	}
	cfg := &config.Config{
		Role:       config.RoleRelay,
		RelayPorts: mappings,
		Version:    settings.Version,
		Logging:    config.Logging{Level: settings.LogLevel},
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// pruneRouting removes routing references that no longer resolve, so a stale
// route name cannot make the whole configuration invalid.
func pruneRouting(cfg *config.Config) error {
	known := map[string]bool{}
	for _, r := range cfg.Routes {
		known[r.Name] = true
	}

	switch cfg.Routing.Mode {
	case "static":
		if !known[cfg.Routing.Static] {
			if len(cfg.Routes) == 1 {
				cfg.Routing.Static = cfg.Routes[0].Name
			} else {
				return fmt.Errorf("routing references unknown static route %q", cfg.Routing.Static)
			}
		}
	case "primary_backup":
		if !known[cfg.Routing.Primary] {
			if len(cfg.Routes) > 0 {
				cfg.Routing.Primary = cfg.Routes[0].Name
			} else {
				return fmt.Errorf("routing references unknown primary route %q", cfg.Routing.Primary)
			}
		}
		if cfg.Routing.Backup != "" && !known[cfg.Routing.Backup] {
			cfg.Routing.Backup = ""
		}
	case "weighted":
		weights := map[string]int{}
		for name, w := range cfg.Routing.Weights {
			if known[name] {
				weights[name] = w
			}
		}
		for _, r := range cfg.Routes {
			if _, ok := weights[r.Name]; !ok {
				weights[r.Name] = r.Weight
			}
		}
		cfg.Routing.Weights = weights
	}
	return nil
}

// ConfigHash returns a stable digest of a rendered configuration.
func ConfigHash(cfg *config.Config) (string, error) {
	if cfg == nil {
		return "", fmt.Errorf("config is nil")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "role=%s\nversion=%d\n", cfg.Role, cfg.Version)

	for _, s := range cfg.Services {
		fmt.Fprintf(&b, "service=%s|%s|%s|%s|%t|%t|%t|%s|%s\n",
			s.Name, s.Upstream, s.HealthPath, s.PathPrefix, s.StripPrefix,
			s.SupportsSSE, s.SupportsWebSocket,
			strings.Join(s.AllowPaths, ","), strings.Join(s.AllowMethods, ","))
	}
	for _, n := range cfg.Nodes {
		fmt.Fprintf(&b, "node=%s|%s\n", n.Name, n.Address)
	}
	for _, r := range cfg.Routes {
		hops := make([]string, 0, len(r.Hops))
		for _, h := range r.Hops {
			hops = append(hops, h.Node+h.URL)
		}
		fmt.Fprintf(&b, "route=%s|%s|%s\n", r.Name, r.Service, strings.Join(hops, ">"))
	}
	for _, m := range cfg.RelayPorts {
		fmt.Fprintf(&b, "relay=%d|%s\n", m.Port, m.Target)
	}
	fmt.Fprintf(&b, "routing=%s|%s|%s|%s\n", cfg.Routing.Mode, cfg.Routing.Primary,
		cfg.Routing.Backup, cfg.Routing.Static)
	fmt.Fprintf(&b, "sticky=%t|%d|%d\n", cfg.Sticky.Enabled, cfg.Sticky.HoldMinutes, cfg.Sticky.FailThreshold)
	fmt.Fprintf(&b, "probe=%t|%d|%d|%d\n", cfg.Probe.Enabled, cfg.Probe.IntervalSeconds,
		cfg.Probe.TimeoutSeconds, cfg.Probe.Samples)
	fmt.Fprintf(&b, "limits=%d|%d\n", cfg.Limits.MaxConcurrent, cfg.Limits.RatePerMinute)

	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])[:16], nil
}

func indexOf(list []string, v string) int {
	for i, item := range list {
		if item == v {
			return i
		}
	}
	return -1
}

// upstreamHostPort extracts host:port from an origin URL.
func upstreamHostPort(raw string) string {
	raw = strings.TrimPrefix(strings.TrimPrefix(raw, "https://"), "http://")
	raw = strings.TrimSuffix(raw, "/")
	if i := strings.IndexAny(raw, "/?#"); i >= 0 {
		raw = raw[:i]
	}
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, ":") {
		return raw + ":80"
	}
	return raw
}
