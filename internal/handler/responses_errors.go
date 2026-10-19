package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"go-llm-proxy/internal/api"
	"go-llm-proxy/internal/httputil"
)

// upstreamError is an upstream failure translated into what a Responses API
// client (Codex in particular) understands: an HTTP status plus an
// OpenAI-style error object whose `code` drives client behaviour
// (context_length_exceeded triggers compaction, rate_limit_exceeded and
// server_is_overloaded are retried with backoff, invalid_prompt is fatal).
type upstreamError struct {
	Status     int
	Code       string
	Type       string
	Message    string
	RetryAfter string
}

var (
	urlPattern  = regexp.MustCompile(`https?://[^\s"')]+`)
	ipv4Pattern = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}(?::\d+)?\b`)
	spacePatt   = regexp.MustCompile(`\s+`)
)

// contextLengthMarkers are lowercase fragments backends use when the prompt
// does not fit the model's context window.
var contextLengthMarkers = []string{
	"context_length_exceeded",
	"maximum context length",
	"context length",
	"context window",
	"max_model_len",
	"maximum model length",
	"too many tokens",
	"prompt is too long",
	"input is too long",
	"input token count",
	"reduce the length",
	"exceeds the maximum number of tokens",
	"exceeds the model",
}

// upstreamMessage extracts a human-readable message from an upstream error
// body: OpenAI-style {"error":{"message"}}, {"message"}, FastAPI {"detail"},
// or the raw text.
func upstreamMessage(body []byte) string {
	var obj struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
		Detail  json.RawMessage `json:"detail"`
	}
	if json.Unmarshal(body, &obj) == nil {
		if len(obj.Error) > 0 {
			var e struct {
				Message string `json:"message"`
			}
			if json.Unmarshal(obj.Error, &e) == nil && e.Message != "" {
				return e.Message
			}
			var s string
			if json.Unmarshal(obj.Error, &s) == nil && s != "" {
				return s
			}
		}
		if obj.Message != "" {
			return obj.Message
		}
		var s string
		if len(obj.Detail) > 0 && json.Unmarshal(obj.Detail, &s) == nil && s != "" {
			return s
		}
	}
	return string(body)
}

// cleanUpstreamMessage removes URLs and IP addresses (internal backend
// addresses must not reach clients), collapses whitespace and bounds length.
func cleanUpstreamMessage(msg string) string {
	msg = urlPattern.ReplaceAllString(msg, "[url]")
	msg = ipv4Pattern.ReplaceAllString(msg, "[host]")
	msg = strings.TrimSpace(spacePatt.ReplaceAllString(msg, " "))
	if r := []rune(msg); len(r) > 400 {
		msg = string(r[:400]) + "..."
	}
	return msg
}

func isContextLengthError(body []byte) bool {
	lower := strings.ToLower(string(body))
	for _, m := range contextLengthMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// looksLikeModelMissing reports whether a 404 body is an OpenAI-style "model
// not found" error (route exists) rather than a missing route such as
// FastAPI's {"detail":"Not Found"} or a plain "404 page not found".
func looksLikeModelMissing(body []byte) bool {
	var obj struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &obj) != nil || len(obj.Error) == 0 {
		return false
	}
	lower := strings.ToLower(string(obj.Error))
	return strings.Contains(lower, "model") || strings.Contains(lower, "does not exist")
}

// classifyUpstreamError maps an upstream HTTP failure to what the client
// should see. Raw backend bodies are never forwarded; only a cleaned message
// for 400s and context-length errors.
func classifyUpstreamError(status int, body []byte, hdr http.Header) upstreamError {
	ue := upstreamError{Status: status, Type: "server_error", Code: "upstream_error",
		Message: fmt.Sprintf("backend returned HTTP %d", status)}
	switch {
	case isContextLengthError(body) && status >= 400 && status < 500:
		ue.Status = http.StatusBadRequest
		ue.Type = "invalid_request_error"
		ue.Code = "context_length_exceeded"
		ue.Message = cleanUpstreamMessage(upstreamMessage(body))
	case status == http.StatusBadRequest || status == http.StatusUnprocessableEntity:
		ue.Status = http.StatusBadRequest
		ue.Type = "invalid_request_error"
		ue.Code = "invalid_prompt"
		if m := cleanUpstreamMessage(upstreamMessage(body)); m != "" {
			ue.Message = m
		}
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		// The client's own key was accepted by this proxy; the backend
		// rejecting the proxy's credential is a gateway problem.
		ue.Status = http.StatusBadGateway
		ue.Code = "upstream_auth_error"
		ue.Message = "backend rejected the proxy's credentials"
	case status == http.StatusTooManyRequests:
		ue.Type = "rate_limit_error"
		ue.Code = "rate_limit_exceeded"
		ue.Message = "backend is rate limiting requests; retry shortly"
		if hdr != nil {
			ue.RetryAfter = hdr.Get("Retry-After")
		}
	case status == http.StatusServiceUnavailable:
		ue.Code = "server_is_overloaded"
		ue.Message = "backend is overloaded or unavailable; retry shortly"
		if hdr != nil {
			ue.RetryAfter = hdr.Get("Retry-After")
		}
	case status >= 400 && status < 500:
		ue.Type = "invalid_request_error"
	}
	return ue
}

// writeUpstreamError sends ue as a real HTTP error with an OpenAI-style body.
func writeUpstreamError(w http.ResponseWriter, ue upstreamError) {
	httputil.SetSecurityHeaders(w)
	w.Header().Set("Content-Type", "application/json")
	if ue.RetryAfter != "" {
		w.Header().Set("Retry-After", ue.RetryAfter)
	}
	w.WriteHeader(ue.Status)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": ue.Message,
			"type":    ue.Type,
			"code":    ue.Code,
		},
	})
}

// failedEventPayload builds the data of a response.failed SSE event.
func failedEventPayload(model string, code, errType, message string, output []any, seq int) map[string]any {
	if output == nil {
		output = []any{}
	}
	return map[string]any{
		"type": "response.failed",
		"response": map[string]any{
			"id":         api.RandomID("resp_"),
			"object":     "response",
			"created_at": float64(time.Now().Unix()),
			"model":      model,
			"status":     "failed",
			"error": map[string]any{
				"code":    code,
				"type":    errType,
				"message": message,
			},
			"output": output,
		},
		"sequence_number": seq,
	}
}

// writeFailedEvent writes a response.failed event to a stream whose headers
// are already sent.
func writeFailedEvent(w http.ResponseWriter, model, code, errType, message string) {
	data, _ := json.Marshal(failedEventPayload(model, code, errType, message, nil, 0))
	fmt.Fprintf(w, "event: response.failed\ndata: %s\n\n", data)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// respondUpstreamFailure reports ue. Before any byte is streamed it is a real
// HTTP error (clients key retry and compaction decisions off the status and
// error code); once an SSE stream is open it is a response.failed event.
func respondUpstreamFailure(w http.ResponseWriter, model string, headersSent bool, ue upstreamError) {
	if headersSent {
		writeFailedEvent(w, model, ue.Code, ue.Type, ue.Message)
		return
	}
	writeUpstreamError(w, ue)
}

// responsesUsage builds the usage object of a Responses API response from
// Chat Completions usage, including cached and reasoning token details.
func responsesUsage(u *api.ChunkUsage) any {
	if u == nil {
		return nil
	}
	cached, reasoning := 0, 0
	if u.PromptTokensDetails != nil {
		cached = u.PromptTokensDetails.CachedTokens
	}
	if u.CompletionTokensDetails != nil {
		reasoning = u.CompletionTokensDetails.ReasoningTokens
	}
	return map[string]any{
		"input_tokens":          u.PromptTokens,
		"input_tokens_details":  map[string]any{"cached_tokens": cached},
		"output_tokens":         u.CompletionTokens,
		"output_tokens_details": map[string]any{"reasoning_tokens": reasoning},
		"total_tokens":          u.TotalTokens,
	}
}

// customToolInput extracts the freeform text of a custom tool call from the
// function arguments the model produced ({"input": "..."}). Anything that is
// not that shape is passed through as-is so no model output is lost.
func customToolInput(args string) string {
	var obj map[string]json.RawMessage
	if json.Unmarshal([]byte(args), &obj) == nil {
		if raw, ok := obj["input"]; ok {
			var s string
			if json.Unmarshal(raw, &s) == nil {
				return s
			}
		}
		return args
	}
	var s string
	if json.Unmarshal([]byte(args), &s) == nil {
		return s
	}
	return args
}

// toolCallOutputItem builds the Responses output item for a completed (or, with
// status "in_progress", just-started) chat tool call: a function_call item
// (with `namespace` when the tool lives in one) or, for freeform tools, a
// custom_tool_call item.
func toolCallOutputItem(reg *toolRegistry, itemID, callID, flatName, args, status string) map[string]any {
	b, bound := reg.resolve(flatName)
	if bound && b.Custom {
		input := ""
		if status != "in_progress" {
			input = customToolInput(args)
		}
		item := map[string]any{
			"id":      itemID,
			"type":    "custom_tool_call",
			"call_id": callID,
			"name":    b.Name,
			"input":   input,
			"status":  status,
		}
		if b.Namespace != "" {
			item["namespace"] = b.Namespace
		}
		return item
	}
	name := flatName
	item := map[string]any{
		"id":        itemID,
		"type":      "function_call",
		"call_id":   callID,
		"arguments": args,
		"status":    status,
	}
	if bound {
		name = b.Name
		if b.Namespace != "" {
			item["namespace"] = b.Namespace
		}
	}
	item["name"] = name
	return item
}

// cleanStructuredOutput prepares the final text of a structured-output
// (json_schema / json_object) request. Models without enforced decoding wrap
// the JSON in Markdown fences or prose; if a valid JSON value can be pulled
// out, return exactly that, otherwise the text unchanged.
func cleanStructuredOutput(text string) string {
	trimmed := strings.TrimSpace(text)
	if json.Valid([]byte(trimmed)) {
		return trimmed
	}
	if strings.HasPrefix(trimmed, "```") {
		body := strings.TrimPrefix(trimmed, "```")
		if i := strings.IndexByte(body, '\n'); i >= 0 {
			body = body[i+1:]
		}
		body = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(body), "```"))
		if json.Valid([]byte(body)) {
			return body
		}
	}
	if start, end := strings.IndexByte(trimmed, '{'), strings.LastIndexByte(trimmed, '}'); start >= 0 && end > start {
		if cand := trimmed[start : end+1]; json.Valid([]byte(cand)) {
			return cand
		}
	}
	return text
}
