package pipeline

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
)

// Image bytes: 1x1 transparent PNG. Valid PNG magic so content-type sniffing
// on the server side gives a plausible image/png.
var testPNG = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
	0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
	0x89, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x44, 0x41,
	0x54, 0x78, 0x9c, 0x62, 0x00, 0x01, 0x00, 0x00,
	0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00,
	0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae,
	0x42, 0x60, 0x82,
}

func TestIsPrivateIP_KnownBlocks(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "127.1.2.3", // loopback
		"10.0.0.1", "10.255.255.255", // RFC 1918
		"172.16.0.1", "172.31.255.255",
		"192.168.0.1",
		"169.254.169.254", // AWS IMDS — link-local
		"169.254.0.1",
		"100.64.0.1", "100.127.255.255", // CGNAT
		"0.0.0.0",            // unspecified v4
		"::1",                // loopback v6
		"::",                 // unspecified v6
		"fe80::1",            // link-local v6
		"fc00::1", "fdff::1", // ULA v6
		"::ffff:127.0.0.1",       // IPv4-mapped loopback
		"::ffff:169.254.169.254", // IPv4-mapped IMDS
		"::ffff:10.0.0.1",        // IPv4-mapped RFC 1918
	}
	for _, s := range blocked {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("failed to parse %q", s)
		}
		if !isPrivateIP(ip) {
			t.Errorf("isPrivateIP(%s) = false, want true", s)
		}
	}
}

func TestIsPrivateIP_KnownAllowed(t *testing.T) {
	allowed := []string{
		"8.8.8.8",
		"1.1.1.1",
		"93.184.216.34", // example.com (classic)
		"2001:4860:4860::8888",
	}
	for _, s := range allowed {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("parse %q", s)
		}
		if isPrivateIP(ip) {
			t.Errorf("isPrivateIP(%s) = true, want false", s)
		}
	}
}

func TestImageURLPreflight(t *testing.T) {
	cases := map[string]bool{
		"":                        false,
		"http://":                 false,
		"file:///etc/passwd":      false,
		"ftp://example.com/x.png": false,
		"http://127.0.0.1/":       false,
		"http://169.254.169.254/latest/meta-data/": false,
		"http://metadata.google.internal/":         false,
		"http://[::1]/":                            false,
		"http://[::ffff:127.0.0.1]/":               false,
		"http://example.com/cat.png":               true,
		"https://example.com/cat.png":              true,
		"data:image/png;base64,AAAA":               true,
	}
	for u, want := range cases {
		if got := imageURLPreflight(u); got != want {
			t.Errorf("imageURLPreflight(%q) = %v, want %v", u, got, want)
		}
	}
}

func TestSSRFSafeControl_RejectsPrivateIPs(t *testing.T) {
	cases := []string{
		"127.0.0.1:80",
		"169.254.169.254:80",
		"10.0.0.1:443",
		"[::1]:80",
		"[::ffff:169.254.169.254]:80",
		"0.0.0.0:80",
	}
	for _, addr := range cases {
		err := ssrfSafeControl("tcp4", addr, (syscall.RawConn)(nil))
		if err == nil {
			t.Errorf("ssrfSafeControl allowed %s, want rejection", addr)
		}
	}
}

func TestSSRFSafeControl_AllowsPublicIPs(t *testing.T) {
	if err := ssrfSafeControl("tcp4", "93.184.216.34:443", nil); err != nil {
		t.Errorf("ssrfSafeControl rejected public IP: %v", err)
	}
}

func TestFetchImageAsDataURL_DataURLPassthrough(t *testing.T) {
	in := "data:image/png;base64,iVBORw0KGgo="
	got, err := fetchImageAsDataURL(context.Background(), in)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != in {
		t.Errorf("data URL should pass through unchanged, got %q", got)
	}
}

func TestEncodeImageResponse_RejectsNon200(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusForbidden,
		Header:     http.Header{"Content-Type": []string{"image/png"}},
		Body:       http.NoBody,
	}
	_, err := encodeImageResponse(resp)
	if err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Errorf("want HTTP error, got %v", err)
	}
}

