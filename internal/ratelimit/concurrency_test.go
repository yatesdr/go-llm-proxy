package ratelimit

import "testing"

func TestConcurrencyEnforcesLimit(t *testing.T) {
	c := NewConcurrency()
	r1, ok := c.Acquire("a", 2)
	if !ok {
		t.Fatal("first acquire refused")
	}
	r2, ok := c.Acquire("a", 2)
	if !ok {
		t.Fatal("second acquire refused")
	}
	if _, ok := c.Acquire("a", 2); ok {
		t.Fatal("third acquire allowed past limit 2")
	}
	if _, ok := c.Acquire("b", 2); !ok {
		t.Fatal("another id should have its own slots")
	}
	r1()
	r1() // a second release must not free a second slot
	if got := c.Active("a"); got != 1 {
		t.Fatalf("active after one release = %d, want 1", got)
	}
	if _, ok := c.Acquire("a", 2); !ok {
		t.Fatal("acquire refused after a slot was released")
	}
	r2()
}

func TestConcurrencyZeroLimitIsUntracked(t *testing.T) {
	c := NewConcurrency()
	for i := 0; i < 100; i++ {
		if _, ok := c.Acquire("a", 0); !ok {
			t.Fatal("limit 0 must not refuse")
		}
	}
	if got := c.Active("a"); got != 0 {
		t.Fatalf("limit 0 tracked %d requests", got)
	}
}

func TestConcurrencyForgetsIdleIDs(t *testing.T) {
	c := NewConcurrency()
	r, _ := c.Acquire("a", 1)
	r()
	if len(c.active) != 0 {
		t.Fatalf("idle id still in map: %v", c.active)
	}
}
