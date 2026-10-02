package entry

import (
	"sync"
	"time"
)

// limiter is a fixed-window per-client request limiter.
//
// A fixed window is intentionally chosen over a token bucket: the goal is to
// stop runaway clients and log spam, not to shape traffic precisely.
type limiter struct {
	mu      sync.Mutex
	perMin  int
	windows map[string]*window
	now     func() time.Time
}

type window struct {
	start time.Time
	count int
}

// newLimiter creates a limiter allowing perMin requests per client per minute.
func newLimiter(perMin int) *limiter {
	if perMin <= 0 {
		return nil
	}
	return &limiter{
		perMin:  perMin,
		windows: map[string]*window{},
		now:     time.Now,
	}
}

// allow reports whether a request from key may proceed.
func (l *limiter) allow(key string) bool {
	if l == nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	w, ok := l.windows[key]
	if !ok || now.Sub(w.start) >= time.Minute {
		l.windows[key] = &window{start: now, count: 1}
		l.sweepLocked(now)
		return true
	}
	if w.count >= l.perMin {
		return false
	}
	w.count++
	return true
}

// sweepLocked drops windows that have expired.
func (l *limiter) sweepLocked(now time.Time) {
	for k, w := range l.windows {
		if now.Sub(w.start) >= 2*time.Minute {
			delete(l.windows, k)
		}
	}
}
