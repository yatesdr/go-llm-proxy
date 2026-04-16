package handler

import (
	"crypto/hmac"
	"crypto/sha256"
	"net"
	"net/http"

	"go-llm-proxy/internal/ratelimit"
)

// constantTimeEqual compares two strings without leaking length or content
// via timing. Both sides are hashed first so the compare is always over
// equal-length inputs.
func constantTimeEqual(a, b string) bool {
	ah := sha256.Sum256([]byte(a))
	bh := sha256.Sum256([]byte(b))
	return hmac.Equal(ah[:], bh[:])
}

// requestIsTLS reports whether the request arrived over TLS. We trust r.TLS
// directly, and X-Forwarded-Proto only when the request came from a
// configured trusted proxy — otherwise any client could spoof the header to
// trick us into setting Secure cookies (or, worse, not setting them).
func requestIsTLS(r *http.Request, rl *ratelimit.RateLimiter) bool {
	if r.TLS != nil {
		return true
	}
	if rl == nil {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if rl.IsTrustedProxy(host) && r.Header.Get("X-Forwarded-Proto") == "https" {
		return true
	}
	return false
}
