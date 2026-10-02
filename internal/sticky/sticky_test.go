package sticky

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAssignAndLookup(t *testing.T) {
	tbl := New(DefaultConfig())
	tbl.Assign("k1", "tokyo")
	if got := tbl.Lookup("k1"); got != "tokyo" {
		t.Fatalf("route = %q, want tokyo", got)
	}
	if got := tbl.Lookup("missing"); got != "" {
		t.Fatalf("unknown key should return empty, got %q", got)
	}
}

func TestDisabledTableIgnoresEverything(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Enabled = false
	tbl := New(cfg)
	tbl.Assign("k1", "tokyo")
	if got := tbl.Lookup("k1"); got != "" {
		t.Fatalf("disabled table must not return assignments, got %q", got)
	}
}

func TestHoldExpiryEvictsAssignment(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Hold = time.Minute
	tbl := New(cfg)

	base := time.Now()
	tbl.SetClock(func() time.Time { return base })
	tbl.Assign("k1", "tokyo")

	if got := tbl.Lookup("k1"); got != "tokyo" {
		t.Fatalf("expected assignment before expiry, got %q", got)
	}

	tbl.SetClock(func() time.Time { return base.Add(2 * time.Minute) })
	if got := tbl.Lookup("k1"); got != "" {
		t.Fatalf("assignment should expire, got %q", got)
	}
}

func TestLookupRefreshesLastUsed(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Hold = 10 * time.Minute
	tbl := New(cfg)

	base := time.Now()
	now := base
	tbl.SetClock(func() time.Time { return now })
	tbl.Assign("k1", "tokyo")

	now = base.Add(9 * time.Minute)
	if got := tbl.Lookup("k1"); got != "tokyo" {
		t.Fatal("assignment should still be valid")
	}

	// Another 9 minutes pass, but the previous lookup refreshed the timer.
	now = base.Add(18 * time.Minute)
	if got := tbl.Lookup("k1"); got != "tokyo" {
		t.Fatal("lookup should have extended the assignment lifetime")
	}
}

func TestFailuresDropAssignmentAtThreshold(t *testing.T) {
	cfg := DefaultConfig()
	cfg.FailThreshold = 2
	tbl := New(cfg)
	tbl.Assign("k1", "tokyo")

	tbl.NoteFailure("k1")
	if got := tbl.Lookup("k1"); got != "tokyo" {
		t.Fatal("one failure should not drop the assignment")
	}

	tbl.NoteFailure("k1")
	if got := tbl.Lookup("k1"); got != "" {
		t.Fatal("assignment should be dropped at the failure threshold")
	}
}

func TestSuccessResetsFailures(t *testing.T) {
	cfg := DefaultConfig()
	cfg.FailThreshold = 2
	tbl := New(cfg)
	tbl.Assign("k1", "tokyo")

	tbl.NoteFailure("k1")
	tbl.NoteSuccess("k1")
	tbl.NoteFailure("k1")

	if got := tbl.Lookup("k1"); got != "tokyo" {
		t.Fatal("a success between failures should keep the assignment")
	}
}

func TestReassignToDifferentRoute(t *testing.T) {
	tbl := New(DefaultConfig())
	tbl.Assign("k1", "tokyo")
	tbl.Assign("k1", "malibu")
	if got := tbl.Lookup("k1"); got != "malibu" {
		t.Fatalf("route = %q, want malibu", got)
	}
}

func TestMaxEntriesEvictsOldest(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxEntries = 2
	tbl := New(cfg)

	base := time.Now()
	now := base
	tbl.SetClock(func() time.Time { return now })

	tbl.Assign("a", "r")
	now = base.Add(time.Second)
	tbl.Assign("b", "r")
	now = base.Add(2 * time.Second)
	tbl.Assign("c", "r")

	if tbl.Size() > 2 {
		t.Fatalf("table size = %d, want at most 2", tbl.Size())
	}
	if got := tbl.Lookup("a"); got != "" {
		t.Fatal("oldest entry should have been evicted")
	}
	if got := tbl.Lookup("c"); got != "r" {
		t.Fatal("newest entry should remain")
	}
}

func TestFlushAndForget(t *testing.T) {
	tbl := New(DefaultConfig())
	tbl.Assign("a", "r")
	tbl.Assign("b", "r")
	tbl.Forget("a")
	if got := tbl.Lookup("a"); got != "" {
		t.Fatal("forgotten key should not resolve")
	}
	tbl.Flush()
	if tbl.Size() != 0 {
		t.Fatalf("size = %d after flush, want 0", tbl.Size())
	}
}

func TestSessionKeyPrefersExplicitHeader(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("X-Session-Id", "abc123")
	key := SessionKey(req, nil, "1.2.3.4")
	if key != "sid:abc123" {
		t.Fatalf("key = %q, want sid:abc123", key)
	}
}

func TestSessionKeyFallsBackToFingerprint(t *testing.T) {
	body := []byte(`{"model":"claude-opus-5","messages":[{"role":"user","content":"refactor"}]}`)

	req1 := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req1.Header.Set("User-Agent", "claude-cli/1.0")
	k1 := SessionKey(req1, body, "1.2.3.4")

	req2 := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req2.Header.Set("User-Agent", "claude-cli/1.0")
	k2 := SessionKey(req2, body, "1.2.3.4")

	if k1 == "" {
		t.Fatal("fingerprint key should not be empty")
	}
	if k1 != k2 {
		t.Fatalf("same client and body should produce the same key: %q vs %q", k1, k2)
	}
}

func TestSessionKeyDiffersByClient(t *testing.T) {
	body := []byte(`{"model":"x"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("User-Agent", "cli/1.0")

	a := SessionKey(req, body, "1.2.3.4")
	b := SessionKey(req, body, "5.6.7.8")
	if a == b {
		t.Fatal("different client addresses should produce different keys")
	}
}

func TestSessionKeyHandlesHostPort(t *testing.T) {
	body := []byte(`{"model":"x"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("User-Agent", "cli/1.0")

	a := SessionKey(req, body, "1.2.3.4")
	b := SessionKey(req, body, "1.2.3.4:55555")
	if a != b {
		t.Fatal("address with port should normalise to the same key")
	}
}

func TestSessionKeyBodyPrefixBounded(t *testing.T) {
	// Bodies that share a long prefix collapse to the same key by design; the
	// fingerprint only reads a bounded prefix so cost stays constant.
	long := make([]byte, 4096)
	for i := range long {
		long[i] = 'a'
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("User-Agent", "cli/1.0")

	k1 := SessionKey(req, long, "1.2.3.4")
	if len(k1) == 0 {
		t.Fatal("expected a key for a large body")
	}
	if len(k1) > 64 {
		t.Fatalf("key length %d looks unbounded", len(k1))
	}
}

func TestEntriesSnapshot(t *testing.T) {
	tbl := New(DefaultConfig())
	tbl.Assign("a", "r1")
	tbl.Assign("b", "r2")
	entries := tbl.Entries()
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
}
