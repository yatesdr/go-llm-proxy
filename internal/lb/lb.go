// Package lb selects which backend serves each request for models backed by
// a pool. Selection is sticky: requests carrying the same conversation prefix
// hash to the same backend so upstream KV/prefix caches stay hot, while new
// sessions spread across the pool.
package lb

import (
	"log/slog"
	"sync"
	"time"

	"go-llm-proxy/internal/config"
)

// Circuit-breaker tuning: a backend is skipped for breakerOpenFor after
// breakerThreshold consecutive failed requests. When the window elapses the
// backend becomes eligible again (half-open); another failure re-opens it,
// a success closes it.
const (
	breakerThreshold = 3
	breakerOpenFor   = 30 * time.Second
)

// backendState tracks live per-backend counters, keyed by backend URL and
// shared across every model/pool that references the URL.
type backendState struct {
	inflight    int64
	consecFails int
	openUntil   time.Time
	probeDown   bool // periodic health probe marked this backend offline
}

// usable reports whether selection should consider this backend. Caller
// holds mu.
func (s *backendState) usable(now time.Time) bool {
	if s.probeDown {
		return false
	}
	return s.consecFails < breakerThreshold || now.After(s.openUntil)
}

// breakerLabel returns the human-readable breaker state. Caller holds mu.
func (s *backendState) breakerLabel(now time.Time) string {
	if s.consecFails < breakerThreshold {
		return "closed"
	}
	if now.After(s.openUntil) {
		return "half-open"
	}
	return "open"
}

var (
	mu     sync.Mutex
	states = make(map[string]*backendState)
)

func state(url string) *backendState {
	if s, ok := states[url]; ok {
		return s
	}
	s := &backendState{}
	states[url] = s
	return s
}

// Inflight returns the current in-flight request count for a backend URL.
func Inflight(url string) int64 {
	mu.Lock()
	defer mu.Unlock()
	return state(url).inflight
}

// RecordOutcome feeds a request outcome into the backend's circuit breaker.
// Call with success=false only for backend-caused failures (transport errors,
// 5xx, auth rejection) — client-side 4xx must not trip the breaker.
func RecordOutcome(url string, success bool) {
	if url == "" {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	s := state(url)
	if success {
		if s.consecFails >= breakerThreshold {
			slog.Info("lb: backend recovered, closing breaker", "backend", url)
		}
		s.consecFails = 0
		return
	}
	s.consecFails++
	if s.consecFails >= breakerThreshold {
		if s.consecFails == breakerThreshold || time.Now().After(s.openUntil) {
			slog.Warn("lb: backend breaker open", "backend", url, "consecutive_failures", s.consecFails)
		}
		s.openUntil = time.Now().Add(breakerOpenFor)
	}
}

// SetProbeHealth records a periodic health-probe result for a backend URL.
// Probe-down backends are excluded from selection until a probe succeeds.
func SetProbeHealth(url string, online bool) {
	mu.Lock()
	defer mu.Unlock()
	state(url).probeDown = !online
}

// BackendStatus is a point-in-time view of one backend's live state.
type BackendStatus struct {
	URL         string `json:"url"`
	Inflight    int64  `json:"inflight"`
	Breaker     string `json:"breaker"` // closed | half-open | open
	ConsecFails int    `json:"consecutive_failures"`
	ProbeDown   bool   `json:"probe_down"`
}

// Status returns the live state for a backend URL.
func Status(url string) BackendStatus {
	now := time.Now()
	mu.Lock()
	defer mu.Unlock()
	s := state(url)
	return BackendStatus{
		URL:         url,
		Inflight:    s.inflight,
		Breaker:     s.breakerLabel(now),
		ConsecFails: s.consecFails,
		ProbeDown:   s.probeDown,
	}
}

// ResolveModel picks a backend for model m and returns a request-scoped model
// view whose Backend/APIKey point at the chosen backend, plus a release
// function the caller must defer — it decrements the backend's in-flight
// count when the request (including any streaming body) finishes.
//
// body is the raw request body; its stable conversation prefix is the
// affinity key. Handlers pass the body as received (before pipeline
// rewrites) so retries and follow-up turns hash identically.
//
// Single-backend models pass through unchanged apart from accounting.
func ResolveModel(cfg *config.Config, m *config.ModelConfig, body []byte) (*config.ModelConfig, func()) {
	backends := cfg.EffectiveBackends(m)
	chosen := pick(backends, AffinityKey(body))

	mu.Lock()
	st := state(chosen.URL)
	st.inflight++
	mu.Unlock()

	var once sync.Once
	release := func() {
		once.Do(func() {
			mu.Lock()
			st.inflight--
			mu.Unlock()
		})
	}

	if chosen.URL == m.Backend && chosen.APIKey == m.APIKey {
		return m, release
	}
	view := *m
	view.Backend = chosen.URL
	view.APIKey = chosen.APIKey
	return &view, release
}

// ResolveAlternate picks a backend other than excludeURL for a one-shot
// failover retry after the first choice failed. Returns (nil, nil) when the
// model has no other enabled backend. The failed backend's cache is lost
// either way, so the alternate is chosen by load, not affinity.
func ResolveAlternate(cfg *config.Config, m *config.ModelConfig, excludeURL string) (*config.ModelConfig, func()) {
	var remaining []config.BackendConfig
	for _, b := range cfg.EffectiveBackends(m) {
		if b.URL != excludeURL && !b.Disabled {
			remaining = append(remaining, b)
		}
	}
	if len(remaining) == 0 {
		return nil, nil
	}
	chosen := pick(remaining, 0)

	mu.Lock()
	st := state(chosen.URL)
	st.inflight++
	mu.Unlock()

	var once sync.Once
	release := func() {
		once.Do(func() {
			mu.Lock()
			st.inflight--
			mu.Unlock()
		})
	}

	view := *m
	view.Backend = chosen.URL
	view.APIKey = chosen.APIKey
	return &view, release
}

// pick chooses one backend from the list. Disabled (drained) backends are
// excluded unless every backend is drained, in which case the first entry is
// returned so the request fails against a real upstream rather than nothing.
func pick(backends []config.BackendConfig, affinity uint64) config.BackendConfig {
	candidates := backends[:0:0]
	for _, b := range backends {
		if !b.Disabled {
			candidates = append(candidates, b)
		}
	}
	if len(candidates) == 0 {
		candidates = backends
	}
	if len(candidates) == 1 {
		return candidates[0]
	}
	return selectBackend(candidates, affinity)
}
