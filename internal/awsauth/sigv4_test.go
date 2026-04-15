package awsauth

import (
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestDeriveSigningKey verifies the HMAC chain against the published AWS
// reference example from "Examples of how to derive a signing key for
// Signature Version 4" — the canonical external test for this primitive.
//   https://docs.aws.amazon.com/IAM/latest/UserGuide/reference_sigv-create-signing-key.html
func TestDeriveSigningKey(t *testing.T) {
	secret := "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	date := "20120215"
	region := "us-east-1"
	service := "iam"
	want := "f4780e2d9f65fa895f9c67b32ce1baf0b0d8a43505a000a1a9e090d414db404d"

	got := hex.EncodeToString(deriveSigningKey(secret, date, region, service))
	if got != want {
		t.Fatalf("kSigning mismatch:\n  got:  %s\n  want: %s", got, want)
	}
}

func TestHashEmptyPayload(t *testing.T) {
	got := hashPayload(nil)
	if got != emptyBodyHash {
		t.Fatalf("empty body hash:\n  got:  %s\n  want: %s", got, emptyBodyHash)
	}
	if hashPayload([]byte{}) != emptyBodyHash {
		t.Fatalf("zero-length body hash should match nil")
	}
}

func TestHashKnownPayload(t *testing.T) {
	// SHA256("hello") — well-known.
	want := "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	got := hashPayload([]byte("hello"))
	if got != want {
		t.Fatalf("hash mismatch:\n  got:  %s\n  want: %s", got, want)
	}
}

func TestTrimAndCollapse(t *testing.T) {
	cases := map[string]string{
		"":                      "",
		"   ":                   "",
		"foo":                   "foo",
		"  foo  ":               "foo",
		"foo  bar":              "foo bar",
		"foo\t\tbar":            "foo bar",
		"a   b   c":             "a b c",
		`"keep  spaces"`:        `"keep  spaces"`,
		`pre  "in  side"  post`: `pre "in  side" post`,
	}
	for in, want := range cases {
		if got := trimAndCollapse(in); got != want {
			t.Errorf("trimAndCollapse(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAWSURLEncode(t *testing.T) {
	cases := []struct {
		in          string
		encodeSlash bool
		want        string
	}{
		{"abc", true, "abc"},
		{"a b", true, "a%20b"},
		{"a/b", true, "a%2Fb"},
		{"a/b", false, "a/b"},
		{"a+b", true, "a%2Bb"},
		{"a=b", true, "a%3Db"},
		{"a&b", true, "a%26b"},
		{"a~b._-", true, "a~b._-"},
		{"héllo", true, "h%C3%A9llo"},
	}
	for _, tc := range cases {
		if got := awsURLEncode(tc.in, tc.encodeSlash); got != tc.want {
			t.Errorf("awsURLEncode(%q, %v) = %q, want %q", tc.in, tc.encodeSlash, got, tc.want)
		}
	}
}

func TestCanonicalQueryString(t *testing.T) {
	cases := map[string]string{
		"":                "",
		"b=2&a=1":         "a=1&b=2",
		"a=2&a=1":         "a=1&a=2",          // sorted by value when keys tie
		"q=hello world":   "q=hello%20world",  // space encoded
		"k=v+w":           "k=v%2Bw",          // '+' is literal, not space
		"key with space=":  "key%20with%20space=",
	}
	for in, want := range cases {
		if got := canonicalQueryString(in); got != want {
			t.Errorf("canonicalQueryString(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestCanonicalRequest_Bedrock asserts the canonical request format for a
// realistic Bedrock Converse call. We verify the structural correctness of
// each section rather than against an AWS suite vector, since the suite
// doesn't cover bedrock service requests.
func TestCanonicalRequest_Bedrock(t *testing.T) {
	body := []byte(`{"messages":[]}`)
	req, _ := http.NewRequest(http.MethodPost,
		"https://bedrock-runtime.us-east-1.amazonaws.com/model/anthropic.claude-3-sonnet/converse", nil)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Amz-Date", "20240101T120000Z")
	req.Header.Set("X-Amz-Content-Sha256", hashPayload(body))

	canonical, signed := canonicalRequest(req, hashPayload(body))

	wantSigned := "content-type;host;x-amz-content-sha256;x-amz-date"
	if signed != wantSigned {
		t.Fatalf("signed headers:\n  got:  %s\n  want: %s", signed, wantSigned)
	}

	want := strings.Join([]string{
		"POST",
		"/model/anthropic.claude-3-sonnet/converse",
		"",
		"content-type:application/json",
		"host:bedrock-runtime.us-east-1.amazonaws.com",
		"x-amz-content-sha256:" + hashPayload(body),
		"x-amz-date:20240101T120000Z",
		"",
		wantSigned,
		hashPayload(body),
	}, "\n")
	if canonical != want {
		t.Fatalf("canonical request mismatch:\n  got:\n%s\n  want:\n%s", canonical, want)
	}
}

// TestSignRequest_EndToEnd locks down the full output of SignRequest with
// fixed inputs. The expected signature was computed by an independent run of
// this same algorithm; if any of the primitives drift, this will catch it.
// (Treat the golden signature as a regression anchor, not external proof —
// TestDeriveSigningKey provides the external correctness anchor.)
func TestSignRequest_EndToEnd(t *testing.T) {
	creds := Credentials{
		AccessKeyID:     "AKIDEXAMPLE",
		SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
	}
	body := []byte(`{"messages":[{"role":"user","content":[{"text":"hi"}]}]}`)
	req, _ := http.NewRequest(http.MethodPost,
		"https://bedrock-runtime.us-east-1.amazonaws.com/model/anthropic.claude-3-sonnet/converse",
		strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")

	now := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	SignRequest(req, body, creds, "us-east-1", "bedrock", now)

	// Required headers are present.
	if req.Header.Get("X-Amz-Date") != "20240101T120000Z" {
		t.Errorf("X-Amz-Date wrong: %q", req.Header.Get("X-Amz-Date"))
	}
	if req.Header.Get("X-Amz-Content-Sha256") != hashPayload(body) {
		t.Errorf("X-Amz-Content-Sha256 wrong: %q", req.Header.Get("X-Amz-Content-Sha256"))
	}
	if req.Header.Get("X-Amz-Security-Token") != "" {
		t.Errorf("X-Amz-Security-Token should be empty for static creds")
	}

	auth := req.Header.Get("Authorization")
	wantPrefix := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20240101/us-east-1/bedrock/aws4_request, SignedHeaders=content-type;host;x-amz-content-sha256;x-amz-date, Signature="
	if !strings.HasPrefix(auth, wantPrefix) {
		t.Fatalf("Authorization prefix mismatch:\n  got:  %s\n  want prefix: %s", auth, wantPrefix)
	}

	sig := strings.TrimPrefix(auth, wantPrefix)
	if len(sig) != 64 {
		t.Errorf("signature should be 64 hex chars, got %d (%q)", len(sig), sig)
	}
	for _, c := range sig {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Errorf("signature contains non-hex char: %c", c)
			break
		}
	}

	// Lock in the actual computed signature as a regression anchor: any drift
	// in the primitives (canonicalization, key derivation, etc.) will trip
	// this. External correctness comes from TestDeriveSigningKey.
	const wantSig = "d25839fc06276ce5daeaf6c959572b650f8cb2f44cce45b8aa716fe6efc7a936"
	if sig != wantSig {
		t.Errorf("signature regression:\n  got:  %s\n  want: %s", sig, wantSig)
	}
}

func TestSignRequest_WithSessionToken(t *testing.T) {
	creds := Credentials{
		AccessKeyID:     "AKIDEXAMPLE",
		SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
		SessionToken:    "FwoGZXIvYXdzEXAMPLEsessiontoken",
	}
	req, _ := http.NewRequest(http.MethodGet, "https://bedrock-runtime.us-east-1.amazonaws.com/foundation-models", nil)
	SignRequest(req, nil, creds, "us-east-1", "bedrock", time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC))

	if req.Header.Get("X-Amz-Security-Token") != creds.SessionToken {
		t.Fatalf("session token header missing or wrong: %q", req.Header.Get("X-Amz-Security-Token"))
	}
	if !strings.Contains(req.Header.Get("Authorization"), "x-amz-security-token") {
		t.Errorf("session token must be in SignedHeaders: %q", req.Header.Get("Authorization"))
	}
}

func TestSignRequest_Deterministic(t *testing.T) {
	creds := Credentials{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "secret"}
	body := []byte(`{"x":1}`)
	now := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)

	req1, _ := http.NewRequest(http.MethodPost, "https://bedrock-runtime.us-east-1.amazonaws.com/x", nil)
	SignRequest(req1, body, creds, "us-east-1", "bedrock", now)

	req2, _ := http.NewRequest(http.MethodPost, "https://bedrock-runtime.us-east-1.amazonaws.com/x", nil)
	SignRequest(req2, body, creds, "us-east-1", "bedrock", now)

	if req1.Header.Get("Authorization") != req2.Header.Get("Authorization") {
		t.Fatalf("signing should be deterministic for identical inputs")
	}
}

func TestSignRequest_DifferentBodyChangesSignature(t *testing.T) {
	creds := Credentials{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "secret"}
	now := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)

	req1, _ := http.NewRequest(http.MethodPost, "https://bedrock-runtime.us-east-1.amazonaws.com/x", nil)
	SignRequest(req1, []byte(`{"x":1}`), creds, "us-east-1", "bedrock", now)

	req2, _ := http.NewRequest(http.MethodPost, "https://bedrock-runtime.us-east-1.amazonaws.com/x", nil)
	SignRequest(req2, []byte(`{"x":2}`), creds, "us-east-1", "bedrock", now)

	if req1.Header.Get("Authorization") == req2.Header.Get("Authorization") {
		t.Fatalf("body change must produce a different signature")
	}
}
