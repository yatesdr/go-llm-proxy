package handler

import (
	"encoding/json"
	"fmt"
	"net/http"

	"go-llm-proxy/internal/awsauth"
	"go-llm-proxy/internal/httputil"
)

// Bedrock error handling lives in this file so the policy is in one place:
//
//   * Classify the upstream response into an HTTP status + a coarse error
//     category. The upstream exception type (ThrottlingException,
//     ValidationException, ...) is NEVER placed in the response to the
//     client; it is captured in debugDetail for server-side logs only.
//
//   * Render the result in either the Anthropic or OAI error shape, and as
//     either a JSON document or an SSE event, depending on what the caller
//     requested.
//
// MAINTENANCE RULE: no code in this file may log or render the Bedrock API
// key, SigV4 credentials, or raw upstream body strings. Route any
// upstream-derived content through awsauth.ScrubAWSErrorBody first, and
// only include it under a Debug-level log key (scrubbed_body / debug_detail).

// apiShape selects the error shape written to the client.
type apiShape int

const (
	shapeAnthropic apiShape = iota
	shapeOAI
)

// classifyBedrockError inspects the upstream status + body and returns a
// sanitized classification suitable for the client plus a scrubbed detail
// string suitable for server-side logs.
//
// Callers MUST NOT put upstreamType into the response to the client. It is
// returned so it can be attached to a slog.Debug call for operator
// diagnosis. debugDetail is similarly log-only.
func classifyBedrockError(upstreamStatus int, upstreamBody []byte) (
	clientStatus int, errType, publicMessage, upstreamType, debugDetail string,
) {
	// Pull the AWS error envelope when present: {"__type": "...", "message": "..."}.
	// Failure to parse is fine — many error bodies are empty on streaming.
	var env struct {
		Type    string `json:"__type"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(upstreamBody, &env)
	upstreamType = env.Type

	debugDetail = awsauth.ScrubAWSErrorBody(upstreamBody)

	switch {
	case upstreamStatus == http.StatusBadRequest:
		return http.StatusBadRequest, "invalid_request_error", "invalid request",
			upstreamType, debugDetail
	case upstreamStatus == http.StatusUnauthorized,
		upstreamStatus == http.StatusForbidden:
		// Deliberately collapse to 502 so the client cannot distinguish "key
		// missing" from "key lacks policy" — those are backend ACL details
		// that the proxy's operator owns, not the caller.
		return http.StatusBadGateway, "api_error", "upstream authorization failed",
			upstreamType, debugDetail
	case upstreamStatus == http.StatusNotFound:
		return http.StatusNotFound, "not_found_error", "model not found",
			upstreamType, debugDetail
	case upstreamStatus == http.StatusRequestTimeout,
		upstreamStatus == http.StatusGatewayTimeout:
		return http.StatusGatewayTimeout, "api_error", "upstream timed out",
			upstreamType, debugDetail
	case upstreamStatus == http.StatusTooManyRequests:
		return http.StatusTooManyRequests, "rate_limit_error", "rate limited",
			upstreamType, debugDetail
	case upstreamStatus >= 500:
		return http.StatusBadGateway, "api_error", "upstream error",
			upstreamType, debugDetail
	}
	// Anything else (e.g. a 418) — collapse to 502 with the generic
	// category. Never leak the upstream status directly.
	return http.StatusBadGateway, "api_error", "upstream error",
		upstreamType, debugDetail
}

// renderBedrockError writes the classified error to the client in the shape
// the caller's API expects. For streaming requests, the response headers
// must NOT have been written yet — this function writes them.
func renderBedrockError(
	w http.ResponseWriter, shape apiShape, stream bool,
	clientStatus int, errType, publicMessage string,
) {
	if !stream {
		switch shape {
		case shapeAnthropic:
			httputil.WriteAnthropicError(w, clientStatus, errType, publicMessage)
		case shapeOAI:
			httputil.WriteError(w, clientStatus, publicMessage)
		}
		return
	}
	switch shape {
	case shapeAnthropic:
		emitBedrockSSEError(w, publicMessage)
	case shapeOAI:
		emitChatSSEError(w, publicMessage)
	}
}

// classifyStreamException maps the Bedrock event-stream :exception-type
// header to a fixed client-facing vocabulary. The upstream type string is
// logged server-side only; it must never appear in the SSE payload we
// forward to the client.
func classifyStreamException(shape apiShape, exceptionType string) (errType, publicMessage string) {
	switch exceptionType {
	case "throttlingException", "modelStreamErrorException":
		if shape == shapeAnthropic {
			return "overloaded_error", "upstream rate limited"
		}
		return "api_error", "upstream rate limited"
	case "validationException":
		return "invalid_request_error", "invalid request"
	case "modelTimeoutException":
		return "api_error", "upstream timed out"
	}
	return "api_error", "upstream stream error"
}

// bedrockLogSummary builds a short, always-safe log line describing an
// upstream failure. Use with slog.Error at the call site so INFO-level
// operators see what category of error occurred without any upstream
// identifiers.
func bedrockLogSummary(model, endpoint string, status int, errType string) string {
	return fmt.Sprintf("bedrock error model=%s endpoint=%s status=%d category=%s",
		model, endpoint, status, errType)
}
