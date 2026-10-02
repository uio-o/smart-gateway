// Package config defines the on-disk configuration model shared by the
// smart-gateway agent. Nothing in this package is tied to a specific upstream
// service: every route, target and limit is operator supplied.
package config

import (
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Role describes what a node is allowed to do in a route chain.
type Role string

const (
	// RoleEntry accepts client traffic and performs path selection.
	RoleEntry Role = "entry"
	// RoleRelay forwards traffic to the next hop without understanding it.
	RoleRelay Role = "relay"
)

// Hop is one leg of a route. Exactly one of Node or URL is expected.
type Hop struct {
	// Node references a node name declared in Config.Nodes.
	Node string `yaml:"node,omitempty" json:"node,omitempty"`
	// URL is a direct target such as http://10.0.0.1:8080.
	URL string `yaml:"url,omitempty" json:"url,omitempty"`
	// Mode overrides the forwarding mode for this leg (http or tcp).
	Mode string `yaml:"mode,omitempty" json:"mode,omitempty"`
}

// Route is a named chain of hops ending at a service.
type Route struct {
	Name string `yaml:"name" json:"name"`
	// Service references Config.Services by name.
	Service string `yaml:"service" json:"service"`
	// Hops are traversed in order. The final hop must reach the service.
	Hops []Hop `yaml:"hops" json:"hops"`
	// Enabled defaults to true when omitted.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Weight is used by the weighted scheduling mode.
	Weight int `yaml:"weight,omitempty" json:"weight,omitempty"`
}

// IsEnabled reports whether the route is active.
func (r Route) IsEnabled() bool { return r.Enabled == nil || *r.Enabled }

// Service is an upstream the gateway may forward to.
type Service struct {
	Name string `yaml:"name" json:"name"`
	// Upstream is the origin base URL, for example http://10.0.0.5:8084.
	Upstream string `yaml:"upstream" json:"upstream"`
	// Host header override. Empty means keep the client value.
	HostOverride string `yaml:"host_override,omitempty" json:"host_override,omitempty"`
	// HealthPath is probed to decide whether the service is reachable.
	HealthPath string `yaml:"health_path,omitempty" json:"health_path,omitempty"`
	// PathPrefix is the client facing prefix that this service serves.
	PathPrefix string `yaml:"path_prefix,omitempty" json:"path_prefix,omitempty"`
	// StripPrefix removes PathPrefix before forwarding.
	StripPrefix bool `yaml:"strip_prefix,omitempty" json:"strip_prefix,omitempty"`
	// AllowPaths is a whitelist of forwarded paths. Empty means all paths.
	AllowPaths []string `yaml:"allow_paths,omitempty" json:"allow_paths,omitempty"`
	// AllowMethods is a whitelist of HTTP methods. Empty means GET and POST.
	AllowMethods []string `yaml:"allow_methods,omitempty" json:"allow_methods,omitempty"`
	// MaxBodyBytes caps request bodies. Zero means 100 MiB.
	MaxBodyBytes int64 `yaml:"max_body_bytes,omitempty" json:"max_body_bytes,omitempty"`
	// ReadTimeoutSeconds bounds an idle upstream read. Zero means no limit.
	ReadTimeoutSeconds int `yaml:"read_timeout_seconds,omitempty" json:"read_timeout_seconds,omitempty"`
	// SupportsSSE disables response buffering for streaming bodies.
	SupportsSSE bool `yaml:"supports_sse,omitempty" json:"supports_sse,omitempty"`
	// SupportsWebSocket enables Upgrade forwarding.
	SupportsWebSocket bool `yaml:"supports_websocket,omitempty" json:"supports_websocket,omitempty"`
}

// DefaultMaxBodyBytes is used when a service does not set MaxBodyBytes.
const DefaultMaxBodyBytes int64 = 100 << 20

// DefaultListen is the entry node bind address used when none is configured.
// It is loopback only: an entry node is expected to sit behind an operator
// managed reverse proxy that terminates TLS.
const DefaultListen = "127.0.0.1:8080"

// BodyLimit returns the effective request body limit.
func (s Service) BodyLimit() int64 {
	if s.MaxBodyBytes > 0 {
		return s.MaxBodyBytes
	}
	return DefaultMaxBodyBytes
}

// Methods returns the effective allowed method set, uppercased.
func (s Service) Methods() map[string]bool {
	src := s.AllowMethods
	if len(src) == 0 {
		src = []string{"GET", "POST"}
	}
	out := make(map[string]bool, len(src))
	for _, m := range src {
		out[strings.ToUpper(strings.TrimSpace(m))] = true
	}
	return out
}

// ReadTimeout returns the effective upstream idle read timeout.
func (s Service) ReadTimeout() time.Duration {
	if s.ReadTimeoutSeconds <= 0 {
		return 0
	}
	return time.Duration(s.ReadTimeoutSeconds) * time.Second
}

// Node is a remote agent reachable by the entry node.
type Node struct {
	Name string `yaml:"name" json:"name"`
	// Address is host:port of the peer agent.
	Address string `yaml:"address" json:"address"`
	// Mode is the forwarding mode used to reach this node (http or tcp).
	Mode string `yaml:"mode,omitempty" json:"mode,omitempty"`
	// Upstreams, on a relay node, maps a listen port to its next hop.
	Upstreams []RelayMapping `yaml:"upstreams,omitempty" json:"upstreams,omitempty"`
}

// RelayMapping binds a local listen port to a downstream target.
type RelayMapping struct {
	// Port is the local TCP port to listen on.
	Port int `yaml:"port" json:"port"`
	// Target is host:port of the next hop.
	Target string `yaml:"target" json:"target"`
}

// Limits holds node level guards.
type Limits struct {
	// MaxConcurrent caps simultaneous in-flight requests. Zero means unbounded.
	MaxConcurrent int `yaml:"max_concurrent,omitempty" json:"max_concurrent,omitempty"`
	// RatePerMinute caps requests per client per minute. Zero means disabled.
	RatePerMinute int `yaml:"rate_per_minute,omitempty" json:"rate_per_minute,omitempty"`
}

// Sticky controls session affinity behaviour.
type Sticky struct {
	// Enabled turns affinity on.
	Enabled bool `yaml:"enabled" json:"enabled"`
	// HoldMinutes keeps an assignment for this long after last use.
	HoldMinutes int `yaml:"hold_minutes,omitempty" json:"hold_minutes,omitempty"`
	// FailThreshold is the consecutive failure count that forces reassignment.
	FailThreshold int `yaml:"fail_threshold,omitempty" json:"fail_threshold,omitempty"`
	// UnhealthySuccessRate forces reassignment below this success ratio.
	UnhealthySuccessRate float64 `yaml:"unhealthy_success_rate,omitempty" json:"unhealthy_success_rate,omitempty"`
}

// Routing selects how the entry node picks a route.
type Routing struct {
	// Mode is one of static, primary_backup or weighted.
	Mode string `yaml:"mode" json:"mode"`
	// Primary is the preferred route name in primary_backup mode.
	Primary string `yaml:"primary,omitempty" json:"primary,omitempty"`
	// Backup is the fallback route name in primary_backup mode.
	Backup string `yaml:"backup,omitempty" json:"backup,omitempty"`
	// Static names the single route used in static mode.
	Static string `yaml:"static,omitempty" json:"static,omitempty"`
	// Weights maps route names to relative weights in weighted mode.
	Weights map[string]int `yaml:"weights,omitempty" json:"weights,omitempty"`
}

// Probe configures active health measurement.
type Probe struct {
	// Enabled turns probing on.
	Enabled bool `yaml:"enabled" json:"enabled"`
	// IntervalSeconds is the delay between probe rounds. Zero means 30.
	IntervalSeconds int `yaml:"interval_seconds,omitempty" json:"interval_seconds,omitempty"`
	// TimeoutSeconds bounds a single probe. Zero means 10.
	TimeoutSeconds int `yaml:"timeout_seconds,omitempty" json:"timeout_seconds,omitempty"`
	// Path is appended to a service upstream when probing. Zero means "/".
	Path string `yaml:"path,omitempty" json:"path,omitempty"`
	// Samples is the sliding window size. Zero means 20.
	Samples int `yaml:"samples,omitempty" json:"samples,omitempty"`
}

// Interval returns the effective probe interval.
func (p Probe) Interval() time.Duration {
	if p.IntervalSeconds <= 0 {
		return 30 * time.Second
	}
	return time.Duration(p.IntervalSeconds) * time.Second
}

// Timeout returns the effective per-probe timeout.
func (p Probe) Timeout() time.Duration {
	if p.TimeoutSeconds <= 0 {
		return 10 * time.Second
	}
	return time.Duration(p.TimeoutSeconds) * time.Second
}

// Window returns the effective sample window size.
func (p Probe) Window() int {
	if p.Samples <= 0 {
		return 20
	}
	return p.Samples
}

// Control connects the agent to its panel.
type Control struct {
	// PanelURL is the base URL of the control panel.
	PanelURL string `yaml:"panel_url,omitempty" json:"panel_url,omitempty"`
	// NodeID identifies this agent to the panel.
	NodeID string `yaml:"node_id,omitempty" json:"node_id,omitempty"`
	// Token authenticates the agent to the panel.
	Token string `yaml:"token,omitempty" json:"token,omitempty"`
	// PollSeconds is the config polling interval. Zero means 15.
	PollSeconds int `yaml:"poll_seconds,omitempty" json:"poll_seconds,omitempty"`
	// PushSeconds is the status push interval. Zero means 10.
	PushSeconds int `yaml:"push_seconds,omitempty" json:"push_seconds,omitempty"`
}

// PollInterval returns the effective config poll interval.
func (c Control) PollInterval() time.Duration {
	if c.PollSeconds <= 0 {
		return 15 * time.Second
	}
	return time.Duration(c.PollSeconds) * time.Second
}

// PushInterval returns the effective status push interval.
func (c Control) PushInterval() time.Duration {
	if c.PushSeconds <= 0 {
		return 10 * time.Second
	}
	return time.Duration(c.PushSeconds) * time.Second
}

// Logging configures local logging behaviour.
type Logging struct {
	// Level is debug, info, warn or error.
	Level string `yaml:"level,omitempty" json:"level,omitempty"`
	// AuditPath appends one JSON line per forwarded request. Empty disables it.
	AuditPath string `yaml:"audit_path,omitempty" json:"audit_path,omitempty"`
}

// Config is the full agent configuration.
type Config struct {
	// Role is entry or relay.
	Role Role `yaml:"role" json:"role"`
	// Listen is host:port the entry node binds.
	Listen string `yaml:"listen,omitempty" json:"listen,omitempty"`
	// RelayPorts are TCP ports a relay node accepts peer connections on.
	RelayPorts []RelayMapping `yaml:"relay_ports,omitempty" json:"relay_ports,omitempty"`

	Services []Service `yaml:"services,omitempty" json:"services,omitempty"`
	Nodes    []Node    `yaml:"nodes,omitempty" json:"nodes,omitempty"`
	Routes   []Route   `yaml:"routes,omitempty" json:"routes,omitempty"`

	Routing Routing `yaml:"routing,omitempty" json:"routing,omitempty"`
	Sticky  Sticky  `yaml:"sticky,omitempty" json:"sticky,omitempty"`
	Probe   Probe   `yaml:"probe,omitempty" json:"probe,omitempty"`
	Limits  Limits  `yaml:"limits,omitempty" json:"limits,omitempty"`
	Control Control `yaml:"control,omitempty" json:"control,omitempty"`
	Logging Logging `yaml:"logging,omitempty" json:"logging,omitempty"`

	// Version is bumped by the panel on every config change.
	Version int64 `yaml:"version,omitempty" json:"version,omitempty"`
}

// Load reads and validates a YAML configuration file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	return Parse(raw)
}

