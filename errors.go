package main

import (
	"encoding/json"
	"net/http"
)

// writeError sends an OpenAI-compatible error response.
func writeError(w http.ResponseWriter, status int, message string) {
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
