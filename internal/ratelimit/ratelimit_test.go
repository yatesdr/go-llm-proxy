package ratelimit

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// newTestRateLimiter creates a RateLimiter with short intervals for testing.
func newTestRateLimiter(trustedProxies []string) *RateLimiter {
	rl := NewRateLimiter(trustedProxies)
	// Override decay to a short interval for tests that need it.
	rl.decayInterval = 10 * time.Millisecond
	return rl
}

func TestCheck_NewIP(t *testing.T) {
	rl := NewRateLimiter(nil)
	defer rl.Close()

	if !rl.Check("1.2.3.4") {
		t.Fatal("new IP should be allowed")
	}
}

func TestCheck_BelowThrottle(t *testing.T) {
	rl := NewRateLimiter(nil)
	defer rl.Close()

	// Record failures below throttle threshold (default 3).
	rl.RecordFailure("1.2.3.4")
	rl.RecordFailure("1.2.3.4")

	if !rl.Check("1.2.3.4") {
		t.Fatal("IP with 2 failures should be allowed")
	}
}

func TestCheck_AtThrottle(t *testing.T) {
	rl := NewRateLimiter(nil)
	defer rl.Close()

	// 3 failures = throttleAfter, delay is 1s which is < maxThrottleDelay (2s).
	for i := 0; i < 3; i++ {
		rl.RecordFailure("1.2.3.4")
	}

	if !rl.Check("1.2.3.4") {
		t.Fatal("IP at throttle threshold should still be allowed (delay within tolerance)")
	}
}

func TestCheck_AtThrottlePlusOne(t *testing.T) {
	rl := NewRateLimiter(nil)
	defer rl.Close()

	// 4 failures: exponent=1, delay=2s which equals maxThrottleDelay — still allowed.
	for i := 0; i < 4; i++ {
		rl.RecordFailure("1.2.3.4")
	}

	if !rl.Check("1.2.3.4") {
		t.Fatal("IP with 4 failures should still be allowed (delay == maxThrottleDelay)")
	}
}

func TestCheck_Rejected(t *testing.T) {
	rl := NewRateLimiter(nil)
	defer rl.Close()

	// 5 failures: exponent=2, delay=4s which exceeds maxThrottleDelay (2s).
	for i := 0; i < 5; i++ {
		rl.RecordFailure("1.2.3.4")
	}

	if rl.Check("1.2.3.4") {
		t.Fatal("IP with 5 failures should be rejected")
	}
}

func TestCheck_Decay(t *testing.T) {
	rl := newTestRateLimiter(nil)
	defer rl.Close()

	// 5 failures = rejected.
	for i := 0; i < 5; i++ {
		rl.RecordFailure("1.2.3.4")
	}

	if rl.Check("1.2.3.4") {
		t.Fatal("should be rejected before decay")
	}

	// Wait for enough decay intervals to reduce failures below rejection threshold.
	// Need to decay from 5 to 4 (1 decay), at 10ms per decay.
	time.Sleep(15 * time.Millisecond)

	if !rl.Check("1.2.3.4") {
		t.Fatal("should be allowed after decay reduces failures")
	}
}

func TestCheck_FullDecay(t *testing.T) {
	rl := newTestRateLimiter(nil)
	defer rl.Close()

	rl.RecordFailure("1.2.3.4")
	rl.RecordFailure("1.2.3.4")

	// Wait for full decay (2 failures * 10ms = 20ms).
	time.Sleep(30 * time.Millisecond)

	if !rl.Check("1.2.3.4") {
		t.Fatal("should be allowed after full decay")
	}

	// Verify the record was cleaned up.
	rl.mu.Lock()
	_, exists := rl.ips["1.2.3.4"]
	rl.mu.Unlock()
	if exists {
		t.Fatal("IP record should be deleted after full decay")
	}
}

func TestCheck_IndependentIPs(t *testing.T) {
	rl := NewRateLimiter(nil)
	defer rl.Close()

	for i := 0; i < 5; i++ {
		rl.RecordFailure("1.2.3.4")
	}

	if rl.Check("1.2.3.4") {
		t.Fatal("1.2.3.4 should be rejected")
	}
	if !rl.Check("5.6.7.8") {
		t.Fatal("5.6.7.8 should be allowed (different IP)")
	}
}

func TestClose_StopsCleanup(t *testing.T) {
	rl := NewRateLimiter(nil)
	rl.Close()
	// Just verifying Close() doesn't panic or deadlock.
}

// TestCapacityEviction_EvictsOldestAndStaysBounded exercises the LRU cap
// path that used to be O(n) and DoSable. With 100K concurrent tracked IPs,
// pushing a 100,001st must evict the oldest in constant time and keep the
// total bounded.
func TestCapacityEviction_EvictsOldestAndStaysBounded(t *testing.T) {
	rl := NewRateLimiter(nil)
	defer rl.Close()

	// Fill the tracker to capacity. Each RecordFailure uses a unique IP so
	// no entries merge.
	for i := 0; i < maxTrackedIPs; i++ {
		rl.RecordFailure(fmt.Sprintf("10.%d.%d.%d", (i>>16)&0xff, (i>>8)&0xff, i&0xff))
	}

	rl.mu.Lock()
	atCap := len(rl.ips)
	rl.mu.Unlock()
	if atCap != maxTrackedIPs {
		t.Fatalf("setup: expected %d entries, got %d", maxTrackedIPs, atCap)
	}

	// The oldest entry inserted is the one we expect to be evicted on
	// the next new-IP push.
	oldestIP := "10.0.0.0"

	// Push one more new IP — triggers capacity eviction.
	rl.RecordFailure("192.0.2.1")

	rl.mu.Lock()
	total := len(rl.ips)
	_, oldestStillThere := rl.ips["10.0.0.0"]
	_, newerThere := rl.ips["192.0.2.1"]
	rl.mu.Unlock()

	if total > maxTrackedIPs {
		t.Errorf("tracker exceeded cap: %d", total)
	}
	if oldestStillThere {
		t.Errorf("oldest IP %q should have been evicted", oldestIP)
	}
	if !newerThere {
		t.Errorf("new IP should be present")
	}
}

