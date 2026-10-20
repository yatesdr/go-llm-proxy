package ratelimit

import (
	"sync"
	"time"
)

// Window allows at most Limit events per key in each fixed Window (for
// example 5 per hour per IP). Unlike RateLimiter it counts every event, not
// only failures, so it suits public endpoints that cost something on
// success, such as sending an email.
type Window struct {
	limit  int
	window time.Duration
	now    func() time.Time

	mu     sync.Mutex
	counts map[string]*windowCount
}

type windowCount struct {
	start time.Time
	n     int
}

// maxWindowKeys caps tracked keys; past it, expired entries are swept and,
// if still full, the table starts over (a burst of distinct keys can't grow
// memory without bound).
const maxWindowKeys = 100000

// NewWindow returns a limiter allowing limit events per key per window.
func NewWindow(limit int, window time.Duration) *Window {
	return &Window{limit: limit, window: window, now: time.Now, counts: make(map[string]*windowCount)}
}

// Allow records one event for key and reports whether it is within the limit.
func (w *Window) Allow(key string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()
	c, ok := w.counts[key]
	if !ok || now.Sub(c.start) >= w.window {
		if !ok && len(w.counts) >= maxWindowKeys {
			w.sweepLocked(now)
		}
		c = &windowCount{start: now}
		w.counts[key] = c
	}
	if c.n >= w.limit {
		return false
	}
	c.n++
	return true
}

func (w *Window) sweepLocked(now time.Time) {
	for k, c := range w.counts {
		if now.Sub(c.start) >= w.window {
			delete(w.counts, k)
		}
	}
	if len(w.counts) >= maxWindowKeys {
		w.counts = make(map[string]*windowCount)
	}
}
