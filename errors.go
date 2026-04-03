package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"runtime/debug"
)

// setSecurityHeaders applies standard security headers to all responses.
func setSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Cache-Control", "no-store")
}

// writeError sends an OpenAI-compatible error response.
func writeError(w http.ResponseWriter, status int, message string) {
	setSecurityHeaders(w)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "invalid_request_error",
			"code":    http.StatusText(status),
		},
	})
}

// writeAnthropicError sends an Anthropic-compatible error response.
// Claude Code expects this format for all Messages API errors.
func writeAnthropicError(w http.ResponseWriter, status int, errType, message string) {
	setSecurityHeaders(w)
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
				writeError(w, http.StatusInternalServerError, "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}
