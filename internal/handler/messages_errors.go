package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"go-llm-proxy/internal/httputil"
)

// This file maps upstream failures onto Anthropic-shaped errors for the
// Messages endpoint. Clients (Claude Code) act on the HTTP status, the
// error.type and a few message formats, so each of those is chosen
// deliberately:
//
//   - 401/403 from the backend are the proxy's own credentials failing, not the
//     client's; they become 502 api_error so the client does not start a login
//     flow or give up on a key that is fine.
//   - context-length failures are rewritten to "prompt is too long: N tokens >
//     M maximum" (or the max_tokens variant) which the client parses to compact
//     or shrink max_tokens.
//   - messages are scrubbed of URLs, addresses and credentials before being
//     returned; the unscrubbed body is only logged.

// anthropicErrorForStatus maps an upstream HTTP status to the status and
// Anthropic error type returned to the client.
func anthropicErrorForStatus(upstream int) (int, string) {
	switch {
	case upstream == http.StatusBadRequest || upstream == http.StatusUnprocessableEntity:
		return http.StatusBadRequest, "invalid_request_error"
	case upstream == http.StatusUnauthorized || upstream == http.StatusForbidden:
		return http.StatusBadGateway, "api_error"
	case upstream == http.StatusNotFound:
		return http.StatusNotFound, "not_found_error"
	case upstream == http.StatusRequestEntityTooLarge:
		return http.StatusRequestEntityTooLarge, "request_too_large"
	case upstream == http.StatusTooManyRequests:
		return http.StatusTooManyRequests, "rate_limit_error"
	case upstream == http.StatusServiceUnavailable || upstream == 529:
		return upstream, "overloaded_error"
	case upstream >= 500:
		return upstream, "api_error"
	case upstream >= 400:
		return http.StatusBadRequest, "invalid_request_error"
	}
	return http.StatusBadGateway, "api_error"
}

// upstreamErrorInfo is the parsed form of a backend error body.
type upstreamErrorInfo struct {
	Message string
	Type    string // Anthropic-style error.type, when the body carried one
	// Token counts some servers return as structured fields (llama.cpp).
	PromptTokens int
	ContextSize  int
}

// parseUpstreamError extracts the human message (and any type / token counts)
// from the error body formats seen in the wild: OpenAI/vLLM
// {"error":{"message":...}}, {"error":"..."}, FastAPI {"detail":...},
// {"message":...}, and Anthropic {"type":"error","error":{...}}.
func parseUpstreamError(body []byte) upstreamErrorInfo {
	var info upstreamErrorInfo
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return info
	}

	var generic map[string]json.RawMessage
	if json.Unmarshal(body, &generic) != nil {
		info.Message = trimmed
		return info
	}

	str := func(raw json.RawMessage) string {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
		return ""
	}

	if raw, ok := generic["error"]; ok {
		var obj struct {
			Message     json.RawMessage `json:"message"`
			Type        string          `json:"type"`
			NPromptToks int             `json:"n_prompt_tokens"`
			NCtx        int             `json:"n_ctx"`
			Detail      json.RawMessage `json:"detail"`
		}
		if s := str(raw); s != "" {
			info.Message = s
		} else if json.Unmarshal(raw, &obj) == nil {
			info.Message = str(obj.Message)
			if info.Message == "" {
				info.Message = str(obj.Detail)
			}
			info.Type = obj.Type
			info.PromptTokens = obj.NPromptToks
			info.ContextSize = obj.NCtx
		}
	}
	if info.Message == "" {
		for _, k := range []string{"message", "detail", "msg"} {
			if raw, ok := generic[k]; ok {
				if s := str(raw); s != "" {
					info.Message = s
					break
				}
				// FastAPI validation errors: detail is a list of objects.
				var items []struct {
					Msg string `json:"msg"`
				}
				if json.Unmarshal(raw, &items) == nil && len(items) > 0 {
					var parts []string
					for _, it := range items {
						if it.Msg != "" {
							parts = append(parts, it.Msg)
						}
					}
					info.Message = strings.Join(parts, "; ")
					break
				}
			}
		}
	}
	if info.Message == "" {
		info.Message = trimmed
	}
	return info
}

