package handler

import (
	"net/http"
	"strings"
	"testing"
)

// The bedrock_errors classifier has one security-critical job: translate an
// upstream Bedrock response into a client-safe HTTP status + error category
// without leaking AWS identifiers (account IDs, ARNs, request IDs) or the
// internal exception class name (ThrottlingException, ValidationException,
// etc.). These tests lock that contract.

func TestClassifyBedrockError_StatusMapping(t *testing.T) {
	cases := []struct {
		name          string
		inStatus      int
		wantStatus    int
		wantErrType   string
		wantMsgPrefix string
	}{
		{"400 → 400 invalid", 400, 400, "invalid_request_error", "invalid"},
		{"401 collapses to 502", 401, 502, "api_error", "upstream authorization"},
		{"403 collapses to 502", 403, 502, "api_error", "upstream authorization"},
		{"404 → 404 not_found", 404, 404, "not_found_error", "model not found"},
		{"408 → 504 timeout", 408, 504, "api_error", "upstream timed out"},
		{"429 → 429 rate_limit", 429, 429, "rate_limit_error", "rate limited"},
		{"500 → 502 api_error", 500, 502, "api_error", "upstream error"},
		{"502 → 502 api_error", 502, 502, "api_error", "upstream error"},
		{"503 → 502 api_error", 503, 502, "api_error", "upstream error"},
		{"504 → 504 timeout", 504, 504, "api_error", "upstream timed out"},
		{"unknown 418 → 502 generic", 418, 502, "api_error", "upstream error"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, errType, msg, _, _ := classifyBedrockError(c.inStatus, nil)
			if status != c.wantStatus {
				t.Errorf("status: got %d want %d", status, c.wantStatus)
			}
			if errType != c.wantErrType {
				t.Errorf("errType: got %q want %q", errType, c.wantErrType)
			}
			if !strings.Contains(strings.ToLower(msg), c.wantMsgPrefix) {
				t.Errorf("message %q does not contain %q", msg, c.wantMsgPrefix)
			}
		})
	}
}

func TestClassifyBedrockError_NoUpstreamTypeLeakage(t *testing.T) {
	// A representative AWS error body. classifyBedrockError must extract
	// __type for server-side logs but NEVER put it (or the ARN / account ID
	// / request ID) into the client-facing message.
	body := []byte(`{
		"__type": "ThrottlingException",
		"message": "Rate exceeded for arn:aws:bedrock:us-east-2:123456789012:inference-profile/us.anthropic.claude-3-5-sonnet-20241022-v2:0. Request ID: 8a1d5b2e-cd2f-4f1f-9a40-3f91c1e5a18b"
	}`)
	_, _, publicMessage, upstreamType, debugDetail := classifyBedrockError(http.StatusTooManyRequests, body)

	// Server-side diagnostic should capture the type for operators.
	if upstreamType != "ThrottlingException" {
		t.Errorf("upstreamType: got %q want ThrottlingException", upstreamType)
	}

	// Public message must not leak upstream identifiers or type names.
	forbidden := []string{
		"ThrottlingException",
		"arn:aws",
		"123456789012",
		"8a1d5b2e-cd2f-4f1f-9a40-3f91c1e5a18b",
		"us-east-2",
		"inference-profile",
	}
	for _, f := range forbidden {
		if strings.Contains(publicMessage, f) {
			t.Errorf("public message leaks %q: %s", f, publicMessage)
		}
	}

	// Debug detail is server-side only, but it must still have identifiers
	// redacted by the scrubber (account ID + request UUID).
	if strings.Contains(debugDetail, "123456789012") {
		t.Errorf("debug detail contains raw account ID: %s", debugDetail)
	}
	if strings.Contains(debugDetail, "8a1d5b2e-cd2f-4f1f-9a40-3f91c1e5a18b") {
		t.Errorf("debug detail contains raw request UUID: %s", debugDetail)
	}
}

func TestClassifyStreamException_NoTypeLeakage(t *testing.T) {
	cases := []struct {
		upstreamType string
		shape        apiShape
		wantType     string
	}{
		{"throttlingException", shapeAnthropic, "overloaded_error"},
		{"throttlingException", shapeOAI, "api_error"},
		{"modelStreamErrorException", shapeAnthropic, "overloaded_error"},
		{"validationException", shapeAnthropic, "invalid_request_error"},
		{"modelTimeoutException", shapeAnthropic, "api_error"},
		{"SomeNewFutureException", shapeAnthropic, "api_error"},
		{"", shapeAnthropic, "api_error"},
	}
	for _, c := range cases {
		t.Run(c.upstreamType, func(t *testing.T) {
			errType, msg := classifyStreamException(c.shape, c.upstreamType)
			if errType != c.wantType {
				t.Errorf("errType: got %q want %q", errType, c.wantType)
			}
			if c.upstreamType != "" && strings.Contains(msg, c.upstreamType) {
				t.Errorf("public message leaks upstream type: %s", msg)
			}
		})
	}
}
