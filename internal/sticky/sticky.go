// Package sticky stores session to route assignments so one conversation keeps
// using the same network path instead of hopping between paths per request.
package sticky

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Entry is one remembered assignment.
type Entry struct {
	// Route is the assigned route name.
	Route string `json:"route"`
	// AssignedAt is when the assignment was first made.
	AssignedAt time.Time `json:"assigned_at"`
	// LastUsed is refreshed on every request.
	LastUsed time.Time `json:"last_used"`
	// FailCount counts consecutive failures for this assignment.
	FailCount int `json:"fail_count"`
	// Key is the affinity key that produced this entry.
	Key string `json:"key"`
}

// Config tunes affinity behaviour.
type Config struct {
	// Enabled turns affinity on.
	Enabled bool
	// Hold is how long an idle assignment is remembered.
	Hold time.Duration
	// FailThreshold is the consecutive failure count that drops an assignment.
	FailThreshold int
	// MaxEntries bounds the table. Zero means 10000.
	MaxEntries int
}

// DefaultConfig returns conservative affinity defaults.
func DefaultConfig() Config {
	return Config{
		Enabled:       true,
		Hold:          2 * time.Hour,
		FailThreshold: 3,
		MaxEntries:    10000,
	}
}

// Table is a concurrency safe affinity map with time based eviction.
type Table struct {
	mu      sync.Mutex
	cfg     Config
	entries map[string]*Entry
	now     func() time.Time
}

// New creates a table with the given configuration.
func New(cfg Config) *Table {
	if cfg.MaxEntries <= 0 {
		cfg.MaxEntries = 10000
	}
	if cfg.FailThreshold <= 0 {
		cfg.FailThreshold = 3
	}
	if cfg.Hold <= 0 {
		cfg.Hold = 2 * time.Hour
	}
	return &Table{
		cfg:     cfg,
		entries: map[string]*Entry{},
		now:     time.Now,
	}
}

// SetClock overrides the time source, for tests.
func (t *Table) SetClock(fn func() time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.now = fn
}

// Lookup returns the assigned route for a key, or empty when none applies.
func (t *Table) Lookup(key string) string {
	if !t.cfg.Enabled || key == "" {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	e, ok := t.entries[key]
	if !ok {
		return ""
	}
	if t.now().Sub(e.LastUsed) > t.cfg.Hold {
		delete(t.entries, key)
		return ""
	}
	e.LastUsed = t.now()
	return e.Route
}

// Assign records a route for a key, keeping the original assignment time when
// the route is unchanged.
func (t *Table) Assign(key, route string) {
	if !t.cfg.Enabled || key == "" || route == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	now := t.now()
	if e, ok := t.entries[key]; ok {
		if e.Route == route {
			e.LastUsed = now
			e.FailCount = 0
			return
		}
		e.Route = route
		e.FailCount = 0
		e.LastUsed = now
		return
	}
	t.entries[key] = &Entry{
		Route:      route,
		AssignedAt: now,
		LastUsed:   now,
		Key:        key,
	}
	t.evictLocked()
}

// NoteFailure records a failed attempt for a key. Once the threshold is
// reached the assignment is dropped so the next request can be reassigned.
func (t *Table) NoteFailure(key string) {
	if !t.cfg.Enabled || key == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	e, ok := t.entries[key]
	if !ok {
		return
	}
	e.FailCount++
	if e.FailCount >= t.cfg.FailThreshold {
		delete(t.entries, key)
	}
}

// NoteSuccess clears the failure counter for a key.
func (t *Table) NoteSuccess(key string) {
	if !t.cfg.Enabled || key == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if e, ok := t.entries[key]; ok {
		e.FailCount = 0
		e.LastUsed = t.now()
	}
}

// Forget removes one assignment.
func (t *Table) Forget(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.entries, key)
}

// Flush removes every assignment.
func (t *Table) Flush() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.entries = map[string]*Entry{}
}

// Entries returns a snapshot of current assignments.
func (t *Table) Entries() []Entry {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Entry, 0, len(t.entries))
	for _, e := range t.entries {
		out = append(out, *e)
	}
	return out
}

// Size reports the number of live assignments.
func (t *Table) Size() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.entries)
}

// evictLocked drops expired entries, then the oldest ones if still over budget.
func (t *Table) evictLocked() {
	now := t.now()
	for k, e := range t.entries {
		if now.Sub(e.LastUsed) > t.cfg.Hold {
			delete(t.entries, k)
		}
	}
	for len(t.entries) > t.cfg.MaxEntries {
		var oldestKey string
		var oldest time.Time
		for k, e := range t.entries {
			if oldestKey == "" || e.LastUsed.Before(oldest) {
				oldestKey, oldest = k, e.LastUsed
			}
		}
		if oldestKey == "" {
			return
		}
		delete(t.entries, oldestKey)
	}
}

// SessionKey derives an affinity key for a request.
//
// Priority order:
//  1. An explicit session header, when the client sends one.
//  2. A client supplied conversation identifier in the body prefix.
//  3. A fingerprint over client address, user agent and body prefix.
//
// The fingerprint lets affinity work without any client cooperation, which
// matters for API clients that cannot send custom headers.
func SessionKey(r *http.Request, bodyPrefix []byte, clientIP string) string {
	if v := firstHeader(r,
		"X-Session-Id",
		"X-Conversation-Id",
		"X-Request-Session",
	); v != "" {
		return "sid:" + v
	}

	if ip := normalizeIP(clientIP); ip != "" {
		h := sha256.New()
		h.Write([]byte(ip))
		h.Write([]byte{0})
		h.Write([]byte(strings.ToLower(r.UserAgent())))
		h.Write([]byte{0})
		h.Write(trimBodyFingerprint(bodyPrefix))
		return "fp:" + hex.EncodeToString(h.Sum(nil))[:32]
	}
	return ""
}

func firstHeader(r *http.Request, names ...string) string {
	for _, n := range names {
		if v := strings.TrimSpace(r.Header.Get(n)); v != "" {
			return v
		}
	}
	return ""
}

func normalizeIP(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(raw); err == nil {
		raw = host
	}
	if ip := net.ParseIP(raw); ip != nil {
		return ip.String()
	}
	return raw
}

// trimBodyFingerprint bounds how much body participates in the fingerprint so
// the cost stays constant for large payloads.
const fingerprintBytes = 512

func trimBodyFingerprint(b []byte) []byte {
	if len(b) > fingerprintBytes {
		return b[:fingerprintBytes]
	}
	return b
}
