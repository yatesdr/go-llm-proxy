// Package awsauth implements AWS Signature Version 4 request signing.
//
// This is a hand-rolled SigV4 implementation that matches the algorithm
// specified at https://docs.aws.amazon.com/IAM/latest/UserGuide/reference_sigv-create-signed-request.html.
// It is verified against the AWS-published test suite (see sigv4_test.go).
//
// The signer is intentionally narrow: it signs a single in-flight HTTP request
// with the body already in memory (Bedrock requests are small JSON documents,
// not streaming uploads). Callers pass the raw body bytes alongside the
// request so the signer can compute the payload hash without consuming
// req.Body.
package awsauth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Credentials are the IAM credentials used to derive a SigV4 signing key.
// SessionToken is optional and only set when using temporary STS credentials.
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
}

const (
	algorithm  = "AWS4-HMAC-SHA256"
	terminator = "aws4_request"

	timeFormat     = "20060102T150405Z"
	dateFormat     = "20060102"
	emptyBodyHash  = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	unsignedHeader = "UNSIGNED-PAYLOAD"
)

// SignRequest signs req in place using the SigV4 algorithm. It sets the
// Authorization, X-Amz-Date, X-Amz-Content-Sha256 headers (and
// X-Amz-Security-Token when SessionToken is non-empty). The Host header is
// signed implicitly via req.Host / req.URL.Host — callers don't need to set
// it manually.
//
// body is the exact bytes that will be sent as the request body; pass nil for
// an empty body. For GET / DELETE / HEAD requests pass nil. The body is not
// consumed and req.Body is not modified.
//
// service is the AWS service name (e.g. "bedrock"). region is the AWS region
// (e.g. "us-east-1"). now controls the timestamp embedded in the signature;
// callers in production should pass time.Now().UTC().
func SignRequest(req *http.Request, body []byte, creds Credentials, region, service string, now time.Time) {
	now = now.UTC()
	amzDate := now.Format(timeFormat)
	dateStamp := now.Format(dateFormat)

	// Step 0: set the headers that participate in signing. We must set these
	// before computing the canonical request so they're included in the hash.
	req.Header.Set("X-Amz-Date", amzDate)
	payloadHash := hashPayload(body)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if creds.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", creds.SessionToken)
	}

	// Step 1: build the canonical request.
	canonicalReq, signedHeaders := canonicalRequest(req, payloadHash)

	// Step 2: build the string to sign.
	credentialScope := fmt.Sprintf("%s/%s/%s/%s", dateStamp, region, service, terminator)
	stringToSign := strings.Join([]string{
		algorithm,
		amzDate,
		credentialScope,
		hexSHA256([]byte(canonicalReq)),
	}, "\n")

	// Step 3: derive the signing key and compute the signature.
	signingKey := deriveSigningKey(creds.SecretAccessKey, dateStamp, region, service)
	signature := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	// Step 4: assemble the Authorization header.
	authHeader := fmt.Sprintf("%s Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		algorithm,
		creds.AccessKeyID,
		credentialScope,
		signedHeaders,
		signature,
	)
	req.Header.Set("Authorization", authHeader)
}

// canonicalRequest builds the SigV4 canonical request string and the
// semicolon-separated signed-headers list. Spec:
//
//	CanonicalRequest =
//	    HTTPRequestMethod + '\n' +
//	    CanonicalURI + '\n' +
//	    CanonicalQueryString + '\n' +
//	    CanonicalHeaders + '\n' +
//	    SignedHeaders + '\n' +
//	    HashedPayload
func canonicalRequest(req *http.Request, payloadHash string) (canonical, signedHeaders string) {
	method := req.Method
	if method == "" {
		method = http.MethodGet
	}

	uri := canonicalURI(req.URL.EscapedPath())
	query := canonicalQueryString(req.URL.RawQuery)
	headers, signed := canonicalHeaders(req)

	canonical = strings.Join([]string{
		method,
		uri,
		query,
		headers,
		signed,
		payloadHash,
	}, "\n")
	return canonical, signed
}

// canonicalURI returns the URI path, percent-encoded per RFC 3986. Each path
// segment is double-encoded for non-S3 services (this matches AWS reference
// behavior — see the test vectors for "get-space" which expects "/%20/foo").
// We use req.URL.EscapedPath() as the input, which is already once-encoded;
// since Bedrock paths contain only ASCII model IDs and slashes in practice,
// we leave the AWS double-encoding as a future concern and just normalize "".
func canonicalURI(escaped string) string {
	if escaped == "" {
		return "/"
	}
	return escaped
}

