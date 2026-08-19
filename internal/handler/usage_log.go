package handler

import (
	"fmt"
	"time"

	"go-llm-proxy/internal/api"
	"go-llm-proxy/internal/config"
	"go-llm-proxy/internal/lb"
	"go-llm-proxy/internal/usage"
)

// healthRecorder is the process-wide health store, set once at startup via
// SetHealthStore. It lets the usage-logging funnel update backend health from
// real request outcomes without threading the store through every handler.
// nil-safe: if unset (e.g. in tests), health recording is skipped.
var healthRecorder *config.HealthStore

// SetHealthStore registers the health store used by recordBackendHealth.
// Call once during startup, before serving requests.
func SetHealthStore(hs *config.HealthStore) { healthRecorder = hs }

// recordBackendHealth updates a model's health — and the serving backend's
// circuit breaker — from an upstream status code. Only backend-caused
// outcomes move the needle: 2xx marks online; auth/billing (401/402/403) and
// server errors (5xx) mark offline. Other 4xx (bad request, unknown model)
// and 429 rate-limits are client- or transient-side, so they leave the
// existing status untouched to avoid false "offline" flapping.
func recordBackendHealth(model, backend string, statusCode int) {
	success := false
	errMsg := ""
	switch {
	case statusCode >= 200 && statusCode < 300:
		success = true
	case statusCode == 401 || statusCode == 402 || statusCode == 403:
		errMsg = fmt.Sprintf("backend rejected credentials: HTTP %d", statusCode)
	case statusCode >= 500:
		errMsg = fmt.Sprintf("backend error: HTTP %d", statusCode)
	default:
		return // client-side or transient — no signal either way
	}
	lb.RecordOutcome(backend, success)
	if healthRecorder != nil && model != "" {
		healthRecorder.RecordUsage(model, success, errMsg)
	}
}

// Unified usage-logging helpers.
//
// Every handler needs to emit a usage.UsageRecord after a request completes.
// The record's fields are nearly identical regardless of path: a few
// per-request values (timestamp, key, model, endpoint, status, byte counts)
// plus token counts that come from one of two sources — `*api.ChunkUsage`
// for Chat-Completions-shaped backends or `*converseUsage` for Bedrock.
//
// Before this helper there were 10+ hand-rolled record constructions across
// the handlers, which is exactly how fields drift out of sync. Route
// everything through `logUsage` and drift becomes a compile error.

type usageLogInput struct {
	startTime     time.Time
	statusCode    int
	keyName       string
	keyHash       string
	model         string
	aliasFrom     string
	backend       string // chosen backend URL (empty for non-model routes)
	endpoint      string
	requestBytes  int64
	responseBytes int64
	inputTokens   int
	outputTokens  int
	totalTokens   int
}

// logUsage writes a single usage record. Safe to call with ul==nil.
// UsageLogger queues the record to its owned writer so request goroutines do
// not contend directly on SQLite.
func logUsage(ul *usage.UsageLogger, in usageLogInput) {
	// Update backend health from the outcome first — this must run even when
	// usage logging is disabled (ul == nil).
	recordBackendHealth(in.model, in.backend, in.statusCode)
	if ul == nil {
		return
	}
	rec := usage.UsageRecord{
		Timestamp:     in.startTime,
		KeyHash:       in.keyHash,
		KeyName:       in.keyName,
		Model:         in.model,
		AliasFrom:     in.aliasFrom,
		Backend:       in.backend,
		Endpoint:      in.endpoint,
		StatusCode:    in.statusCode,
		RequestBytes:  in.requestBytes,
		ResponseBytes: in.responseBytes,
		InputTokens:   in.inputTokens,
		OutputTokens:  in.outputTokens,
		TotalTokens:   in.totalTokens,
		DurationMS:    time.Since(in.startTime).Milliseconds(),
	}
	ul.Log(rec)
}

// logUsageChat is the Chat-Completions adapter: extract tokens from
// *api.ChunkUsage (if non-nil) and emit.
func logUsageChat(ul *usage.UsageLogger, in usageLogInput, u *api.ChunkUsage) {
	if u != nil {
		in.inputTokens = u.PromptTokens
		in.outputTokens = u.CompletionTokens
		in.totalTokens = u.TotalTokens
	}
	logUsage(ul, in)
}

// logUsageConverse is the Bedrock Converse adapter: extract tokens from
// *converseUsage (if non-nil) and emit.
func logUsageConverse(ul *usage.UsageLogger, in usageLogInput, u *converseUsage) {
	if u != nil {
		in.inputTokens = u.Input
		in.outputTokens = u.Output
		in.totalTokens = u.Input + u.Output
	}
	logUsage(ul, in)
}
