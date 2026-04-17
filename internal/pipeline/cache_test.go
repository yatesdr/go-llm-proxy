package pipeline

import (
	"testing"
	"time"
)

func TestBoundedCache_StorePermanent(t *testing.T) {
	c := newBoundedCache()
	c.Store("k", "v")
	got, ok := c.Load("k")
	if !ok || got != "v" {
		t.Fatalf("expected hit with value v, got ok=%v val=%q", ok, got)
	}
}

func TestBoundedCache_StoreWithTTL_NotExpired(t *testing.T) {
	c := newBoundedCache()
	c.StoreWithTTL("k", "v", time.Minute)
	got, ok := c.Load("k")
	if !ok || got != "v" {
		t.Fatalf("expected hit before TTL, got ok=%v val=%q", ok, got)
	}
}

func TestBoundedCache_StoreWithTTL_Expired(t *testing.T) {
	c := newBoundedCache()
	c.StoreWithTTL("k", "v", time.Nanosecond)
	time.Sleep(10 * time.Millisecond)
	_, ok := c.Load("k")
	if ok {
		t.Fatalf("expected miss after TTL, got hit")
	}
}

func TestBoundedCache_StoreWithTTL_LazyCleanup(t *testing.T) {
	c := newBoundedCache()
	c.StoreWithTTL("k", "v", time.Nanosecond)
	time.Sleep(10 * time.Millisecond)

	// Load should drop the expired entry.
	_, _ = c.Load("k")

	c.mu.RLock()
	_, stillThere := c.items["k"]
	c.mu.RUnlock()
	if stillThere {
		t.Fatalf("expected expired entry to be lazily removed after Load")
	}
}

func TestBoundedCache_PermanentNotExpiredByTime(t *testing.T) {
	c := newBoundedCache()
	c.Store("k", "v")
	// Simulate time passing (we can't change wall clock, but permanent
	// entries have zero expiresAt so should always hit).
	time.Sleep(5 * time.Millisecond)
	got, ok := c.Load("k")
	if !ok || got != "v" {
		t.Fatalf("permanent entry should not expire, got ok=%v val=%q", ok, got)
	}
}

func TestBoundedCache_Eviction(t *testing.T) {
	c := newBoundedCache()
	// Fill to capacity.
	for i := 0; i < maxCacheEntries; i++ {
		c.Store(keyN(i), "v")
	}
	// Next store should trigger bulk clear + insert.
	c.Store("overflow", "new")
	got, ok := c.Load("overflow")
	if !ok || got != "new" {
		t.Fatalf("expected new entry after eviction, got ok=%v val=%q", ok, got)
	}
	// Older entries gone.
	if _, ok := c.Load(keyN(0)); ok {
		t.Fatalf("expected older entry to be evicted")
	}
}

func TestBoundedCache_Reset(t *testing.T) {
	c := newBoundedCache()
	c.Store("a", "1")
	c.StoreWithTTL("b", "2", time.Minute)
	c.Reset()
	if _, ok := c.Load("a"); ok {
		t.Fatalf("expected a to be gone after Reset")
	}
	if _, ok := c.Load("b"); ok {
		t.Fatalf("expected b to be gone after Reset")
	}
}

func TestBoundedCache_StoreOverwrite(t *testing.T) {
	c := newBoundedCache()
	c.StoreWithTTL("k", "old", time.Nanosecond)
	time.Sleep(5 * time.Millisecond)
	// Overwriting an expired entry with a permanent one should succeed.
	c.Store("k", "new")
	got, ok := c.Load("k")
	if !ok || got != "new" {
		t.Fatalf("expected overwrite to win, got ok=%v val=%q", ok, got)
	}
}

func keyN(n int) string {
	// Tiny helper to generate cache keys for the eviction test without
	// pulling strconv into the import list purely for tests.
	s := ""
	if n == 0 {
		return "k0"
	}
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	return "k" + s
}
