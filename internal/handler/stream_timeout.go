package handler

import (
	"context"
	"net/http"
	"strings"
	"time"

	"go-llm-proxy/internal/config"
)

// defaultModelTimeoutSeconds applies when a model carries no timeout (configs
// loaded through finalizeConfig always have one; hand-built models may not).
const defaultModelTimeoutSeconds = 300

// armUntilHeaders is the one way to bound an upstream model call.
//
//	ctx, cancel, disarm := armUntilHeaders(r.Context(), model)
//	defer cancel()
//	resp, err := client.Do(req.WithContext(ctx))   // or NewRequestWithContext(ctx...)
//	...
//	disarmIfStream(resp, disarm)                   // right after Do returns
//
// The model's timeout starts counting now and stays armed through the wait for
// response headers and, for a non-streaming response, the whole body. It is
// disarmed only for a 2xx text/event-stream (disarmIfStream), because a live
// generation may legitimately outlast any fixed budget.
//
// Do NOT disarm up front just because the request asked for stream:true — a
// hung backend never sends headers, and with no transport-level header
// timeout the armed timer is the only thing that frees the request. If a
// failover re-sends on the same ctx, the timeout budget is shared by both
// attempts.
func armUntilHeaders(parent context.Context, model *config.ModelConfig) (ctx context.Context, cancel context.CancelFunc, disarm func()) {
	seconds := defaultModelTimeoutSeconds
	if model != nil && model.Timeout > 0 {
		seconds = model.Timeout
	}
	return requestTimeout(parent, seconds)
}

// disarmIfStream disarms the model timeout when resp is a successful
// server-sent-event stream, and does nothing otherwise (error replies and
// ordinary JSON bodies stay under the timeout). Safe with a nil resp.
func disarmIfStream(resp *http.Response, disarm func()) {
	if resp == nil || disarm == nil {
		return
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 &&
		strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		disarm()
	}
}

// requestTimeout returns a context for one proxied upstream exchange,
// its cancel func, and a disarm func.
//
// The context is cancelled `seconds` after arming UNLESS disarm is
// called first. Callers disarm the moment they know the exchange is a
// streaming response: model.Timeout is a guard against hung backends,
// but applied to a live stream it becomes a wall-clock cap that severs
// healthy long generations mid-flight — a reasoning model can
// legitimately stream for longer than any fixed budget, and the kill
// manifests downstream as an abrupt connection reset with no error
// event (observed 2026-07-15: every generation exceeding the 300s
// default died at exactly 300s, sending resume-capable clients into a
// re-think/re-issue loop).
//
// A disarmed exchange is still bounded: the shared transport's
// DialContext (10s) / TLSHandshake (10s) cover connecting, and the parent
// context (the client's own connection) cancels the upstream read when the
// client goes away. There is deliberately no transport-level header timeout:
// the wait for response headers is bounded by this timer, so it must still be
// armed when the request is sent. Use armUntilHeaders, which does that.
func requestTimeout(parent context.Context, seconds int) (ctx context.Context, cancel context.CancelFunc, disarm func()) {
	ctx, cancel = context.WithCancel(parent)
	t := time.AfterFunc(time.Duration(seconds)*time.Second, cancel)
	return ctx, cancel, func() { t.Stop() }
}

// statusOf returns resp's status code, or 0 for a nil response (transport
// error), in the form lb.RecordAttempt takes.
func statusOf(resp *http.Response) int {
	if resp == nil {
		return 0
	}
	return resp.StatusCode
}