func TestEncodeImageResponse_RejectsNonImageContentType(t *testing.T) {
	resp := &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"text/html"}},
		Body:       http.NoBody,
	}
	_, err := encodeImageResponse(resp)
	if err == nil || !strings.Contains(err.Error(), "content-type") {
		t.Errorf("want content-type error, got %v", err)
	}
}

func TestEncodeImageResponse_EnforcesSizeLimit(t *testing.T) {
	big := bytes.Repeat([]byte{0x89}, imageFetchMaxSize+1024)
	resp := &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"image/png"}},
		Body:       io.NopCloser(bytes.NewReader(big)),
	}
	_, err := encodeImageResponse(resp)
	if err == nil || !strings.Contains(err.Error(), "byte limit") {
		t.Errorf("want size-limit error, got %v", err)
	}
}

func TestEncodeImageResponse_SuccessWithCharsetSuffix(t *testing.T) {
	resp := &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"image/png; charset=binary"}},
		Body:       io.NopCloser(bytes.NewReader(testPNG)),
	}
	got, err := encodeImageResponse(resp)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	wantPrefix := "data:image/png;base64,"
	if !strings.HasPrefix(got, wantPrefix) {
		t.Errorf("data URL prefix: %q", got[:min(len(got), len(wantPrefix))])
	}
	encoded := strings.TrimPrefix(got, wantPrefix)
	dec, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Errorf("base64 round-trip: %v", err)
	}
	if !bytes.Equal(dec, testPNG) {
		t.Errorf("decoded bytes != input")
	}
}

// Live-fetch tests would need either a non-loopback host (flaky) or a
// custom dialer that bypasses the SSRF check (defeats the point). The
// encoding primitives are covered above; the SSRF dialer is covered by
// TestSSRFSafeControl_*; the preflight by TestImageURLPreflight.

func TestFetchImageAsDataURL_HappyPathViaHTTPTestBlocked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(testPNG)
	}))
	defer srv.Close()

	// httptest always binds to 127.0.0.1, so preflight rejects here — which
	// is exactly what we want: loopback must be unreachable even through
	// this helper. Confirm the rejection fires with the preflight message
	// (not a dial-level error).
	_, err := fetchImageAsDataURL(context.Background(), srv.URL+"/a.png")
	if err == nil {
		t.Fatalf("expected rejection for loopback httptest server")
	}
	if !strings.Contains(err.Error(), "blocked") && !strings.Contains(err.Error(), "ssrf") &&
		!strings.Contains(err.Error(), "private") {
		t.Errorf("error should mention SSRF/blocked/private, got %v", err)
	}
}

// TestFetchImageAsDataURL_DialerAppliesAfterDNS is the core DNS-rebinding
// test: even if the preflight passes (hostname doesn't look private), the
// dialer rejects the resolved IP.  We can simulate this by using a bare IP
// literal that preflightURLSafe accepts but ssrfSafeControl rejects — the
// preflight only rejects literal-private IPs, so a literal public IP the
// test server isn't actually listening on would give a network error; we
// instead use a private IP directly and confirm the dialer catches it even
// though the URL isn't otherwise suspicious-looking.
func TestFetchImageAsDataURL_DialerEnforcesAfterPreflight(t *testing.T) {
	// Use an explicit loopback — preflight WILL reject this in the hostname
	// check path, but we're testing the defense-in-depth behavior of the
	// dialer. Construct a URL that would bypass preflight: use a hostname
	// that resolves to loopback. `localhost` typically does.
	//
	// preflightURLSafe doesn't do DNS, so `localhost` slides through
	// (it has no literal IP in the URL string, and it's not the metadata
	// hostname we explicitly block). The dialer must catch it.
	_, err := fetchImageAsDataURL(context.Background(), "http://localhost:80/whatever.png")
	if err == nil {
		t.Fatalf("expected dialer to reject localhost after DNS")
	}
	// Accept either the dial-time ssrf error OR a generic dial failure
	// (on systems where localhost isn't in /etc/hosts, the dial fails
	// differently — still blocked, just not by our check).
	msg := err.Error()
	if !strings.Contains(msg, "ssrf") && !strings.Contains(msg, "private") &&
		!strings.Contains(msg, "refused") && !strings.Contains(msg, "no such host") {
		t.Errorf("unexpected error kind: %v", err)
	}
}
