package handler

import (
	"time"

	"go-llm-proxy/internal/api"
	"go-llm-proxy/internal/usage"
)

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
	endpoint      string
	requestBytes  int64
	responseBytes int64
	inputTokens   int
	outputTokens  int
	totalTokens   int
}

// logUsage writes a single usage record. Safe to call with ul==nil.
// Emission is deferred to a goroutine so the caller's hot path is not
// blocked by SQLite contention.
func logUsage(ul *usage.UsageLogger, in usageLogInput) {
	if ul == nil {
		return
	}
	rec := usage.UsageRecord{
		Timestamp:     in.startTime,
		KeyHash:       in.keyHash,
		KeyName:       in.keyName,
		Model:         in.model,
		Endpoint:      in.endpoint,
		StatusCode:    in.statusCode,
		RequestBytes:  in.requestBytes,
		ResponseBytes: in.responseBytes,
		InputTokens:   in.inputTokens,
		OutputTokens:  in.outputTokens,
		TotalTokens:   in.totalTokens,
		DurationMS:    time.Since(in.startTime).Milliseconds(),
	}
	go ul.Log(rec)
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
