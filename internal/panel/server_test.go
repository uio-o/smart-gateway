package panel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/uio-o/smart-gateway/internal/config"
	"github.com/uio-o/smart-gateway/internal/store"
)

// newTestPanel builds a panel backed by a temporary database.
func newTestPanel(t *testing.T, opts Options) (*Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "panel.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if opts.AdminToken == "" {
		opts.AdminToken = "test-admin-token"
	}
	opts.Store = st
	srv, err := New(opts)
	if err != nil {
		t.Fatalf("new panel: %v", err)
	}
	return srv, st
}

// do issues a request against the panel handler.
func do(t *testing.T, srv *Server, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	return w
}

func TestPublicEndpointsNeedNoToken(t *testing.T) {
	srv, _ := newTestPanel(t, Options{})

	// The UI document itself carries no secrets, so it must load before login.
	w := do(t, srv, http.MethodGet, "/", "", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("GET / content type = %q", ct)
	}
	if !strings.Contains(w.Body.String(), "Smart Gateway") {
		t.Fatal("UI document does not look like the panel UI")
	}

	// The installer is a generic script and must also be reachable unauthenticated.
	w = do(t, srv, http.MethodGet, "/install.sh", "", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /install.sh = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "--panel") {
		t.Fatal("install script is missing its documented flags")
	}

	w = do(t, srv, http.MethodGet, "/healthz", "", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /healthz = %d, want 200", w.Code)
	}
}

func TestOperatorAPIRefusesAnonymousRequests(t *testing.T) {
	srv, _ := newTestPanel(t, Options{})

	for _, path := range []string{"/api/nodes", "/api/services", "/api/routes", "/api/tokens", "/api/settings", "/api/summary"} {
		w := do(t, srv, http.MethodGet, path, "", "")
		if w.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without a token = %d, want 401", path, w.Code)
		}
	}
}

func TestUnknownPathIsNotFound(t *testing.T) {
	srv, _ := newTestPanel(t, Options{})
	w := do(t, srv, http.MethodGet, "/definitely-not-a-route", "", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("GET /definitely-not-a-route = %d, want 404", w.Code)
	}
}

func TestNodeLifecycleThroughAPI(t *testing.T) {
	const token = "operator-token"
	srv, _ := newTestPanel(t, Options{AdminToken: token})

	created := do(t, srv, http.MethodPost, "/api/nodes", token,
		`{"name":"tokyo","role":"relay","address":"1.2.3.4:3001","enabled":true}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("create node = %d: %s", created.Code, created.Body.String())
	}
	var node store.Node
	if err := json.Unmarshal(created.Body.Bytes(), &node); err != nil {
		t.Fatalf("decode node: %v", err)
	}
	if node.ID == "" {
		t.Fatal("created node has no id")
	}

	list := do(t, srv, http.MethodGet, "/api/nodes", token, "")
	if list.Code != http.StatusOK {
		t.Fatalf("list nodes = %d", list.Code)
	}
	var wrap struct {
		Nodes []store.Node `json:"nodes"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &wrap); err != nil {
		t.Fatalf("decode node list: %v", err)
	}
	if len(wrap.Nodes) != 1 || wrap.Nodes[0].Name != "tokyo" {
		t.Fatalf("unexpected node list: %+v", wrap.Nodes)
	}

	// A node name is embedded in generated configuration identifiers, so the
	// store must reject one that would break them.
	bad := do(t, srv, http.MethodPost, "/api/nodes", token,
		`{"name":"bad name","role":"relay","address":"1.2.3.4:3001"}`)
	if bad.Code == http.StatusCreated {
		t.Fatal("store accepted a node name containing a space")
	}

	del := do(t, srv, http.MethodDelete, "/api/nodes/"+node.ID, token, "")
	if del.Code != http.StatusOK {
		t.Fatalf("delete node = %d: %s", del.Code, del.Body.String())
	}
}

