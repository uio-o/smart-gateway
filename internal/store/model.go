// Package store persists panel state. The schema mirrors the agent
// configuration so that generating an agent config is a pure transformation.
package store

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// ErrNotFound is returned when a record does not exist.
var ErrNotFound = errors.New("not found")

// Role describes how a node participates in a route.
type Role string

const (
	// RoleEntry accepts client traffic and picks a route.
	RoleEntry Role = "entry"
	// RoleRelay forwards bytes to its next hop.
	RoleRelay Role = "relay"
)

// Node is a gateway node registered with the panel.
type Node struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Role      Role      `json:"role"`
	Address   string    `json:"address"`
	Remark    string    `json:"remark"`
	Enabled   bool      `json:"enabled"`
	RelayPort int       `json:"relay_port"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// Runtime state reported by the agent.
	LastSeenAt  time.Time `json:"last_seen_at"`
	AgentOK     bool      `json:"agent_ok"`
	AgentError  string    `json:"agent_error"`
	ConfigHash  string    `json:"config_hash"`
	ActiveConns int64     `json:"active_conns"`
}

// Service is an upstream the gateway may forward to.
type Service struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Upstream is the origin base URL.
	Upstream string `json:"upstream"`
	// HostOverride rewrites the Host header when set.
	HostOverride string `json:"host_override"`
	// HealthPath is probed to determine reachability.
	HealthPath string `json:"health_path"`
	// PathPrefix is the client facing prefix served by this service.
	PathPrefix string `json:"path_prefix"`
	// StripPrefix removes PathPrefix before forwarding.
	StripPrefix bool `json:"strip_prefix"`
	// AllowPaths is a whitelist of forwarded paths. Empty allows all.
	AllowPaths []string `json:"allow_paths"`
	// AllowMethods is a whitelist of methods. Empty means GET and POST.
	AllowMethods []string `json:"allow_methods"`
	// MaxBodyMB caps request bodies in MiB.
	MaxBodyMB int64 `json:"max_body_mb"`
	// ReadTimeoutSeconds bounds an idle upstream read.
	ReadTimeoutSeconds int `json:"read_timeout_seconds"`
	// SupportsSSE disables response buffering.
	SupportsSSE bool `json:"supports_sse"`
	// SupportsWebSocket enables upgrade forwarding.
	SupportsWebSocket bool `json:"supports_websocket"`
	// Enabled takes the service out of consideration when false.
	Enabled bool `json:"enabled"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Route is a named path through nodes ending at a service.
type Route struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Service string `json:"service"`
	// NodeNames are the hops in order. Empty means forward straight to the
	// service, which is the "direct" case.
	NodeNames []string `json:"node_names"`
	// Port is the local relay port used on the last relay hop.
	Port    int  `json:"port"`
	Weight  int  `json:"weight"`
	Enabled bool `json:"enabled"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Routing holds the site wide scheduling policy.
type Routing struct {
	// Mode is static, primary_backup or weighted.
	Mode string `json:"mode"`
	// Primary and Backup name routes in primary_backup mode.
	Primary string `json:"primary"`
	Backup  string `json:"backup"`
	// Static names the route in static mode.
	Static string `json:"static"`
	// Weights maps route name to weight in weighted mode.
	Weights map[string]int `json:"weights"`
}

// Sticky controls session affinity.
type Sticky struct {
	Enabled       bool `json:"enabled"`
	HoldMinutes   int  `json:"hold_minutes"`
	FailThreshold int  `json:"fail_threshold"`
}

// Probe controls active measurement.
type Probe struct {
	Enabled         bool `json:"enabled"`
	IntervalSeconds int  `json:"interval_seconds"`
	TimeoutSeconds  int  `json:"timeout_seconds"`
	Samples         int  `json:"samples"`
}

// Limits holds node level guards.
type Limits struct {
	MaxConcurrent int `json:"max_concurrent"`
	RatePerMinute int `json:"rate_per_minute"`
}

// Settings is the singleton site configuration.
type Settings struct {
	Routing Routing `json:"routing"`
	Sticky  Sticky  `json:"sticky"`
	Probe   Probe   `json:"probe"`
	Limits  Limits  `json:"limits"`
	// AuditPath is where entry nodes append request records.
	AuditPath string `json:"audit_path"`
	// LogLevel is passed to agents.
	LogLevel string `json:"log_level"`
	// Version increments on every change so agents can detect updates.
	Version int64 `json:"version"`
	// RelayPortStart and RelayPortEnd bound the port pool the panel assigns.
	RelayPortStart int `json:"relay_port_start"`
	RelayPortEnd   int `json:"relay_port_end"`
	// EntryListen is the address entry nodes bind. Empty means the agent keeps
	// whatever its own -listen flag says, which is the right default when the
	// agent already knows its local bind address.
	EntryListen string `json:"entry_listen"`

	UpdatedAt time.Time `json:"updated_at"`
}

// DefaultSettings returns the settings a fresh panel starts with.
func DefaultSettings() Settings {
	return Settings{
		Routing: Routing{
			Mode:    "primary_backup",
			Weights: map[string]int{},
		},
		Sticky: Sticky{
			Enabled:       true,
			HoldMinutes:   120,
			FailThreshold: 3,
		},
		Probe: Probe{
			Enabled:         true,
			IntervalSeconds: 30,
			TimeoutSeconds:  10,
			Samples:         20,
		},
		Limits: Limits{
			MaxConcurrent: 512,
			RatePerMinute: 600,
		},
		LogLevel:       "info",
		RelayPortStart: 3001,
		RelayPortEnd:   3100,
		Version:        1,
	}
}

// NewID returns a random identifier.
func NewID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// ValidateNodeName rejects names that would break generated configuration.
func ValidateNodeName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("node name is required")
	}
	if strings.ContainsAny(name, " \t\n/:?#") {
		return errors.New("node name must not contain spaces or reserved characters")
	}
	return nil
}

// ValidateAddress checks a host:port pair.
func ValidateAddress(addr string) error {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return errors.New("address is required")
	}
	if strings.Contains(addr, "://") {
		return errors.New("address must be host:port without a scheme")
	}
	host, port, found := strings.Cut(addr, ":")
	if !found || host == "" || port == "" {
		return errors.New("address must be host:port")
	}
	for _, r := range port {
		if r < '0' || r > '9' {
			return errors.New("port must be numeric")
		}
	}
	return nil
}

// ValidateUpstream checks an origin URL.
func ValidateUpstream(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return errors.New("upstream is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("upstream is not a valid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("upstream must use http or https")
	}
	if u.Host == "" {
		return errors.New("upstream must include a host")
	}
	if u.User != nil {
		return errors.New("upstream must not contain credentials")
	}
	return nil
}
