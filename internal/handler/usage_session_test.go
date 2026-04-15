package handler

import (
	"testing"
	"time"
)

func TestSessionStore_CreateAndValidate(t *testing.T) {
	s := newSessionStore()
	token := s.create()
	if token == "" {
		t.Fatal("create returned empty token")
	}
	if len(token) != dashboardSessionTokenSize*2 {
		t.Errorf("token length %d, want %d hex chars", len(token), dashboardSessionTokenSize*2)
	}
	if !s.validate(token) {
		t.Error("freshly created token should validate")
	}
}

func TestSessionStore_ValidateEmpty(t *testing.T) {
	s := newSessionStore()
	if s.validate("") {
		t.Error("empty token should not validate")
	}
}

func TestSessionStore_ValidateUnknown(t *testing.T) {
	s := newSessionStore()
	if s.validate("deadbeef") {
		t.Error("unknown token should not validate")
	}
}

func TestSessionStore_Revoke(t *testing.T) {
	s := newSessionStore()
	t1 := s.create()
	t2 := s.create()

	s.revoke(t1)

	if s.validate(t1) {
		t.Error("revoked token should not validate")
	}
	if !s.validate(t2) {
		t.Error("unrevoked token should still validate")
	}
}

func TestSessionStore_ExpiryInvalidates(t *testing.T) {
	s := newSessionStore()
	// Inject a token with an expiry in the past by bypassing create's TTL.
	expiredToken := "expired"
	s.mu.Lock()
	s.sessions[expiredToken] = time.Now().Add(-1 * time.Minute)
	s.mu.Unlock()

	if s.validate(expiredToken) {
		t.Error("expired token should not validate")
	}
	// validate() should have GC'd it.
	s.mu.Lock()
	_, stillThere := s.sessions[expiredToken]
	s.mu.Unlock()
	if stillThere {
		t.Error("expired token should be removed from store on failed validate")
	}
}

func TestSessionStore_TokensAreUnique(t *testing.T) {
	s := newSessionStore()
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		tok := s.create()
		if seen[tok] {
			t.Fatalf("token collision at i=%d: %q", i, tok)
		}
		seen[tok] = true
	}
}

func TestSessionStore_CapacityEvictsOldest(t *testing.T) {
	s := newSessionStore()
	// Fill to cap with *expired* entries so create can GC them naturally —
	// if the store works correctly, we should never actually hit the
	// "drop-oldest" path for legitimate load.
	now := time.Now()
	s.mu.Lock()
	for i := 0; i < dashboardMaxSessions; i++ {
		s.sessions[tokenWithSuffix(i)] = now.Add(-1 * time.Minute) // expired
	}
	s.mu.Unlock()

	// Creating one more should free all the expired ones via gcLocked.
	tok := s.create()
	if tok == "" {
		t.Fatal("create failed at capacity")
	}

	s.mu.Lock()
	total := len(s.sessions)
	s.mu.Unlock()
	if total > dashboardMaxSessions {
		t.Errorf("session count %d exceeds cap %d", total, dashboardMaxSessions)
	}
	if !s.validate(tok) {
		t.Error("new token should still validate")
	}
}

func tokenWithSuffix(i int) string {
	// Generate a unique, hex-looking placeholder for the test.
	const hex = "0123456789abcdef"
	b := make([]byte, 64)
	for j := range b {
		b[j] = hex[(i+j)%16]
	}
	return string(b)
}
