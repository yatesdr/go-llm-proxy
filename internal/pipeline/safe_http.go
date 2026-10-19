package pipeline

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// SSRF protection for outbound image fetches.
//
// The naive approach — resolve the hostname, check the IP, pass the URL to
// the upstream model — is vulnerable to DNS rebinding: a short-TTL hostname
// can resolve to a public IP during our pre-check, then to 169.254.169.254
// (cloud instance metadata) when the upstream model fetches it. The check
// has no effect on the second resolution.
//
// The fix here is structural, not additional filtering: we fetch the image
// in the proxy using a dialer that validates the *resolved* IP at dial time
// (via net.Dialer.Control, which fires after DNS but before connect), then
// pass a data: URL to the upstream model. The upstream never sees the
// remote hostname and never makes an outbound request of its own.
//
// safeHTTPClient is the client used for those fetches. It has short
// timeouts, refuses redirects, and enforces the IP allowlist at dial time.

const (
	imageFetchTimeout = 15 * time.Second
	imageFetchMaxSize = 10 * 1024 * 1024 // 10 MB
)

var safeHTTPClient = &http.Client{
	Timeout: imageFetchTimeout,
	Transport: &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, Control: ssrfSafeControl}).DialContext,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		IdleConnTimeout:       30 * time.Second,
		MaxIdleConns:          10,
	},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		// Redirects are an SSRF amplifier — a public URL 302's to an internal
		// one, and the second Dial re-hits Control which re-validates, but we
		//'d rather fail loudly than chase them.
		return http.ErrUseLastResponse
	},
}

// ssrfSafeControl is a net.Dialer.Control function that rejects connections
// to private/loopback/metadata addresses *after* DNS resolution. It runs
// on every physical connection attempt, so DNS rebinding cannot slip a
// second resolution past it.
func ssrfSafeControl(network, address string, c syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("ssrf: invalid dial address %q", address)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("ssrf: unresolved host %q at dial time", host)
	}
	if isPrivateIP(ip) {
		return fmt.Errorf("ssrf: refusing to dial private address %s", ip)
	}
	return nil
}

// fetchImageAsDataURL fetches an http(s) image URL through the SSRF-safe
// client and returns a data: URL that can be passed to an upstream vision
// model without the upstream making any outbound request of its own.
//
// data: inputs are returned unchanged (nothing to fetch).
//
// The fetch is size-limited and short-timeout; large or slow origins fail
// with a clear error so the caller can skip the image gracefully.
func fetchImageAsDataURL(ctx context.Context, imageURL string) (string, error) {
	if strings.HasPrefix(imageURL, "data:") {
		return imageURL, nil
	}

	parsed, err := url.Parse(imageURL)
	if err != nil {
		return "", fmt.Errorf("parse image url: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("unsupported image scheme %q", parsed.Scheme)
	}

	// Pre-flight check: short-circuit obviously unsafe hosts before a dial.
	// This is defense-in-depth; the dial-time check in ssrfSafeControl is
	// authoritative.
	if !preflightURLSafe(parsed) {
		return "", errors.New("image URL targets a blocked host")
	}

	fetchCtx, cancel := context.WithTimeout(ctx, imageFetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, imageURL, nil)
	if err != nil {
		return "", fmt.Errorf("build image request: %w", err)
	}
	req.Header.Set("Accept", "image/*")
	req.Header.Set("User-Agent", "go-llm-proxy/image-fetch")

	resp, err := safeHTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch image: %w", err)
	}
	defer resp.Body.Close()

	return encodeImageResponse(resp)
}

// encodeImageResponse validates that an HTTP response looks like an image
// of bounded size and converts its body into a data: URL. Factored out so
// the validation and encoding logic can be unit-tested without going
// through the SSRF-safe dialer (which blocks loopback test servers).
func encodeImageResponse(resp *http.Response) (string, error) {
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("image fetch returned HTTP %d", resp.StatusCode)
	}
	contentType := resp.Header.Get("Content-Type")
	if i := strings.IndexByte(contentType, ';'); i >= 0 {
		contentType = strings.TrimSpace(contentType[:i])
	}
	if !strings.HasPrefix(contentType, "image/") {
		return "", fmt.Errorf("unexpected content-type %q for image fetch", contentType)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, imageFetchMaxSize+1))
	if err != nil {
		return "", fmt.Errorf("read image body: %w", err)
	}
	if int64(len(body)) > imageFetchMaxSize {
		return "", fmt.Errorf("image exceeds %d byte limit", imageFetchMaxSize)
	}
	return "data:" + contentType + ";base64," + base64.StdEncoding.EncodeToString(body), nil
}

// preflightURLSafe rejects URLs whose hostname component alone makes them
// obviously unsafe (well-known metadata names, literal private IPs in the
// URL itself). It does NOT do DNS resolution — that would re-open the
// rebinding window this file is designed to close. The dialer does the
// real check.
func preflightURLSafe(u *url.URL) bool {
	host := u.Hostname()
	if host == "" {
		return false
	}
	if host == "metadata.google.internal" {
		return false
	}
	if ip := net.ParseIP(host); ip != nil && isPrivateIP(ip) {
		return false
	}
	return true
}

const (
	documentFetchMaxSize = 32 * 1024 * 1024 // 32 MB
	documentFetchTimeout = 30 * time.Second
)

// documentHTTPClient fetches url-sourced documents. It is the SSRF-safe
// client; tests substitute a plain client because the safe dialer (rightly)
// refuses loopback test servers.
var documentHTTPClient = safeHTTPClient

// fetchDocument downloads an http(s) document through the SSRF-safe client,
// capped at documentFetchMaxSize. It returns the body and the bare
// content-type (parameters stripped).
func fetchDocument(ctx context.Context, rawURL string) ([]byte, string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, "", fmt.Errorf("parse document url: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, "", fmt.Errorf("unsupported document scheme %q", parsed.Scheme)
	}
	if !preflightURLSafe(parsed) {
		return nil, "", errors.New("document URL targets a blocked host")
	}

	fetchCtx, cancel := context.WithTimeout(ctx, documentFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", fmt.Errorf("build document request: %w", err)
	}
	req.Header.Set("User-Agent", "go-llm-proxy/document-fetch")

	client := documentHTTPClient
	if client.Timeout < documentFetchTimeout {
		// The shared client's 15 s cap suits images; documents get longer.
		c := *client
		c.Timeout = documentFetchTimeout
		client = &c
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("fetch document: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("document fetch returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, documentFetchMaxSize+1))
	if err != nil {
		return nil, "", fmt.Errorf("read document body: %w", err)
	}
	if int64(len(body)) > documentFetchMaxSize {
		return nil, "", fmt.Errorf("document exceeds %d byte limit", documentFetchMaxSize)
	}
	contentType := resp.Header.Get("Content-Type")
	if i := strings.IndexByte(contentType, ';'); i >= 0 {
		contentType = contentType[:i]
	}
	return body, strings.TrimSpace(strings.ToLower(contentType)), nil
}
