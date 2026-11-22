package ratelimit

import "sync"

// Concurrency caps how many requests each caller may have in flight at once.
// Unlike Window it counts requests that are still running, not requests
// started per period, so one long stream holds its slot until it ends.
type Concurrency struct {
	mu     sync.Mutex
	active map[string]int
}

// NewConcurrency returns an empty concurrency limiter.
func NewConcurrency() *Concurrency {
	return &Concurrency{active: make(map[string]int)}
}

// Acquire takes a slot for id when fewer than limit are in use. On success
// the caller must call release once, when the request finishes. limit <= 0
// means no limit: nothing is tracked and Acquire always succeeds.
func (c *Concurrency) Acquire(id string, limit int) (release func(), ok bool) {
	if limit <= 0 {
		return func() {}, true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active[id] >= limit {
		return nil, false
	}
	c.active[id]++
	var once sync.Once
	return func() { once.Do(func() { c.release(id) }) }, true
}

func (c *Concurrency) release(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active[id] <= 1 {
		delete(c.active, id) // keep the map to callers with requests in flight
		return
	}
	c.active[id]--
}

// Active reports how many tracked requests id has in flight.
func (c *Concurrency) Active(id string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.active[id]
}