func TestRouteDeletionIsGuardedBySchedulingPolicy(t *testing.T) {
	const token = "operator-token"
	srv, _ := newTestPanel(t, Options{AdminToken: token})
	ctx := context.Background()

	// A route hop must name a node the store knows about, so create one first.
	if _, err := srv.store.SaveNode(ctx, store.Node{
		Name: "tokyo", Role: store.RoleRelay, Address: "1.2.3.4:3001", Enabled: true,
	}); err != nil {
		t.Fatalf("save node: %v", err)
	}
	svc, err := srv.store.SaveService(ctx, store.Service{
		Name: "origin", Upstream: "http://10.0.0.5:8084", Enabled: true, MaxBodyMB: 32,
	})
	if err != nil {
		t.Fatalf("save service: %v", err)
	}
	rt, err := srv.store.SaveRoute(ctx, store.Route{
		Name: "via-tokyo", Service: svc.Name, NodeNames: []string{"tokyo"}, Port: 3001, Enabled: true,
	})
	if err != nil {
		t.Fatalf("save route: %v", err)
	}
	settings := store.DefaultSettings()
	settings.Routing = store.Routing{Mode: "primary_backup", Primary: rt.Name}
	if _, err := srv.store.SaveSettings(ctx, settings); err != nil {
		t.Fatalf("save settings: %v", err)
	}

	// Deleting the active primary would leave the policy dangling, so it is
	// refused; the operator must change the policy first.
	w := do(t, srv, http.MethodDelete, "/api/routes/"+rt.ID, token, "")
	if w.Code != http.StatusConflict {
		t.Fatalf("delete primary route = %d, want 409: %s", w.Code, w.Body.String())
	}

	settings.Routing = store.Routing{Mode: "weighted", Weights: map[string]int{rt.Name: 1}}
	if _, err := srv.store.SaveSettings(ctx, settings); err != nil {
		t.Fatalf("save settings: %v", err)
	}
	w = do(t, srv, http.MethodDelete, "/api/routes/"+rt.ID, token, "")
	if w.Code != http.StatusOK {
		t.Fatalf("delete unreferenced route = %d: %s", w.Code, w.Body.String())
	}
}

func TestAgentConfigRequiresAnAgentToken(t *testing.T) {
	srv, st := newTestPanel(t, Options{})

	w := do(t, srv, http.MethodGet, "/api/agent/config?node=tokyo", "", "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous agent config = %d, want 401", w.Code)
	}

	tok, err := st.SaveToken(context.Background(), store.Token{NodeName: "tokyo", Label: "tokyo"})
	if err != nil {
		t.Fatalf("save token: %v", err)
	}
	w = do(t, srv, http.MethodGet, "/api/agent/config?node=tokyo", tok.Token, "")
	// The node does not exist yet, so the panel must report that rather than
	// handing back an empty configuration the agent would happily apply.
	if w.Code == http.StatusOK {
		t.Fatalf("agent config for an unknown node = 200: %s", w.Body.String())
	}
}

func TestDownloadPrefersTheLocalAgentDirectory(t *testing.T) {
	dir := t.TempDir()
	name := "smart-gateway-agent-linux-amd64"
	payload := []byte("#!/bin/true\n")
	if err := os.WriteFile(filepath.Join(dir, name), payload, 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}

	srv, _ := newTestPanel(t, Options{AgentDir: dir})
	w := do(t, srv, http.MethodGet, "/download/"+name, "", "")
	if w.Code != http.StatusOK {
		t.Fatalf("download from local dir = %d", w.Code)
	}
	if w.Body.String() != string(payload) {
		t.Fatal("served bytes do not match the local binary")
	}

	// Path traversal must not escape the configured directory.
	w = do(t, srv, http.MethodGet, "/download/../../etc/passwd", "", "")
	if w.Code == http.StatusOK {
		t.Fatalf("traversal request was served: %s", w.Body.String())
	}
}

// TestAgentConfigLeavesListenToTheNode guards a regression where the panel
// forced 127.0.0.1:8080 on every entry node, silently overriding the bind
// address the operator configured on the machine itself.
func TestAgentConfigLeavesListenToTheNode(t *testing.T) {
	const token = "operator-token"
	srv, _ := newTestPanel(t, Options{AdminToken: token})
	ctx := context.Background()

	if _, err := srv.store.SaveNode(ctx, store.Node{
		Name: "entry", Role: store.RoleEntry, Address: "203.0.113.7:443", Enabled: true,
	}); err != nil {
		t.Fatalf("save node: %v", err)
	}
	svc, err := srv.store.SaveService(ctx, store.Service{
		Name: "origin", Upstream: "http://10.0.0.5:8084", Enabled: true, MaxBodyMB: 32,
	})
	if err != nil {
		t.Fatalf("save service: %v", err)
	}
	if _, err := srv.store.SaveRoute(ctx, store.Route{
		Name: "direct", Service: svc.Name, Enabled: true,
	}); err != nil {
		t.Fatalf("save route: %v", err)
	}

	tok, err := srv.store.SaveToken(ctx, store.Token{NodeName: "entry", Label: "entry"})
	if err != nil {
		t.Fatalf("save token: %v", err)
	}

	w := do(t, srv, http.MethodGet, "/api/agent/config?node=entry", tok.Token, "")
	if w.Code != http.StatusOK {
		t.Fatalf("agent config = %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Config config.Config `json:"config"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	if resp.Config.Listen != "" {
		t.Fatalf("panel dictated the listen address %q; the node owns it", resp.Config.Listen)
	}
}

func TestMountPrefixIsStripped(t *testing.T) {
	srv, _ := newTestPanel(t, Options{MountPrefix: "/sg"})

	w := do(t, srv, http.MethodGet, "/sg/healthz", "", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /sg/healthz = %d, want 200", w.Code)
	}
}
