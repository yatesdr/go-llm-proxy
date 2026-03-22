package main

import (
	"encoding/json"
	"net/http"
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
