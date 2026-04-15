package handler

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestWaitForSearch_Completes(t *testing.T) {
	done := make(chan struct{})
	close(done) // already done when we call

	var keepaliveCalls int32
	completed := waitForSearchOrDisconnect(context.Background(), done,
		func() { atomic.AddInt32(&keepaliveCalls, 1) },
		50*time.Millisecond, 100*time.Millisecond, "test")
	if !completed {
		t.Fatal("expected completed=true for pre-closed searchDone")
	}
	if atomic.LoadInt32(&keepaliveCalls) != 0 {
		t.Errorf("no keepalives expected on fast path, got %d", keepaliveCalls)
	}
}

func TestWaitForSearch_EmitsKeepalives(t *testing.T) {
	done := make(chan struct{})
	var keepaliveCalls int32

	// Close `done` after a few keepalive ticks.
	go func() {
		time.Sleep(75 * time.Millisecond)
		close(done)
	}()

	completed := waitForSearchOrDisconnect(context.Background(), done,
		func() { atomic.AddInt32(&keepaliveCalls, 1) },
		20*time.Millisecond, 100*time.Millisecond, "test")
	if !completed {
		t.Fatal("expected completed=true")
	}
	if got := atomic.LoadInt32(&keepaliveCalls); got < 1 {
		t.Errorf("expected at least 1 keepalive, got %d", got)
	}
}

func TestWaitForSearch_DisconnectWaitsForGoroutine(t *testing.T) {
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())

	// Simulate a goroutine that takes 30ms to respond to ctx cancellation.
	go func() {
		<-ctx.Done()
		time.Sleep(30 * time.Millisecond)
		close(done)
	}()

	// Cancel after a brief delay.
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	completed := waitForSearchOrDisconnect(ctx, done, nil,
		1*time.Second, 500*time.Millisecond, "test")
	elapsed := time.Since(start)

	if completed {
		t.Error("expected completed=false on disconnect")
	}
	// Should wait for the goroutine (~10ms cancel + ~30ms goroutine = ~40ms).
	// Must NOT time out at 500ms grace period.
	if elapsed > 200*time.Millisecond {
		t.Errorf("wait took too long: %v (should have observed goroutine exit near 40ms)", elapsed)
	}
	if elapsed < 30*time.Millisecond {
		t.Errorf("wait returned too quickly: %v (should have waited for goroutine)", elapsed)
	}
	// After function returns, `done` must have been closed — confirms the
	// goroutine exited before we returned, which is the whole point (no race
	// when the caller reads the goroutine's output vars).
	select {
	case <-done:
		// expected
	default:
		t.Error("searchDone should have been closed by the time wait returns")
	}
}

func TestWaitForSearch_DisconnectRespectsGraceTimeout(t *testing.T) {
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // immediately cancelled

	// Goroutine never closes — simulates a misbehaving search that ignores ctx.
	defer close(done) // cleanup

	start := time.Now()
	completed := waitForSearchOrDisconnect(ctx, done, nil,
		1*time.Second, 50*time.Millisecond, "test")
	elapsed := time.Since(start)

	if completed {
		t.Error("expected completed=false on disconnect")
	}
	// Must return within grace period + a reasonable buffer.
	if elapsed > 200*time.Millisecond {
		t.Errorf("wait exceeded grace period: %v", elapsed)
	}
}
