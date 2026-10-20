package ratelimit

import (
	"testing"
	"time"
)

func TestWindowLimitsPerKeyAndResets(t *testing.T) {
	now := time.Unix(1000, 0)
	w := NewWindow(2, time.Hour)
	w.now = func() time.Time { return now }

	if !w.Allow("a") || !w.Allow("a") {
		t.Fatal("first two events should pass")
	}
	if w.Allow("a") {
		t.Fatal("third event in the window should be refused")
	}
	if !w.Allow("b") {
		t.Fatal("another key has its own budget")
	}
	now = now.Add(time.Hour)
	if !w.Allow("a") {
		t.Fatal("a new window should allow again")
	}
}
