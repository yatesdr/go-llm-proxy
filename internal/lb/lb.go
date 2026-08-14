// Package lb selects which backend serves each request for models backed by
// a pool. Selection is sticky: requests carrying the same conversation prefix
// hash to the same backend so upstream KV/prefix caches stay hot, while new
// sessions spread across the pool.
package lb

import (
	"sync"

	"go-llm-proxy/internal/config"
)

// backendState tracks live per-backend counters, keyed by backend URL and
// shared across every model/pool that references the URL.
type backendState struct {
	inflight int64
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
