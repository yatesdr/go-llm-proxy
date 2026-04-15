package awsauth

import (
	"regexp"
)

// ScrubAWSErrorBody returns a logger-safe rendering of an AWS error body.
//
// Raw AWS error responses routinely contain:
//   - 12-digit account IDs embedded in ARNs
//   - Full ARNs (arn:aws:bedrock:us-east-2:123456789012:inference-profile/...)
//   - X-Amzn-RequestId and X-Amz-Errors trace tokens
//
// Those fields are lower-trust than the HTTP response we return to the
// client (which is already sanitized to "bedrock returned HTTP N"), because
// logs end up in syslog / journald / Loki / Datadog / etc. Scrub before
// emitting so the AWS account ID doesn't land in a third-party log pipeline.
//
// We redact conservatively — the scrubbed message is meant for operator
// diagnosis, so we keep the AWS error-code strings (e.g. ValidationException)
// and the human-readable message shape; we just redact identifiers.
func ScrubAWSErrorBody(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	s := string(body)
	if len(s) > 2048 {
		s = s[:2048] + "…[truncated]"
	}
	for _, r := range scrubPatterns {
		s = r.re.ReplaceAllString(s, r.replace)
	}
	return s
}

type scrubRule struct {
	re      *regexp.Regexp
	replace string
}

var scrubPatterns = []scrubRule{
	// Full ARN → arn:aws:<service>:<region>:REDACTED:<rest>
	{
		re:      regexp.MustCompile(`arn:aws[a-z-]*:[a-z0-9-]+:[a-z0-9-]*:\d{12}:[^\s"',]+`),
		replace: "arn:aws:REDACTED",
	},
	// Bare 12-digit account IDs (catches them outside ARN context too).
	{
		re:      regexp.MustCompile(`\b\d{12}\b`),
		replace: "REDACTED-ACCOUNT",
	},
	// AWS access key IDs.
	{
		re:      regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
		replace: "REDACTED-AKID",
	},
	{
		re:      regexp.MustCompile(`\bASIA[0-9A-Z]{16}\b`),
		replace: "REDACTED-ASID",
	},
	// AWS request IDs / trace tokens — UUID-shaped or opaque hex/base64.
	{
		re:      regexp.MustCompile(`\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`),
		replace: "REDACTED-UUID",
	},
}
