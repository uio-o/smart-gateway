package panel

import (
	"strings"
	"testing"

	"github.com/uio-o/smart-gateway/internal/config"
	"github.com/uio-o/smart-gateway/internal/store"
)

func fixture() ([]store.Node, []store.Service, []store.Route, store.Settings) {
	nodes := []store.Node{
		{ID: "n1", Name: "tokyo", Role: store.RoleRelay, Address: "1.1.1.1:3001", Enabled: true},
		{ID: "n2", Name: "malibu", Role: store.RoleRelay, Address: "2.2.2.2:3001", Enabled: true},
		{ID: "n3", Name: "entry", Role: store.RoleEntry, Enabled: true},
	}
	services := []store.Service{
		{
			ID: "s1", Name: "origin", Upstream: "http://10.0.0.5:8084",
			HealthPath: "/health", PathPrefix: "/v1/",
			AllowPaths: []string{"/v1/messages"}, AllowMethods: []string{"POST"},
			MaxBodyMB: 64, SupportsSSE: true, SupportsWebSocket: true, Enabled: true,
		},
	}
	routes := []store.Route{
		{ID: "r1", Name: "via-tokyo", Service: "origin", NodeNames: []string{"tokyo"}, Port: 3001, Weight: 100, Enabled: true},
		{ID: "r2", Name: "via-malibu", Service: "origin", NodeNames: []string{"malibu"}, Port: 3002, Weight: 50, Enabled: true},
		{ID: "r3", Name: "direct", Service: "origin", NodeNames: nil, Weight: 10, Enabled: true},
	}
	settings := store.DefaultSettings()
	settings.Routing = store.Routing{Mode: "primary_backup", Primary: "via-tokyo", Backup: "direct"}
	return nodes, services, routes, settings
}

func TestRenderEntryIncludesEverything(t *testing.T) {
	nodes, services, routes, settings := fixture()

	cfg, err := Render(store.RoleEntry, "entry", nodes, services, routes, settings)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if cfg.Role != config.RoleEntry {
		t.Fatalf("role = %q", cfg.Role)
	}
	if len(cfg.Services) != 1 || cfg.Services[0].Name != "origin" {
		t.Fatalf("services = %+v", cfg.Services)
	}
	// Both enabled relay nodes are usable as hops.
	if len(cfg.Nodes) != 2 {
		t.Fatalf("nodes = %d, want 2", len(cfg.Nodes))
	}
	if len(cfg.Routes) != 3 {
		t.Fatalf("routes = %d, want 3", len(cfg.Routes))
	}
	// The entry node itself must never appear as a hop.
	for _, n := range cfg.Nodes {
		if n.Name == "entry" {
			t.Fatal("entry node must not be rendered as a hop")
		}
	}
	if cfg.Routing.Mode != "primary_backup" || cfg.Routing.Primary != "via-tokyo" {
		t.Fatalf("routing = %+v", cfg.Routing)
	}
}

func TestRenderEntryConvertsBodyLimitToBytes(t *testing.T) {
	nodes, services, routes, settings := fixture()
	cfg, err := Render(store.RoleEntry, "entry", nodes, services, routes, settings)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if got := cfg.Services[0].MaxBodyBytes; got != 64<<20 {
		t.Fatalf("max body bytes = %d, want %d", got, 64<<20)
	}
}

func TestRenderEntryRejectsTopologyWithNoUsableRoute(t *testing.T) {
	nodes, services, routes, settings := fixture()
	services[0].Enabled = false

	// Disabling the only service removes every route, leaving nothing to serve
	// traffic. The panel must surface that instead of emitting a config the
	// agent would silently accept and then fail every request with.
	if _, err := Render(store.RoleEntry, "entry", nodes, services, routes, settings); err == nil {
		t.Fatal("expected an error when no route remains usable")
	}
}

func TestRenderEntrySkipsRouteWithDisabledHop(t *testing.T) {
	nodes, services, routes, settings := fixture()
	nodes[0].Enabled = false // tokyo

	cfg, err := Render(store.RoleEntry, "entry", nodes, services, routes, settings)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, r := range cfg.Routes {
		if r.Name == "via-tokyo" {
			t.Fatal("a route with a disabled hop must be dropped")
		}
	}
}

