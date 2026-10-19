package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go-llm-proxy/internal/config"
	"go-llm-proxy/internal/httputil"
	"go-llm-proxy/internal/lb"
	"go-llm-proxy/internal/pipeline"
)

func newProxyForModel(m config.ModelConfig) *ProxyHandler {
	cs := config.NewTestConfigStore(&config.Config{Models: []config.ModelConfig{m}})
	return NewProxyHandler(cs, nil, pipeline.NewPipeline(cs, httputil.NewHTTPClient()))
}

func slowChatRequest(stream bool) *http.Request {
	body := `{"model":"slow-model","messages":[{"role":"user","content":"hi"}]`
	if stream {
		body += `,"stream":true`
	}
	body += `}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

// A non-streaming generation whose headers arrive after 35s (slow prefill or a
// reasoning model answering in one piece) must succeed when the model timeout
// allows it: the old transport-level 30s header cap is gone.
func TestProxy_SlowHeadersSucceedWithinModelTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("waits 35s for response headers")
	}
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(35 * time.Second):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1","model":"slow-model","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`))
	}))
	defer ts.Close()

	proxy := newProxyForModel(config.ModelConfig{Name: "slow-model", Model: "slow-model", Backend: ts.URL + "/v1", Timeout: 90})
	w := httptest.NewRecorder()
	start := time.Now()
	proxy.ServeHTTP(w, slowChatRequest(false))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "done") {
		t.Fatalf("status %d after %v, body %s", w.Code, time.Since(start), w.Body.String())
	}
}

// The model timeout still frees the request when a backend never answers,
// for streaming requests as well (they are not disarmed before headers).
func TestProxy_ModelTimeoutBoundsHungBackend(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "non-streaming"
		if stream {
			name = "streaming"
		}
		t.Run(name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				select {
				case <-r.Context().Done():
				case <-time.After(4 * time.Second):
				}
			}))
			defer ts.Close()
			proxy := newProxyForModel(config.ModelConfig{Name: "slow-model", Model: "slow-model", Backend: ts.URL + "/v1", Timeout: 2})
			w := httptest.NewRecorder()
			start := time.Now()
			proxy.ServeHTTP(w, slowChatRequest(stream))
			elapsed := time.Since(start)
			if w.Code != http.StatusGatewayTimeout {
				t.Fatalf("status %d, want 504 (body %s)", w.Code, w.Body.String())
			}
			if elapsed < 1500*time.Millisecond || elapsed > 6*time.Second {
				t.Fatalf("timed out after %v, want ~2s", elapsed)
			}
			if lb.Status(ts.URL+"/v1").ConsecFails != 1 {
				t.Fatalf("hung backend should count one failure, got %d", lb.Status(ts.URL+"/v1").ConsecFails)
			}
		})
	}
}

// A 2xx event stream disarms the timeout: it may outlive the model timeout.
func TestProxy_StreamOutlivesModelTimeout(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f := w.(http.Flusher)
		f.Flush()
		time.Sleep(2500 * time.Millisecond)
		_, _ = w.Write([]byte("data: {\"id\":\"1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"late\"},\"finish_reason\":null}]}\n\ndata: [DONE]\n\n"))
		f.Flush()
	}))
	defer ts.Close()
	proxy := newProxyForModel(config.ModelConfig{Name: "slow-model", Model: "slow-model", Backend: ts.URL + "/v1", Timeout: 1})
	w := httptest.NewRecorder()
	proxy.ServeHTTP(w, slowChatRequest(true))
	if !strings.Contains(w.Body.String(), "late") {
		t.Fatalf("stream was cut by the model timeout: %d %s", w.Code, w.Body.String())
	}
}