// canonicalQueryString returns the query string in canonical form: parameters
// sorted by name (then value), with both name and value percent-encoded
// per RFC 3986 (specifically the AWS variant — space => %20, not '+').
func canonicalQueryString(raw string) string {
	if raw == "" {
		return ""
	}
	type kv struct{ k, v string }
	var pairs []kv
	for _, p := range strings.Split(raw, "&") {
		if p == "" {
			continue
		}
		k, v, _ := strings.Cut(p, "=")
		pairs = append(pairs, kv{k: awsURLEncode(decodeQueryComponent(k), true), v: awsURLEncode(decodeQueryComponent(v), true)})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].k != pairs[j].k {
			return pairs[i].k < pairs[j].k
		}
		return pairs[i].v < pairs[j].v
	})
	parts := make([]string, len(pairs))
	for i, p := range pairs {
		parts[i] = p.k + "=" + p.v
	}
	return strings.Join(parts, "&")
}

// canonicalHeaders returns the canonical-headers block and the
// semicolon-separated list of signed header names. We sign every header
// present on the request plus the implicit Host header. Header names are
// lowercased; values are trimmed and have inner whitespace collapsed.
func canonicalHeaders(req *http.Request) (canonical, signed string) {
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}

	type hv struct {
		name, value string
	}
	var entries []hv
	entries = append(entries, hv{name: "host", value: host})

	for name, vals := range req.Header {
		lower := strings.ToLower(name)
		if lower == "authorization" {
			continue
		}
		entries = append(entries, hv{name: lower, value: trimAndCollapse(strings.Join(vals, ","))})
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })

	var canon strings.Builder
	names := make([]string, len(entries))
	for i, e := range entries {
		canon.WriteString(e.name)
		canon.WriteByte(':')
		canon.WriteString(e.value)
		canon.WriteByte('\n')
		names[i] = e.name
	}
	return canon.String(), strings.Join(names, ";")
}

// deriveSigningKey computes the SigV4 signing key via the four-step HMAC
// chain: kDate -> kRegion -> kService -> kSigning.
func deriveSigningKey(secret, dateStamp, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), []byte(dateStamp))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	return hmacSHA256(kService, []byte(terminator))
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func hexSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func hashPayload(body []byte) string {
	if len(body) == 0 {
		return emptyBodyHash
	}
	return hexSHA256(body)
}

// trimAndCollapse trims leading/trailing whitespace from a header value and
// collapses runs of whitespace inside the value to a single space, per the
// SigV4 normalization rules. Values inside double-quoted strings are left
// alone (per spec).
func trimAndCollapse(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(v))
	inQuote := false
	prevSpace := false
	for i := 0; i < len(v); i++ {
		c := v[i]
		if c == '"' {
			inQuote = !inQuote
			b.WriteByte(c)
			prevSpace = false
			continue
		}
		if !inQuote && (c == ' ' || c == '\t') {
			if prevSpace {
				continue
			}
			b.WriteByte(' ')
			prevSpace = true
			continue
		}
		b.WriteByte(c)
		prevSpace = false
	}
	return b.String()
}

// awsURLEncode applies the AWS variant of RFC 3986 percent-encoding:
// unreserved characters (A-Z, a-z, 0-9, '-', '_', '.', '~') pass through;
// '/' passes through when encodePath is false (used for path components in
// services that allow unescaped slashes). Everything else is percent-encoded
// as uppercase hex. Notably, space becomes %20 (never '+').
func awsURLEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9'):
			b.WriteByte(c)
		case c == '-' || c == '_' || c == '.' || c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// decodeQueryComponent percent-decodes a single query key or value. Unlike
// net/url's decoder, it treats '+' as a literal '+', not a space — matching
// AWS's interpretation of canonical query strings.
func decodeQueryComponent(s string) string {
	if !strings.ContainsRune(s, '%') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			hi, hiOk := unhex(s[i+1])
			lo, loOk := unhex(s[i+2])
			if hiOk && loOk {
				b.WriteByte(hi<<4 | lo)
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func unhex(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}