// Parse validates a YAML document.
func Parse(raw []byte) (*Config, error) {
	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Validate checks structural invariants that the runtime relies on.
//
// It is deliberately free of side effects: the same document is validated by
// the panel before rendering and by the agent before applying, and a validator
// that rewrote the document would make those two disagree.
func (c *Config) Validate() error {
	switch c.Role {
	case RoleEntry:
		// A missing listen address means "use the agent's own default", so it
		// is valid. The runtime substitutes the effective address.
		if c.Listen != "" {
			if _, _, err := net.SplitHostPort(c.Listen); err != nil {
				return fmt.Errorf("entry listen address %q is not host:port", c.Listen)
			}
		}
	case RoleRelay:
		if len(c.RelayPorts) == 0 {
			return fmt.Errorf("relay node requires at least one relay port")
		}
		for _, m := range c.RelayPorts {
			if m.Port <= 0 || m.Port > 65535 {
				return fmt.Errorf("relay port %d out of range", m.Port)
			}
			if strings.TrimSpace(m.Target) == "" {
				return fmt.Errorf("relay port %d has empty target", m.Port)
			}
		}
	case "":
		return fmt.Errorf("role is required")
	default:
		return fmt.Errorf("unknown role %q", c.Role)
	}

	seen := make(map[string]bool, len(c.Services))
	for _, s := range c.Services {
		if strings.TrimSpace(s.Name) == "" {
			return fmt.Errorf("service name is required")
		}
		if seen[s.Name] {
			return fmt.Errorf("duplicate service %q", s.Name)
		}
		seen[s.Name] = true
		if !strings.HasPrefix(s.Upstream, "http://") && !strings.HasPrefix(s.Upstream, "https://") {
			return fmt.Errorf("service %q upstream must be an http(s) URL", s.Name)
		}
	}

	nodeNames := make(map[string]bool, len(c.Nodes))
	for _, n := range c.Nodes {
		if strings.TrimSpace(n.Name) == "" {
			return fmt.Errorf("node name is required")
		}
		if nodeNames[n.Name] {
			return fmt.Errorf("duplicate node %q", n.Name)
		}
		nodeNames[n.Name] = true
	}

	for _, r := range c.Routes {
		if strings.TrimSpace(r.Name) == "" {
			return fmt.Errorf("route name is required")
		}
		if !seen[r.Service] {
			return fmt.Errorf("route %q references unknown service %q", r.Name, r.Service)
		}
		// An empty hop list means "forward straight to the service", which is
		// the simple case of a route with no intermediate node.
		for _, h := range r.Hops {
			if strings.TrimSpace(h.Node) == "" && strings.TrimSpace(h.URL) == "" {
				return fmt.Errorf("route %q has a hop with neither node nor url", r.Name)
			}
			if h.Node != "" && !nodeNames[h.Node] {
				return fmt.Errorf("route %q references unknown node %q", r.Name, h.Node)
			}
		}
	}

	if c.Role == RoleEntry {
		switch c.Routing.Mode {
		case "static":
			if c.Routing.Static == "" {
				return fmt.Errorf("static routing requires static route name")
			}
		case "primary_backup":
			if c.Routing.Primary == "" {
				return fmt.Errorf("primary_backup routing requires primary route")
			}
		case "weighted":
			if len(c.Routing.Weights) == 0 {
				return fmt.Errorf("weighted routing requires weights")
			}
		case "":
			return fmt.Errorf("routing mode is required for entry nodes")
		default:
			return fmt.Errorf("unknown routing mode %q", c.Routing.Mode)
		}
	}
	return nil
}

// ServiceByName returns a service by name.
func (c *Config) ServiceByName(name string) (Service, bool) {
	for _, s := range c.Services {
		if s.Name == name {
			return s, true
		}
	}
	return Service{}, false
}

// NodeByName returns a node by name.
func (c *Config) NodeByName(name string) (Node, bool) {
	for _, n := range c.Nodes {
		if n.Name == name {
			return n, true
		}
	}
	return Node{}, false
}

// RouteByName returns a route by name.
func (c *Config) RouteByName(name string) (Route, bool) {
	for _, r := range c.Routes {
		if r.Name == name {
			return r, true
		}
	}
	return Route{}, false
}