func TestProxy_UpstreamFailureCountedOnce(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer ts.Close()
	proxy := newProxyForModel(config.ModelConfig{Name: "slow-model", Model: "slow-model", Backend: ts.URL + "/v1", Timeout: 10})
	proxy.ServeHTTP(httptest.NewRecorder(), slowChatRequest(false))
	if got := lb.Status(ts.URL + "/v1").ConsecFails; got != 1 {
		t.Fatalf("a single 5xx must count once, got %d", got)
	}

	dead := httptest.NewServer(http.NotFoundHandler())
	url := dead.URL + "/v1"
	dead.Close()
	proxy = newProxyForModel(config.ModelConfig{Name: "slow-model", Model: "slow-model", Backend: url, Timeout: 10})
	w := httptest.NewRecorder()
	proxy.ServeHTTP(w, slowChatRequest(false))
	if w.Code != http.StatusBadGateway || lb.Status(url).ConsecFails != 1 {
		t.Fatalf("transport failure: status %d, fails %d", w.Code, lb.Status(url).ConsecFails)
	}
}

// End to end: a connect:// model backend reaches the hub with the labels.
func TestProxy_ConnectBackendThroughHub(t *testing.T) {
	var mu sync.Mutex
	var gotPath, gotApp, gotFor string
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotPath, gotApp, gotFor = r.URL.Path, r.Header.Get("X-Eidonix-Application"), r.Header.Get("X-Eidonix-For")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1","model":"slow-model","choices":[{"index":0,"message":{"role":"assistant","content":"via hub"},"finish_reason":"stop"}]}`))
	}))
	defer hub.Close()
	httputil.SetConnect(hub.URL, "go-llm-proxy")
	t.Cleanup(func() { httputil.SetConnect("", "") })

	proxy := newProxyForModel(config.ModelConfig{Name: "slow-model", Model: "slow-model", Backend: "connect://site-a/glm/v1", Timeout: 10})
	req := slowChatRequest(false)
	req = req.WithContext(httputil.WithCaller(req.Context(), "alice"))
	w := httptest.NewRecorder()
	proxy.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "via hub") {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if gotPath != "/site-a/glm/v1/chat/completions" || gotApp != "go-llm-proxy" || gotFor != "alice" {
		t.Fatalf("hub saw path=%q app=%q for=%q", gotPath, gotApp, gotFor)
	}
}

func TestArmUntilHeaders(t *testing.T) {
	ctx, cancel, disarm := armUntilHeaders(t.Context(), &config.ModelConfig{Timeout: 1})
	defer cancel()
	resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream; charset=utf-8"}}}
	disarmIfStream(resp, disarm)
	select {
	case <-ctx.Done():
		t.Fatal("2xx event stream should disarm the timeout")
	case <-time.After(1300 * time.Millisecond):
	}

	ctx2, cancel2, disarm2 := armUntilHeaders(t.Context(), &config.ModelConfig{Timeout: 1})
	defer cancel2()
	disarmIfStream(&http.Response{StatusCode: 502, Header: http.Header{"Content-Type": {"text/event-stream"}}}, disarm2)
	disarmIfStream(&http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}}, disarm2)
	select {
	case <-ctx2.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("error replies and JSON bodies must stay under the timeout")
	}
}

func TestAdminConnectData(t *testing.T) {
	h, _, _ := newAdminTestHandler(t)
	w := httptest.NewRecorder()
	h.ConnectData(w, authedRequest(t, h, http.MethodGet, "/admin/connect/data", nil))
	if !strings.Contains(w.Body.String(), `"configured":false`) {
		t.Fatalf("no hub configured: %s", w.Body.String())
	}

	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"sites":[{"name":"site-a","online":true,"services":[{"name":"chat","kind":"http","healthy":true,"models":["model-a"]}]}]}`))
	}))
	defer hub.Close()
	cfg := h.cs.Get()
	cfg.Connect = &config.ConnectConfig{URL: hub.URL}
	w = httptest.NewRecorder()
	h.ConnectData(w, authedRequest(t, h, http.MethodGet, "/admin/connect/data", nil))
	body := w.Body.String()
	for _, want := range []string{`"configured":true`, `"site-a"`, `"model-a"`, `"healthy":true`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s in %s", want, body)
		}
	}
}
