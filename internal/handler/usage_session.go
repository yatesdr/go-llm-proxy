package handler

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// Dashboard session storage.
//
// The previous design used a deterministic HMAC of the password as the
// cookie value. That created three problems: sessions could not be revoked
// without rotating the password, there was no expiry enforced server-side,
// and an attacker who captured the cookie once retained access forever.
//
// sessionStore replaces that with per-login random tokens backed by a
// server-side map. Tokens are unguessable (256 bits of entropy), have a
// fixed TTL, and can be individually revoked via a /usage/logout endpoint.
// The map is bounded so a flood of logins can't exhaust memory.

const (
	dashboardSessionTTL       = 24 * time.Hour
	dashboardMaxSessions      = 1024 // cap total live sessions
	dashboardSessionTokenSize = 32   // bytes of entropy → 64 hex chars
)

type sessionStore struct {
	mu       sync.Mutex
	sessions map[string]time.Time // token → expiry
}

func newSessionStore() *sessionStore {
	return &sessionStore{sessions: make(map[string]time.Time)}
}

// create issues a new session token valid for dashboardSessionTTL.
// Returns the token. On entropy failure returns "" (caller should 500).
func (s *sessionStore) create() string {
	buf := make([]byte, dashboardSessionTokenSize)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	token := hex.EncodeToString(buf)

	s.mu.Lock()
	defer s.mu.Unlock()

	// Opportunistically drop expired entries; cheap since the map is small.
	s.gcLocked(time.Now())

	// If somehow still at cap after GC, drop the single oldest to make room.
	// This keeps memory bounded under a session-flood attack while not
	// silently rejecting legitimate logins.
	if len(s.sessions) >= dashboardMaxSessions {
		var oldestToken string
		var oldestExp time.Time
		first := true
		for t, exp := range s.sessions {
			if first || exp.Before(oldestExp) {
				oldestToken = t
				oldestExp = exp
				first = false
			}
		}
		delete(s.sessions, oldestToken)
	}

	s.sessions[token] = time.Now().Add(dashboardSessionTTL)
	return token
}

// validate returns true if the token is present and unexpired. On expiry,
// the entry is removed in passing.
func (s *sessionStore) validate(token string) bool {
	if token == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	exp, ok := s.sessions[token]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(s.sessions, token)
		return false
	}
	return true
}

// revoke explicitly removes a session token. Used by logout.
func (s *sessionStore) revoke(token string) {
	if token == "" {
		return
	}
	s.mu.Lock()
	delete(s.sessions, token)
	s.mu.Unlock()
}

// gcLocked removes expired sessions. Must be called with s.mu held.
func (s *sessionStore) gcLocked(now time.Time) {
	for token, exp := range s.sessions {
		if now.After(exp) {
			delete(s.sessions, token)
		}
	}
}
