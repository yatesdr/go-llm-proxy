package lb

import (
	"fmt"
	"testing"

	"go-llm-proxy/internal/config"
)

func mkBackends(n int, prefix string) []config.BackendConfig {
	out := make([]config.BackendConfig, n)
	for i := range out {
		out[i] = config.BackendConfig{URL: fmt.Sprintf("http://%s-%d:8000", prefix, i), Weight: 1}
	}
	return out
}

func TestSelectBackend_StickyPerKey(t *testing.T) {
	backends := mkBackends(3, "sticky")
	first := selectBackend(backends, 12345)
	for i := 0; i < 50; i++ {
		if got := selectBackend(backends, 12345); got.URL != first.URL {
			t.Fatalf("selection not sticky: %q vs %q", got.URL, first.URL)
		}
	}
}

func TestSelectBackend_SpreadsAcrossKeys(t *testing.T) {
	backends := mkBackends(3, "spread")
	seen := map[string]int{}
	for key := uint64(1); key <= 300; key++ {
		seen[selectBackend(backends, key).URL]++
	}
	for _, b := range backends {
		if seen[b.URL] == 0 {
			t.Errorf("backend %s never selected across 300 keys", b.URL)
		}
	}
}

func TestSelectBackend_MinimalRemapOnGrowth(t *testing.T) {
	three := mkBackends(3, "grow")
	four := append(mkBackends(3, "grow"), config.BackendConfig{URL: "http://grow-3:8000", Weight: 1})
	moved := 0
	const keys = 1000
	for key := uint64(1); key <= keys; key++ {
		if selectBackend(three, key).URL != selectBackend(four, key).URL {
			moved++
		}
	}
	// HRW should remap roughly 1/4 of keys when growing 3 → 4. Allow slack.
	if moved > keys/2 {
		t.Errorf("too many keys remapped on growth: %d of %d", moved, keys)
	}
	if moved == 0 {
		t.Error("expected some keys to remap to the new backend")
	}
}

func TestSelectBackend_BoundedLoadSpill(t *testing.T) {
	backends := []config.BackendConfig{
		{URL: "http://spill-0:8000", Weight: 1, MaxInflight: 2},
		{URL: "http://spill-1:8000", Weight: 1, MaxInflight: 2},
	}
	key := uint64(999)
	winner := selectBackend(backends, key)
	other := backends[0]
	if other.URL == winner.URL {
		other = backends[1]
	}

	// Saturate the winner: next selection must spill to the other backend.
	mu.Lock()
	state(winner.URL).inflight = 2
	mu.Unlock()
	if got := selectBackend(backends, key); got.URL != other.URL {
		t.Errorf("expected spill to %s, got %s", other.URL, got.URL)
	}

	// Both saturated: affinity wins again (no better option).
	mu.Lock()
	state(other.URL).inflight = 2
	mu.Unlock()
	if got := selectBackend(backends, key); got.URL != winner.URL {
		t.Errorf("expected winner under full saturation, got %s", got.URL)
	}

	mu.Lock()
	state(winner.URL).inflight = 0
	state(other.URL).inflight = 0
	mu.Unlock()
}

func TestSelectBackend_BreakerSkipsFailingBackend(t *testing.T) {
	backends := mkBackends(2, "breaker")
	key := uint64(4242)
	winner := selectBackend(backends, key)
	other := backends[0]
	if other.URL == winner.URL {
		other = backends[1]
	}

	for i := 0; i < breakerThreshold; i++ {
		RecordOutcome(winner.URL, false)
	}
	if got := selectBackend(backends, key); got.URL != other.URL {
		t.Errorf("expected breaker to divert to %s, got %s", other.URL, got.URL)
	}

	// Success closes the breaker; affinity returns to the winner.
	RecordOutcome(winner.URL, true)
	if got := selectBackend(backends, key); got.URL != winner.URL {
		t.Errorf("expected return to winner after recovery, got %s", got.URL)
	}
}

func TestSelectBackend_ProbeDownExcluded(t *testing.T) {
	backends := mkBackends(2, "probe")
	key := uint64(7)
	winner := selectBackend(backends, key)
	other := backends[0]
	if other.URL == winner.URL {
		other = backends[1]
	}

	SetProbeHealth(winner.URL, false)
	if got := selectBackend(backends, key); got.URL != other.URL {
		t.Errorf("expected probe-down winner skipped, got %s", got.URL)
	}
	// All down → top-ranked candidate anyway (request becomes the probe).
	SetProbeHealth(other.URL, false)
	if got := selectBackend(backends, key); got.URL != winner.URL {
		t.Errorf("expected winner when all probe-down, got %s", got.URL)
	}
	SetProbeHealth(winner.URL, true)
	SetProbeHealth(other.URL, true)
}

func TestSelectBackend_NoAffinityPicksLeastLoaded(t *testing.T) {
	backends := mkBackends(2, "load")
	mu.Lock()
	state(backends[0].URL).inflight = 5
	state(backends[1].URL).inflight = 1
	mu.Unlock()
	if got := selectBackend(backends, 0); got.URL != backends[1].URL {
		t.Errorf("expected least-loaded backend, got %s", got.URL)
	}
	mu.Lock()
	state(backends[0].URL).inflight = 0
	state(backends[1].URL).inflight = 0
	mu.Unlock()
}

func TestSelectBackend_WeightBiasesShare(t *testing.T) {
	backends := []config.BackendConfig{
		{URL: "http://weight-0:8000", Weight: 3},
		{URL: "http://weight-1:8000", Weight: 1},
	}
	seen := map[string]int{}
	for key := uint64(1); key <= 2000; key++ {
		seen[selectBackend(backends, key).URL]++
	}
	heavy := seen["http://weight-0:8000"]
	if heavy < 1200 || heavy > 1800 {
		t.Errorf("weight-3 backend got %d of 2000 keys, expected ~1500", heavy)
	}
}