var (
	scrubURLRe    = regexp.MustCompile(`(?i)\b(?:https?|wss?|ftp)://[^\s"'<>)]+`)
	scrubHostRe   = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}(?::\d{2,5})?\b`)
	scrubBearerRe = regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{8,}`)
	scrubKeyRe    = regexp.MustCompile(`\b(?:sk|pk|rk|key|tok|tvly|ghp|xox[a-z])[-_][A-Za-z0-9_-]{8,}`)
	scrubKVRe     = regexp.MustCompile(`(?i)(api[_-]?key|authorization|secret|token|password)(["']?\s*[:=]\s*["']?)[^\s"',}]+`)
)

// scrubUpstreamMessage removes URLs, IPv4 addresses and credential-looking
// strings from a backend error message and bounds its length.
func scrubUpstreamMessage(s string) string {
	s = scrubURLRe.ReplaceAllString(s, "[url]")
	s = scrubBearerRe.ReplaceAllString(s, "Bearer [redacted]")
	s = scrubKVRe.ReplaceAllString(s, "${1}${2}[redacted]")
	s = scrubKeyRe.ReplaceAllString(s, "[redacted]")
	s = scrubHostRe.ReplaceAllString(s, "[host]")
	s = strings.TrimSpace(s)
	const maxLen = 1000
	if len(s) > maxLen {
		s = s[:maxLen] + "..."
	}
	return s
}

// Context-length message shapes. Each regexp captures two or three numbers;
// the order is described by the matching extractor.
var (
	// vLLM: "...maximum context length is 32768 tokens. However, you requested
	// 40000 tokens (35000 in the messages, 5000 in the completion)..."
	ctxVLLMRequested = regexp.MustCompile(`(?is)maximum context length is (\d+) tokens.*?requested (\d+) tokens\s*\((\d+) in the messages,\s*(\d+) in the completion`)
	// vLLM >= 0.9: "You passed 35000 input tokens and requested 5000 output
	// tokens. However, the model's context length is only 32768 tokens..."
	ctxVLLMPassed = regexp.MustCompile(`(?is)you passed (\d+) input tokens and requested (\d+) output tokens.*?context length is only (\d+) tokens`)
	// vLLM: "'max_tokens' ... is too large: 5000. This model's maximum context
	// length is 32768 tokens and your request has 35000 input tokens"
	ctxVLLMTooLarge = regexp.MustCompile(`(?is)too large: (\d+)\.\s*this model's maximum context length is (\d+) tokens and your request has (\d+) input tokens`)
	// SGLang: "...maximum context length of 32768 tokens. You requested a total
	// of 40000 tokens: 35000 tokens from the input messages and 5000 tokens
	// for the completion."
	ctxSGLang = regexp.MustCompile(`(?is)maximum context length of (\d+) tokens.*?requested a total of (\d+) tokens:\s*(\d+) tokens from the input.*?(\d+) tokens for the completion`)
	// SGLang / others: "Input length (40001) exceeds the maximum allowed length (32768)"
	ctxInputLen = regexp.MustCompile(`(?is)input length \((\d+)\) exceeds the maximum allowed length \((\d+)\)`)
	// Generic fallback for "maximum context length is M ... N tokens".
	ctxGenericHint = regexp.MustCompile(`(?i)(context length|context window|context size|too many tokens|prompt is too long|too long)`)
)

