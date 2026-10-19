package handler

import (
	"context"
	"net/http"
	"strings"

	"go-llm-proxy/internal/config"
)

// TODO(merge): this file mirrors the helpers the connect branch adds to
// stream_timeout.go. Delete it when that branch is merged.

// armUntilHeaders returns a context bounded by the model's timeout (default
// 300 s). The caller keeps it armed through the upstream Do call, so a hung
// backend is cut off while response headers are awaited, and disarms it with
// disarmIfStream once a streaming response has begun.
func armUntilHeaders(parent context.Context, model *config.ModelConfig) (context.Context, context.CancelFunc, func()) {
	seconds := model.Timeout
	if seconds <= 0 {
		seconds = 300
	}
	return requestTimeout(parent, seconds)
}

// disarmIfStream disarms the timeout only for a successful (2xx)
// text/event-stream response. Error responses and non-streaming bodies stay
// under the timeout.
func disarmIfStream(resp *http.Response, disarm func()) {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 &&
		strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		disarm()
	}
}
