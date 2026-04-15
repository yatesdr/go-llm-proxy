package awsauth

import (
	"strings"
	"testing"
)

func TestScrubAWSErrorBody_RedactsARN(t *testing.T) {
	in := `{"message":"User: arn:aws:iam::123456789012:user/alice is not authorized"}`
	got := ScrubAWSErrorBody([]byte(in))
	if strings.Contains(got, "123456789012") {
		t.Errorf("account id leaked: %s", got)
	}
	if strings.Contains(got, "arn:aws:iam") {
		t.Errorf("full ARN leaked: %s", got)
	}
	if !strings.Contains(got, "REDACTED") {
		t.Errorf("expected REDACTED marker: %s", got)
	}
	if !strings.Contains(got, "not authorized") {
		t.Errorf("human-readable message should survive: %s", got)
	}
}

func TestScrubAWSErrorBody_RedactsBareAccountID(t *testing.T) {
	in := `account 999999999999 denied`
	got := ScrubAWSErrorBody([]byte(in))
	if strings.Contains(got, "999999999999") {
		t.Errorf("bare account id leaked: %s", got)
	}
}

func TestScrubAWSErrorBody_RedactsAccessKeyIDs(t *testing.T) {
	// AKIA/ASIA + 16 alphanumerics = 20 chars total.
	in := `key AKIAIOSFODNN7EXAMPLE rotated by ASIAIOSFODNN7EXAMPLE`
	got := ScrubAWSErrorBody([]byte(in))
	if strings.Contains(got, "AKIA") {
		t.Errorf("AKIA leaked: %s", got)
	}
	if strings.Contains(got, "ASIA") {
		t.Errorf("ASIA leaked: %s", got)
	}
}

func TestScrubAWSErrorBody_RedactsRequestID(t *testing.T) {
	in := `RequestId: 12345678-1234-1234-1234-123456789abc`
	got := ScrubAWSErrorBody([]byte(in))
	if strings.Contains(got, "12345678-1234") {
		t.Errorf("request id leaked: %s", got)
	}
}

func TestScrubAWSErrorBody_Truncates(t *testing.T) {
	in := make([]byte, 4096)
	for i := range in {
		in[i] = 'a'
	}
	got := ScrubAWSErrorBody(in)
	if len(got) > 2100 {
		t.Errorf("no truncation: len=%d", len(got))
	}
	if !strings.Contains(got, "truncated") {
		t.Errorf("truncation marker missing")
	}
}

func TestScrubAWSErrorBody_EmptyInput(t *testing.T) {
	if got := ScrubAWSErrorBody(nil); got != "" {
		t.Errorf("nil should produce empty, got %q", got)
	}
	if got := ScrubAWSErrorBody([]byte{}); got != "" {
		t.Errorf("empty should produce empty, got %q", got)
	}
}

func TestScrubAWSErrorBody_KeepsErrorTypeName(t *testing.T) {
	in := `{"message":"ValidationException: The provided model identifier is invalid."}`
	got := ScrubAWSErrorBody([]byte(in))
	if !strings.Contains(got, "ValidationException") {
		t.Errorf("error type name should be kept for debugging: %s", got)
	}
	if !strings.Contains(got, "model identifier is invalid") {
		t.Errorf("error message should be kept: %s", got)
	}
}