// TestLRUOrder_MoveToBackOnRepeatFailure confirms that repeated failures
// from the same IP move it to the back of the LRU list, so a single
// chatty attacker doesn't get evicted while idle victims stay tracked.
func TestLRUOrder_MoveToBackOnRepeatFailure(t *testing.T) {
	rl := NewRateLimiter(nil)
	defer rl.Close()

	rl.RecordFailure("1.1.1.1")
	rl.RecordFailure("2.2.2.2")
	rl.RecordFailure("3.3.3.3")
	// Touch 1.1.1.1 again — it should now be the *most* recent, not oldest.
	rl.RecordFailure("1.1.1.1")

	rl.mu.Lock()
	front := rl.lru.Front().Value.(*ipRecord)
	back := rl.lru.Back().Value.(*ipRecord)
	rl.mu.Unlock()

	if front.ip != "2.2.2.2" {
		t.Errorf("front of LRU should be 2.2.2.2 after touch, got %s", front.ip)
	}
	if back.ip != "1.1.1.1" {
		t.Errorf("back of LRU should be 1.1.1.1 after touch, got %s", back.ip)
	}
}

func TestClientIP_DirectConnection(t *testing.T) {
	rl := NewRateLimiter(nil)
	defer rl.Close()

	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.1:12345"

	if got := ClientIP(rl, r); got != "10.0.0.1" {
		t.Fatalf("expected 10.0.0.1, got: %q", got)
	}
}

func TestClientIP_TrustedProxy_XRealIP(t *testing.T) {
	rl := NewRateLimiter([]string{"127.0.0.1"})
	defer rl.Close()

	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "127.0.0.1:12345"
	r.Header.Set("X-Real-IP", "203.0.113.50")

	if got := ClientIP(rl, r); got != "203.0.113.50" {
		t.Fatalf("expected 203.0.113.50, got: %q", got)
	}
}

func TestClientIP_TrustedProxy_XForwardedFor(t *testing.T) {
	rl := NewRateLimiter([]string{"127.0.0.1"})
	defer rl.Close()

	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "127.0.0.1:12345"
	r.Header.Set("X-Forwarded-For", "203.0.113.50, 10.0.0.1")

	if got := ClientIP(rl, r); got != "203.0.113.50" {
		t.Fatalf("expected 203.0.113.50 (first in chain), got: %q", got)
	}
}

func TestClientIP_UntrustedProxy_IgnoresHeaders(t *testing.T) {
	rl := NewRateLimiter([]string{"127.0.0.1"})
	defer rl.Close()

	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.1:12345"
	r.Header.Set("X-Real-IP", "spoofed")

	if got := ClientIP(rl, r); got != "10.0.0.1" {
		t.Fatalf("expected direct IP 10.0.0.1 (untrusted proxy), got: %q", got)
	}
}

func TestClientIP_NoTrustedProxies(t *testing.T) {
	rl := NewRateLimiter(nil)
	defer rl.Close()

	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.1:12345"
	r.Header.Set("X-Real-IP", "spoofed")

	if got := ClientIP(rl, r); got != "10.0.0.1" {
		t.Fatalf("expected direct IP when no proxies configured, got: %q", got)
	}
}

func TestClientIP_TrustedCIDR(t *testing.T) {
	rl := NewRateLimiter([]string{"172.17.0.0/16"})
	defer rl.Close()

	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "172.17.0.1:12345"
	r.Header.Set("X-Real-IP", "203.0.113.50")

	if got := ClientIP(rl, r); got != "203.0.113.50" {
		t.Fatalf("expected 203.0.113.50 from CIDR-trusted proxy, got: %q", got)
	}
}

func TestRateLimitMiddleware_AllowsCleanIP(t *testing.T) {
	rl := NewRateLimiter(nil)
	defer rl.Close()

	called := false
	handler := RateLimitMiddleware(rl, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "1.2.3.4:12345"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if !called {
		t.Fatal("handler should be called for clean IP")
	}
}

func TestRateLimitMiddleware_Blocks429(t *testing.T) {
	rl := NewRateLimiter(nil)
	defer rl.Close()

	for i := 0; i < 5; i++ {
		rl.RecordFailure("1.2.3.4")
	}

	handler := RateLimitMiddleware(rl, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called for blocked IP")
	}))

	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "1.2.3.4:12345"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got: %d", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("expected Retry-After header")
	}
}

func TestRateLimitMiddleware_RecordsAuthFailure(t *testing.T) {
	rl := NewRateLimiter(nil)
	defer rl.Close()

	handler := RateLimitMiddleware(rl, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))

	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "1.2.3.4:12345"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	rl.mu.Lock()
	rec := rl.ips["1.2.3.4"]
	rl.mu.Unlock()

	if rec == nil || rec.failures != 1 {
		t.Fatalf("expected 1 failure recorded, got: %v", rec)
	}
}

func TestRateLimitMiddleware_DoesNotRecordSuccess(t *testing.T) {
	rl := NewRateLimiter(nil)
	defer rl.Close()

	handler := RateLimitMiddleware(rl, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "1.2.3.4:12345"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	rl.mu.Lock()
	_, exists := rl.ips["1.2.3.4"]
	rl.mu.Unlock()

	if exists {
		t.Fatal("successful request should not create a failure record")
	}
}
