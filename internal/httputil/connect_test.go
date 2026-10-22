package httputil

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestResolveConnectURL(t *testing.T) {
	SetConnect("http://hub.example.com:8080/", "")
	t.Cleanup(func() { SetConnect("", "") })
	cases := []struct{ in, want string }{
		{"connect://site-a/glm/v1", "http://hub.example.com:8080/site-a/glm/v1"},
		{"connect://site-a/glm/v1/", "http://hub.example.com:8080/site-a/glm/v1/"},
		{"connect://site-a/ocr", "http://hub.example.com:8080/site-a/ocr"},
		{"connect://site-a/glm/v1/chat/completions?stream=1", "http://hub.example.com:8080/site-a/glm/v1/chat/completions?stream=1"},
		{"Connect://site-a/glm/a%2Fb", "http://hub.example.com:8080/site-a/glm/a%2Fb"},
	}
	for _, c := range cases {
		got, err := ResolveConnectURL(c.in)
		if err != nil || got != c.want {
			t.Errorf("ResolveConnectURL(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
	for _, bad := range []string{"connect://", "connect://site-a", "connect://site-a/", "connect:///glm/v1", "connect://u:p@site-a/glm"} {
		if _, err := ResolveConnectURL(bad); err == nil {
			t.Errorf("ResolveConnectURL(%q) should fail", bad)
		}
	}
	// Plain URLs are left alone.
	if got := ResolveBackendURL("http://localhost:8000/v1"); got != "http://localhost:8000/v1" {
		t.Errorf("plain URL changed: %q", got)
	}
}

func TestConnectDefaults(t *testing.T) {
	SetConnect("", "")
	c := Connect()
	if c.URL != "http://connect:8080" || c.Application != "go-llm-proxy" {
		t.Fatalf("defaults = %+v", c)
	}
}

func TestSanitizeLabel(t *testing.T) {
	if got := SanitizeLabel("  Alice\tSmithé\r\nx "); got != "AliceSmithx" {
		t.Errorf("got %q", got)
	}
	if got := SanitizeLabel(strings.Repeat("a", 200)); len(got) != 64 {
		t.Errorf("len = %d, want 64", len(got))
	}
}

// hubRecorder is a stand-in for the connect hub that records what it receives.
type hubRecorder struct {
	mu   sync.Mutex
	reqs []*http.Request
}

func (h *hubRecorder) handler(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	h.reqs = append(h.reqs, r.Clone(context.Background()))
	h.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true}`))
}

func TestConnectTransportResolvesAndLabels(t *testing.T) {
	rec := &hubRecorder{}
	hub := httptest.NewServer(http.HandlerFunc(rec.handler))
	defer hub.Close()
	SetConnect(hub.URL, "my-proxy")
	t.Cleanup(func() { SetConnect("", "") })

	client := NewHTTPClient()
	ctx := WithCaller(context.Background(), "Alice é Smith")
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "connect://site-a/glm/v1/chat/completions?x=1", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer abc")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if len(rec.reqs) != 1 {
		t.Fatalf("hub saw %d requests", len(rec.reqs))
	}
	got := rec.reqs[0]
	if got.URL.Path != "/site-a/glm/v1/chat/completions" || got.URL.RawQuery != "x=1" {
		t.Errorf("hub saw %s?%s", got.URL.Path, got.URL.RawQuery)
	}
	if got.Header.Get("X-Eidonix-Application") != "my-proxy" {
		t.Errorf("application label = %q", got.Header.Get("X-Eidonix-Application"))
	}
	if got.Header.Get("X-Eidonix-For") != "Alice  Smith" {
		t.Errorf("for label = %q", got.Header.Get("X-Eidonix-For"))
	}
	if got.Header.Get("Authorization") != "Bearer abc" {
		t.Errorf("Authorization not forwarded")
	}
}

func TestPlainBackendsGetNoLabels(t *testing.T) {
	rec := &hubRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(rec.handler))
	defer srv.Close()

	ctx := WithCaller(context.Background(), "Alice")
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/models", nil)
	resp, err := NewHTTPClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	h := rec.reqs[0].Header
	if h.Get("X-Eidonix-Application") != "" || h.Get("X-Eidonix-For") != "" {
		t.Errorf("plain backend received labels: %v", h)
	}
}

func TestConnectNoCallerOmitsForLabel(t *testing.T) {
	rec := &hubRecorder{}
	hub := httptest.NewServer(http.HandlerFunc(rec.handler))
	defer hub.Close()
	SetConnect(hub.URL, "")
	t.Cleanup(func() { SetConnect("", "") })

	req, _ := http.NewRequest(http.MethodGet, "connect://site-a/glm/v1/models", nil)
	req.Header.Set("X-Eidonix-For", "spoofed")
	resp, err := NewHTTPClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	h := rec.reqs[0].Header
	if h.Get("X-Eidonix-For") != "" || h.Get("X-Eidonix-Application") != "go-llm-proxy" {
		t.Errorf("labels = %v", h)
	}
}

func TestInvalidConnectURLFailsRequest(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "connect://site-a", nil)
	if resp, err := NewHTTPClient().Do(req); err == nil {
		resp.Body.Close()
		t.Fatal("expected error for connect URL without a service")
	}
}

func TestClientsShareTransport(t *testing.T) {
	a, b := NewHTTPClient(), NewHTTPClient()
	if a.Transport != b.Transport {
		t.Fatal("NewHTTPClient must reuse one transport")
	}
	if NewHTTPClientWithResponseHeaderTimeout(5e9).Transport != NewHTTPClientWithResponseHeaderTimeout(5e9).Transport {
		t.Fatal("same header timeout must reuse one transport")
	}
	rt := a.Transport.(*connectRoundTripper).base.(*http.Transport)
	if rt.ResponseHeaderTimeout != 0 {
		t.Fatalf("default transport must not cap response headers, got %v", rt.ResponseHeaderTimeout)
	}
	if a.CheckRedirect == nil {
		t.Fatal("redirects must stay disabled")
	}
}

func TestConnectAffinityHeader(t *testing.T) {
	rec := &hubRecorder{}
	hub := httptest.NewServer(http.HandlerFunc(rec.handler))
	defer hub.Close()
	SetConnect(hub.URL, "")
	t.Cleanup(func() { SetConnect("", "") })

	do := func(ctx context.Context, url string) http.Header {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(`{}`))
		req.Header.Set(HeaderAffinity, "spoofed")
		resp, err := NewHTTPClient().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return rec.reqs[len(rec.reqs)-1].Header
	}
	if got := do(WithAffinity(context.Background(), 0x9f2c), "connect://site-a/glm/v1/chat/completions").Get(HeaderAffinity); got != "0000000000009f2c" {
		t.Errorf("affinity header = %q", got)
	}
	// No key: a client-sent value never reaches the site.
	if got := do(context.Background(), "connect://site-a/glm/v1/chat/completions").Get(HeaderAffinity); got != "" {
		t.Errorf("affinity header without a key = %q", got)
	}
	// Plain backends are chosen here, not at a site: they get no key from us.
	if got := do(WithAffinity(context.Background(), 0x9f2c), hub.URL+"/v1/chat/completions").Get(HeaderAffinity); got != "spoofed" {
		t.Errorf("plain backend affinity header changed: %q", got)
	}
}
