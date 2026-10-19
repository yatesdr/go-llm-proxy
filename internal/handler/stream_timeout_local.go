package handler

import (
	"context"
	"net/http"
	"strings"

	"go-llm-proxy/internal/config"
)

// TODO(merge): this file mirrors the helpers the `connect` branch adds to
// stream_timeout.go. Delete it when that branch is merged.

// armUntilHeaders returns a context cancelled after the model's timeout
// (default 300 s) unless disarm is called first. Callers build the upstream
// request with ctx, call client.Do, and disarm once a streaming response has
// started (disarmIfStream): the timeout then bounds connecting and waiting
// for headers, never a live stream.
func armUntilHeaders(parent context.Context, model *config.ModelConfig) (context.Context, context.CancelFunc, func()) {
	seconds := model.Timeout
	if seconds <= 0 {
		seconds = 300
	}
	return requestTimeout(parent, seconds)
}

// disarmIfStream disarms the timeout for a 2xx text/event-stream response.
func disarmIfStream(resp *http.Response, disarm func()) {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 &&
		strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		disarm()
	}
}
