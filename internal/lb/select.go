package lb

import "go-llm-proxy/internal/config"

// selectBackend chooses among two or more enabled candidates.
//
// Phase 2 placeholder: deterministic first candidate, matching the
// pre-load-balancer behavior (a pooled model's bridged first backend). The
// weighted-rendezvous + bounded-load algorithm replaces this next.
func selectBackend(candidates []config.BackendConfig, affinity uint64) config.BackendConfig {
	_ = affinity
	return candidates[0]
}