func TestRenderEntryRepairsPrimaryWhenRouteGone(t *testing.T) {
	nodes, services, routes, settings := fixture()
	// Remove the configured primary by disabling its hop.
	nodes[0].Enabled = false

	cfg, err := Render(store.RoleEntry, "entry", nodes, services, routes, settings)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if cfg.Routing.Primary == "via-tokyo" {
		t.Fatal("primary should have been repaired to an existing route")
	}
	if _, ok := cfg.RouteByName(cfg.Routing.Primary); !ok {
		t.Fatalf("repaired primary %q does not exist", cfg.Routing.Primary)
	}
}

func TestRenderEntryDropsStaleBackup(t *testing.T) {
	nodes, services, routes, settings := fixture()
	settings.Routing.Backup = "does-not-exist"

	cfg, err := Render(store.RoleEntry, "entry", nodes, services, routes, settings)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if cfg.Routing.Backup != "" {
		t.Fatalf("stale backup should be cleared, got %q", cfg.Routing.Backup)
	}
}

func TestRenderEntryErrorsWhenNoRouteCanSatisfyStatic(t *testing.T) {
	nodes, services, routes, settings := fixture()
	settings.Routing = store.Routing{Mode: "static", Static: "gone"}

	if _, err := Render(store.RoleEntry, "entry", nodes, services, routes, settings); err == nil {
		t.Fatal("expected an error when the static route does not exist and no fallback is unambiguous")
	}
}

func TestRenderRelayOnlyIncludesItsOwnRoutes(t *testing.T) {
	nodes, services, routes, settings := fixture()

	cfg, err := Render(store.RoleRelay, "tokyo", nodes, services, routes, settings)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if len(cfg.RelayPorts) != 1 {
		t.Fatalf("relay ports = %d, want 1", len(cfg.RelayPorts))
	}
	got := cfg.RelayPorts[0]
	if got.Port != 3001 || got.Target != "10.0.0.5:8084" {
		t.Fatalf("mapping = %+v, want port 3001 to the origin", got)
	}
}

func TestRenderRelayForNodeNotInAnyRoute(t *testing.T) {
	nodes, services, routes, settings := fixture()
	nodes = append(nodes, store.Node{ID: "n9", Name: "idle", Role: store.RoleRelay, Address: "9.9.9.9:3001", Enabled: true})

	// A relay with no routes cannot be rendered as a valid relay config
	// because a relay requires at least one port.
	if _, err := Render(store.RoleRelay, "idle", nodes, services, routes, settings); err == nil {
		t.Fatal("a relay with no routes should be reported as invalid")
	}
}

func TestRelayMappingsChainToNextHop(t *testing.T) {
	nodes := []store.Node{
		{Name: "tokyo", Role: store.RoleRelay, Address: "1.1.1.1:3001", Enabled: true},
		{Name: "bgp", Role: store.RoleRelay, Address: "3.3.3.3:3001", Enabled: true},
	}
	services := []store.Service{
		{Name: "origin", Upstream: "https://10.0.0.5:8443", Enabled: true},
	}
	routes := []store.Route{
		{Name: "tokyo-via-bgp", Service: "origin", NodeNames: []string{"tokyo", "bgp"}, Port: 3005, Enabled: true},
	}

	tokyo, err := RelayMappingsFor("tokyo", nodes, services, routes)
	if err != nil {
		t.Fatalf("tokyo mappings: %v", err)
	}
	if len(tokyo) != 1 || tokyo[0].Target != "3.3.3.3:3001" {
		t.Fatalf("tokyo should forward to the next hop, got %+v", tokyo)
	}

	bgp, err := RelayMappingsFor("bgp", nodes, services, routes)
	if err != nil {
		t.Fatalf("bgp mappings: %v", err)
	}
	if len(bgp) != 1 || bgp[0].Target != "10.0.0.5:8443" {
		t.Fatalf("the last hop should forward to the origin, got %+v", bgp)
	}
}

