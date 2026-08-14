package lb

import (
	"hash/fnv"
	"math"
	"sort"
	"time"

	"go-llm-proxy/internal/config"
)

// selectBackend chooses among two or more enabled candidates.
//
// Sticky requests (affinity != 0) use weighted rendezvous (highest-random-
// weight) hashing: each backend scores the affinity key independently and the
// ranking is stable across requests and proxy restarts, so a session keeps
// landing on the backend that holds its prefix cache. Adding or removing a
// backend only remaps the sessions that hashed to it.
//
// The ranked order is then walked with two gates:
//   - health: backends whose circuit breaker is open or whose probe marked
//     them down are skipped (unless every candidate is unhealthy, in which
//     case the top-ranked candidate is returned and the request itself
//     becomes the probe);
//   - bounded load: a backend at its max_inflight cap is skipped when a
//     later-ranked candidate has spare capacity — capacity wins over
//     affinity only under pressure.
//
// Requests with no affinity spread by least normalized load (inflight/weight).
func selectBackend(candidates []config.BackendConfig, affinity uint64) config.BackendConfig {
	now := time.Now()

	mu.Lock()
	defer mu.Unlock()

	healthy := make([]config.BackendConfig, 0, len(candidates))
	for _, b := range candidates {
		if state(b.URL).usable(now) {
			healthy = append(healthy, b)
		}
	}
	if len(healthy) == 0 {
		healthy = candidates
	}

	var ranked []config.BackendConfig
	if affinity == 0 {
		ranked = rankByLoad(healthy)
	} else {
		ranked = rankByRendezvous(healthy, affinity)
	}

	// Bounded-load walk: first unsaturated backend in ranked order.
	for _, b := range ranked {
		if b.MaxInflight > 0 && state(b.URL).inflight >= int64(b.MaxInflight) {
			continue
		}
		return b
	}
	// Everything saturated (or unhealthy fallback): stick with the winner.
	return ranked[0]
}

// rankByRendezvous orders backends by weighted HRW score for the given key
// (descending). Weight scales capacity share: score = -weight / ln(h) with
// h ∈ (0,1) derived from hash(url, key).
func rankByRendezvous(backends []config.BackendConfig, key uint64) []config.BackendConfig {
	type scored struct {
		b     config.BackendConfig
		score float64
	}
	scoredList := make([]scored, len(backends))
	for i, b := range backends {
		h := fnv.New64a()
		_, _ = h.Write([]byte(b.URL))
		var kb [8]byte
		for j := 0; j < 8; j++ {
			kb[j] = byte(key >> (8 * j))
		}
		_, _ = h.Write(kb[:])
		// Map hash to (0,1); add 1 to avoid ln(0).
		u := (float64(h.Sum64()>>11) + 1) / float64(1<<53)
		w := float64(b.Weight)
		if w <= 0 {
			w = 1
		}
		scoredList[i] = scored{b, -w / math.Log(u)}
	}
	sort.SliceStable(scoredList, func(i, j int) bool { return scoredList[i].score > scoredList[j].score })
	out := make([]config.BackendConfig, len(scoredList))
	for i, s := range scoredList {
		out[i] = s.b
	}
	return out
}

// rankByLoad orders backends by normalized in-flight load (ascending), for
// requests with no session affinity. Caller holds mu.
func rankByLoad(backends []config.BackendConfig) []config.BackendConfig {
	out := make([]config.BackendConfig, len(backends))
	copy(out, backends)
	load := func(b config.BackendConfig) float64 {
		w := float64(b.Weight)
		if w <= 0 {
			w = 1
		}
		return float64(state(b.URL).inflight) / w
	}
	sort.SliceStable(out, func(i, j int) bool { return load(out[i]) < load(out[j]) })
	return out
}
