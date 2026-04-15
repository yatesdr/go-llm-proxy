package httputil

import (
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"time"
)

// SetSecurityHeaders applies standard security headers to all responses.
func SetSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Cache-Control", "no-store")
}

// WriteError sends an OpenAI-compatible error response.
func WriteError(w http.ResponseWriter, status int, message string) {
	SetSecurityHeaders(w)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errorTypeForStatus(status),
			"code":    http.StatusText(status),
		},
	})
}

// errorTypeForStatus maps HTTP status codes to OpenAI-compatible error types.
func errorTypeForStatus(status int) string {
	switch {
	case status == http.StatusUnauthorized:
		return "authentication_error"
	case status == http.StatusForbidden:
		return "permission_error"
	case status == http.StatusNotFound:
		return "not_found_error"
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status >= 500:
		return "server_error"
	default:
		return "invalid_request_error"
	}
}

// WriteAnthropicError sends an Anthropic-compatible error response.
// Claude Code expects this format for all Messages API errors.
func WriteAnthropicError(w http.ResponseWriter, status int, errType, message string) {
	SetSecurityHeaders(w)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    errType,
			"message": message,
		},
	})
}

// RecoveryMiddleware catches panics in handlers and returns a generic 500 error.
// The stack trace is logged server-side but never exposed to the client.
func RecoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				slog.Error("panic recovered",
					"error", err,
					"method", r.Method,
					"path", r.URL.Path,
					"stack", string(debug.Stack()),
				)
				WriteError(w, http.StatusInternalServerError, "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// NewHTTPClient returns the standard HTTP client used for upstream calls.
//
// Three properties matter:
//
//  1. **No redirect following.** A compromised or misconfigured backend
//     could 3xx the proxy into an internal address; we refuse redirects
//     outright and let the caller decide.
//
//  2. **Transport-level timeouts at every phase.** Go's default transport
//     has no dial, TLS handshake, or response-header timeout, only an
//     overall request timeout (which callers set via context). A
//     slow-loris upstream that accepts the TCP connection then stalls
//     headers ties up a goroutine for the full context timeout (up to
//     300s) per in-flight request; a few parallel slow-loris targets can
//     exhaust the server. The values here bound every phase explicitly.
//
//  3. **Bounded idle-connection pool.** Prevents unbounded growth of
//     keepalive sockets against a single upstream.
//
// Per-request context timeouts still apply on top for the total-request
// bound; these transport settings just stop the connection from hanging
// forever if the upstream misbehaves at a specific phase.
func NewHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			IdleConnTimeout:       90 * time.Second,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   10,
			// ForceAttemptHTTP2 is on by default since Go 1.13; explicit here
			// for visibility — we do want HTTP/2 for streaming backends.
			ForceAttemptHTTP2: true,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
