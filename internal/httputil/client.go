package httputil

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// NewHTTPClient returns the standard HTTP client used for upstream calls.
//
//  1. No redirect following. A compromised or misconfigured backend could 3xx
//     the proxy into an internal address; redirects are refused outright and
//     the caller decides.
//
//  2. One shared transport. Every client returned here uses the same
//     connection pool, so keep-alive connections are reused across handlers
//     and requests instead of each call opening its own sockets. Callers may
//     set Client.Timeout on their own copy; the transport itself is shared.
//
//  3. Dial and TLS-handshake timeouts, but NO response-header timeout.
//     Inference calls legitimately take a long time to produce headers (a
//     slow prefill, or a non-streaming generation that returns everything at
//     once). The wait is bounded by the per-model timeout, which callers
//     arm on the request context (see handler.armUntilHeaders), not by the
//     transport.
//
//  4. connect:// URLs (see connect.go) are resolved to the Eidonix connect
//     hub and labelled at this layer, so every caller that uses these clients
//     supports them without any per-call-site code.
//
// The SSRF-safe fetcher for user-supplied URLs (internal/pipeline/safe_http.go)
// is deliberately separate and never goes through here.
func NewHTTPClient() *http.Client {
	return newClient(0)
}

// NewHTTPClientWithResponseHeaderTimeout returns a client whose transport also
// enforces a response-header timeout. Prefer NewHTTPClient plus a request
// context deadline; this exists for callers that want a transport-level bound
// as well. Transports are cached per timeout value, so repeated calls with the
// same value share one connection pool.
func NewHTTPClientWithResponseHeaderTimeout(headerTimeout time.Duration) *http.Client {
	if headerTimeout < 0 {
		headerTimeout = 0
	}
	return newClient(headerTimeout)
}

func newClient(headerTimeout time.Duration) *http.Client {
	return &http.Client{
		Transport: sharedTransport(headerTimeout),
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

var (
	transportsMu sync.Mutex
	transports   = map[time.Duration]http.RoundTripper{}
)

func sharedTransport(headerTimeout time.Duration) http.RoundTripper {
	transportsMu.Lock()
	defer transportsMu.Unlock()
	if rt, ok := transports[headerTimeout]; ok {
		return rt
	}
	rt := &connectRoundTripper{base: &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: headerTimeout, // 0 = none
		ExpectContinueTimeout: 1 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   32,
		ForceAttemptHTTP2:     true,
		// Detect dead HTTP/2 connections with pings rather than a header
		// timeout: an idle-but-healthy connection stays up, a half-dead
		// one is dropped within PingTimeout.
		HTTP2: &http.HTTP2Config{
			SendPingTimeout: 30 * time.Second,
			PingTimeout:     15 * time.Second,
		},
	}}
	transports[headerTimeout] = rt
	return rt
}