func TestRelayMappingsRejectPortCollision(t *testing.T) {
	nodes := []store.Node{
		{Name: "a", Role: store.RoleRelay, Address: "1.1.1.1:3001", Enabled: true},
		{Name: "b", Role: store.RoleRelay, Address: "2.2.2.2:3001", Enabled: true},
	}
	services := []store.Service{{Name: "origin", Upstream: "http://10.0.0.5:8084", Enabled: true}}
	// Two different routes reuse the same port on node "a" but resolve to
	// different targets, which cannot both be honoured.
	routes := []store.Route{
		{Name: "r1", Service: "origin", NodeNames: []string{"a", "b"}, Port: 3009, Enabled: true},
		{Name: "r2", Service: "origin", NodeNames: []string{"a"}, Port: 3009, Enabled: true},
	}

	if _, err := RelayMappingsFor("a", nodes, services, routes); err == nil {
		t.Fatal("expected an error for a port mapped to two different targets")
	}
}

func TestRelayMappingsSkipDisabledRoutes(t *testing.T) {
	nodes := []store.Node{{Name: "tokyo", Role: store.RoleRelay, Address: "1.1.1.1:3001", Enabled: true}}
	services := []store.Service{{Name: "origin", Upstream: "http://10.0.0.5:8084", Enabled: true}}
	routes := []store.Route{
		{Name: "off", Service: "origin", NodeNames: []string{"tokyo"}, Port: 3001, Enabled: false},
	}

	mappings, err := RelayMappingsFor("tokyo", nodes, services, routes)
	if err != nil {
		t.Fatalf("mappings: %v", err)
	}
	if len(mappings) != 0 {
		t.Fatalf("disabled routes should not produce mappings, got %+v", mappings)
	}
}

func TestUpstreamHostPort(t *testing.T) {
	cases := map[string]string{
		"http://10.0.0.5:8084":          "10.0.0.5:8084",
		"https://api.example.com":       "api.example.com:80",
		"https://api.example.com:8443/": "api.example.com:8443",
		"http://10.0.0.5:80/base/path":  "10.0.0.5:80",
	}
	for in, want := range cases {
		if got := upstreamHostPort(in); got != want {
			t.Errorf("upstreamHostPort(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestConfigHashIsStableAndSensitive(t *testing.T) {
	nodes, services, routes, settings := fixture()

	cfg1, err := Render(store.RoleEntry, "entry", nodes, services, routes, settings)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	h1, err := ConfigHash(cfg1)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}

	cfg2, err := Render(store.RoleEntry, "entry", nodes, services, routes, settings)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	h2, _ := ConfigHash(cfg2)
	if h1 != h2 {
		t.Fatalf("hash is not stable: %s vs %s", h1, h2)
	}

	// A real change must alter the hash.
	settings.Routing.Primary = "via-malibu"
	cfg3, err := Render(store.RoleEntry, "entry", nodes, services, routes, settings)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	h3, _ := ConfigHash(cfg3)
	if h1 == h3 {
		t.Fatal("hash should change when the routing policy changes")
	}
}

func TestRenderEntryProducesValidConfigForAgent(t *testing.T) {
	nodes, services, routes, settings := fixture()
	cfg, err := Render(store.RoleEntry, "entry", nodes, services, routes, settings)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	// Round trip through the agent's own validator to prove the panel cannot
	// emit a configuration the agent would reject.
	if err := cfg.Validate(); err != nil {
		t.Fatalf("rendered config is invalid for the agent: %v", err)
	}
}

func TestRenderEntryEmptyTopologyStillValidates(t *testing.T) {
	settings := store.DefaultSettings()
	settings.Routing = store.Routing{Mode: "primary_backup"}

	// With nothing configured the panel must not panic; it reports an error
	// that the operator can act on.
	if _, err := Render(store.RoleEntry, "entry", nil, nil, nil, settings); err == nil {
		t.Fatal("expected an error when there are no routes at all")
	}
}

func TestRelayMappingsRequirePort(t *testing.T) {
	nodes := []store.Node{{Name: "tokyo", Role: store.RoleRelay, Address: "1.1.1.1:3001", Enabled: true}}
	services := []store.Service{{Name: "origin", Upstream: "http://10.0.0.5:8084", Enabled: true}}
	routes := []store.Route{
		{Name: "r", Service: "origin", NodeNames: []string{"tokyo"}, Port: 0, Enabled: true},
	}

	_, err := RelayMappingsFor("tokyo", nodes, services, routes)
	if err == nil {
		t.Fatal("a route without an assigned port must be reported")
	}
	if !strings.Contains(err.Error(), "no relay port") {
		t.Fatalf("unexpected error message: %v", err)
	}
}
