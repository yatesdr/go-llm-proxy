package httputil

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"runtime/debug"
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

// NewHTTPClient returns an http.Client that refuses to follow redirects.
// All proxy HTTP clients must use this to prevent SSRF via backend redirect responses.
func NewHTTPClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
