package config

import "testing"

const validEntry = `
role: entry
listen: 127.0.0.1:8080
services:
  - name: aether-main
    upstream: http://209.50.229.83:8084
    health_path: /health
    path_prefix: /v1/
    supports_sse: true
    supports_websocket: true
nodes:
  - name: tokyo
    address: 45.67.89.10:3001
routes:
  - name: tokyo-direct
    service: aether-main
    hops:
      - node: tokyo
routing:
  mode: primary_backup
  primary: tokyo-direct
sticky:
  enabled: true
`

func TestParseValidEntry(t *testing.T) {
	cfg, err := Parse([]byte(validEntry))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Role != RoleEntry {
		t.Fatalf("role = %q, want entry", cfg.Role)
	}
	if got := cfg.Sticky.HoldMinutes; got != 0 {
		t.Fatalf("hold minutes = %d, want 0 (unset)", got)
	}
	svc, ok := cfg.ServiceByName("aether-main")
	if !ok {
		t.Fatal("service not found")
	}
	if svc.BodyLimit() != DefaultMaxBodyBytes {
		t.Fatalf("body limit = %d, want default", svc.BodyLimit())
	}
	if !svc.Methods()["POST"] || !svc.Methods()["GET"] {
		t.Fatal("default methods should include GET and POST")
	}
	if _, ok := cfg.RouteByName("tokyo-direct"); !ok {
		t.Fatal("route not found")
	}
}

func TestParseRejectsUnknownServiceRef(t *testing.T) {
	bad := `
role: entry
listen: 127.0.0.1:8080
routes:
  - name: r1
    service: missing
    hops:
      - url: http://127.0.0.1:9
routing:
  mode: static
  static: r1
`
	if _, err := Parse([]byte(bad)); err == nil {
		t.Fatal("expected error for unknown service reference")
	}
}

func TestParseRejectsUnknownNodeRef(t *testing.T) {
	bad := `
role: entry
listen: 127.0.0.1:8080
services:
  - name: s1
    upstream: http://127.0.0.1:8084
routes:
  - name: r1
    service: s1
    hops:
      - node: ghost
routing:
  mode: static
  static: r1
`
	if _, err := Parse([]byte(bad)); err == nil {
		t.Fatal("expected error for unknown node reference")
	}
}

func TestParseRejectsBadUpstreamScheme(t *testing.T) {
	bad := `
role: entry
listen: 127.0.0.1:8080
services:
  - name: s1
    upstream: ftp://example.com
`
	if _, err := Parse([]byte(bad)); err == nil {
		t.Fatal("expected error for non-http upstream")
	}
}

func TestParseRejectsMissingRole(t *testing.T) {
	if _, err := Parse([]byte("listen: 127.0.0.1:8080\n")); err == nil {
		t.Fatal("expected error for missing role")
	}
}

func TestParseRelayRequiresPorts(t *testing.T) {
	bad := `
role: relay
relay_ports:
  - port: 0
    target: 127.0.0.1:8084
`
	if _, err := Parse([]byte(bad)); err == nil {
		t.Fatal("expected error for out-of-range relay port")
	}

	good := `
role: relay
relay_ports:
  - port: 3001
    target: 127.0.0.1:8084
`
	if _, err := Parse([]byte(good)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRouteEnabledDefaultsTrue(t *testing.T) {
	cfg, err := Parse([]byte(validEntry))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.Routes[0].IsEnabled() {
		t.Fatal("route should default to enabled")
	}
}