// rewriteContextLengthError recognises backend context-overflow errors and
// returns the Anthropic-style message the client understands:
//
//	prompt is too long: N tokens > M maximum
//	input length and `max_tokens` exceed context limit: N + MAX > M
//
// The second form is used when the prompt itself fits and only the requested
// completion overflows, so the client shrinks max_tokens instead of compacting.
func rewriteContextLengthError(info upstreamErrorInfo) (string, bool) {
	msg := info.Message
	tooLong := func(n, m int) (string, bool) {
		return fmt.Sprintf("prompt is too long: %d tokens > %d maximum", n, m), true
	}
	overflow := func(n, maxTok, m int) (string, bool) {
		if n > m {
			return tooLong(n, m)
		}
		return fmt.Sprintf("input length and `max_tokens` exceed context limit: %d + %d > %d", n, maxTok, m), true
	}
	num := func(s string) int { v, _ := strconv.Atoi(s); return v }

	if info.PromptTokens > 0 && info.ContextSize > 0 {
		return tooLong(info.PromptTokens, info.ContextSize)
	}
	if m := ctxVLLMRequested.FindStringSubmatch(msg); m != nil {
		return overflow(num(m[3]), num(m[4]), num(m[1]))
	}
	if m := ctxVLLMPassed.FindStringSubmatch(msg); m != nil {
		return overflow(num(m[1]), num(m[2]), num(m[3]))
	}
	if m := ctxVLLMTooLarge.FindStringSubmatch(msg); m != nil {
		return overflow(num(m[3]), num(m[1]), num(m[2]))
	}
	if m := ctxSGLang.FindStringSubmatch(msg); m != nil {
		return overflow(num(m[3]), num(m[4]), num(m[1]))
	}
	if m := ctxInputLen.FindStringSubmatch(msg); m != nil {
		return tooLong(num(m[1]), num(m[2]))
	}
	if info.Type == "exceed_context_size_error" ||
		(ctxGenericHint.MatchString(msg) && strings.Contains(strings.ToLower(msg), "context")) {
		return "prompt is too long: " + scrubUpstreamMessage(msg), true
	}
	return "", false
}

// buildClientError turns a backend error response into the status, error type
// and message returned to the client. native selects the Anthropic-passthrough
// flavour, where the backend's own error.type is kept when it is meaningful.
func buildClientError(status int, body []byte, native bool) (int, string, string) {
	info := parseUpstreamError(body)
	outStatus, outType := anthropicErrorForStatus(status)

	if native && info.Type != "" && status != http.StatusUnauthorized && status != http.StatusForbidden {
		switch info.Type {
		case "invalid_request_error", "rate_limit_error", "overloaded_error", "api_error",
			"not_found_error", "request_too_large":
			outType = info.Type
		}
	}

	msg := ""
	if status == http.StatusBadRequest || status == http.StatusRequestEntityTooLarge || status == http.StatusUnprocessableEntity {
		if rewritten, ok := rewriteContextLengthError(info); ok {
			// Keep it a plain 400 invalid_request_error: that is what the
			// client's "prompt is too long" handling keys on.
			return http.StatusBadRequest, "invalid_request_error", rewritten
		}
	}
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		msg = "the model backend rejected the proxy's credentials"
		if info.Message != "" {
			msg += ": " + scrubUpstreamMessage(info.Message)
		}
	default:
		msg = scrubUpstreamMessage(info.Message)
	}
	if msg == "" {
		msg = fmt.Sprintf("backend returned HTTP %d", status)
	}
	return outStatus, outType, msg
}

// retryHeaders are the upstream headers clients use to pace retries.
var retryHeaders = []string{"Retry-After", "Retry-After-Ms", "X-Should-Retry"}

// copyRetryHeaders forwards retry-pacing and rate-limit headers from an
// upstream response (retry-after, retry-after-ms, x-should-retry,
// anthropic-ratelimit-*) to the client response.
func copyRetryHeaders(w http.ResponseWriter, src http.Header) {
	for _, k := range retryHeaders {
		if v := src.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	for k, vals := range src {
		if strings.HasPrefix(strings.ToLower(k), "anthropic-ratelimit-") && len(vals) > 0 {
			w.Header().Set(k, vals[0])
		}
	}
}

// writeUpstreamFailure answers a non-streaming (or not-yet-started streaming)
// request with the mapped error and the upstream's retry headers.
func writeUpstreamFailure(w http.ResponseWriter, upstream http.Header, status int, body []byte, native bool) {
	outStatus, outType, msg := buildClientError(status, body, native)
	copyRetryHeaders(w, upstream)
	httputil.WriteAnthropicError(w, outStatus, outType, msg)
}

// writeSSEError emits an Anthropic stream error event on an already-started
// SSE response.
func writeSSEError(w http.ResponseWriter, errType, message string) {
	data, _ := json.Marshal(map[string]any{
		"type":  "error",
		"error": map[string]any{"type": errType, "message": message},
	})
	fmt.Fprintf(w, "event: error\ndata: %s\n\n", data)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
