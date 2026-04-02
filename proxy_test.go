package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExtractModelFromJSON_Valid(t *testing.T) {
	body := []byte(`{"model":"gpt-4","messages":[]}`)
	if got := extractModelFromJSON(body); got != "gpt-4" {
		t.Fatalf("expected gpt-4, got: %q", got)
	}
}

func TestExtractModelFromJSON_Missing(t *testing.T) {
	body := []byte(`{"messages":[]}`)
	if got := extractModelFromJSON(body); got != "" {
		t.Fatalf("expected empty, got: %q", got)
	}
}

func TestExtractModelFromJSON_Invalid(t *testing.T) {
	body := []byte(`not json`)
	if got := extractModelFromJSON(body); got != "" {
		t.Fatalf("expected empty for invalid JSON, got: %q", got)
	}
}

func TestExtractModelFromJSON_Empty(t *testing.T) {
	body := []byte(`{}`)
	if got := extractModelFromJSON(body); got != "" {
		t.Fatalf("expected empty for empty object, got: %q", got)
	}
}

func TestRewriteModelName_Replaces(t *testing.T) {
	body := []byte(`{"model":"old-model","temperature":0.7}`)
	result := rewriteModelName(body, "new-model")

	var m map[string]json.RawMessage
	if err := json.Unmarshal(result, &m); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}

	var model string
	if err := json.Unmarshal(m["model"], &model); err != nil {
		t.Fatalf("failed to unmarshal model: %v", err)
	}
	if model != "new-model" {
		t.Fatalf("expected new-model, got: %q", model)
	}
}

func TestRewriteModelName_PreservesOtherFields(t *testing.T) {
	body := []byte(`{"model":"old","temperature":0.7,"max_tokens":100}`)
	result := rewriteModelName(body, "new")

	var m map[string]json.RawMessage
	if err := json.Unmarshal(result, &m); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}

	if _, ok := m["temperature"]; !ok {
		t.Fatal("temperature field was lost")
	}
	if _, ok := m["max_tokens"]; !ok {
		t.Fatal("max_tokens field was lost")
	}
}

func TestRewriteModelName_InvalidJSON(t *testing.T) {
	body := []byte(`not json`)
	result := rewriteModelName(body, "new")
	if string(result) != string(body) {
		t.Fatalf("expected original body returned for invalid JSON")
	}
}

func TestRewriteModelName_NoModelField(t *testing.T) {
	body := []byte(`{"temperature":0.7}`)
	result := rewriteModelName(body, "new")
	if string(result) != string(body) {
		t.Fatalf("expected original body when no model field present")
	}
}

func TestFindModel_Found(t *testing.T) {
	cfg := &Config{
		Models: []ModelConfig{
			{Name: "model-a", Backend: "http://localhost/v1"},
			{Name: "model-b", Backend: "http://localhost/v1"},
		},
	}

	m := findModel(cfg, "model-b")
	if m == nil || m.Name != "model-b" {
		t.Fatalf("expected model-b, got: %v", m)
	}
}

func TestFindModel_NotFound(t *testing.T) {
	cfg := &Config{
		Models: []ModelConfig{
			{Name: "model-a", Backend: "http://localhost/v1"},
		},
	}

	m := findModel(cfg, "nonexistent")
	if m != nil {
		t.Fatalf("expected nil, got: %v", m)
	}
}

func TestAllowedPaths(t *testing.T) {
	allowed := []string{
		"/v1/chat/completions",
		"/v1/completions",
		"/v1/embeddings",
		"/v1/images/generations",
		"/v1/audio/transcriptions",
		"/v1/audio/translations",
		"/v1/audio/speech",
		"/v1/messages",
	}
	for _, p := range allowed {
		if !allowedPaths.MatchString(p) {
			t.Errorf("expected %q to be allowed", p)
		}
	}

	denied := []string{
		"/v1/models",
		"/v1/fine-tunes",
		"/v1/chat/completions/extra",
		"/v2/chat/completions",
		"/v1/",
		"/",
	}
	for _, p := range denied {
		if allowedPaths.MatchString(p) {
			t.Errorf("expected %q to be denied", p)
		}
	}
}

// newTestProxyHandler creates a ProxyHandler backed by a fake upstream server.
// The upstream handler receives the proxied request for assertions.
// OpenAI backends get /v1 in the base URL; Anthropic backends do not (matching real conventions).
func newTestProxyHandler(t *testing.T, modelType string, upstream http.HandlerFunc) (*ProxyHandler, *httptest.Server) {
	t.Helper()
	ts := httptest.NewServer(upstream)

	backend := ts.URL + "/v1"
	if modelType == BackendAnthropic {
		backend = ts.URL
	}

	cfg := &Config{
		Models: []ModelConfig{
			{
				Name:    "test-model",
				Backend: backend,
				APIKey:  "backend-secret",
				Model:   "test-model",
				Timeout: 10,
				Type:    modelType,
			},
		},
	}
	cs := &ConfigStore{config: cfg}
	return NewProxyHandler(cs, nil), ts
}

func TestProxyHandler_OpenAIAuthHeader(t *testing.T) {
	var gotAuth string
	proxy, ts := newTestProxyHandler(t, "", func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	})
	defer ts.Close()

	body := strings.NewReader(`{"model":"test-model","messages":[]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	proxy.ServeHTTP(w, req)

	if gotAuth != "Bearer backend-secret" {
		t.Fatalf("expected Bearer auth, got: %q", gotAuth)
	}
}

func TestProxyHandler_AnthropicAuthHeader(t *testing.T) {
	var gotAPIKey, gotAuth string
	proxy, ts := newTestProxyHandler(t, BackendAnthropic, func(w http.ResponseWriter, r *http.Request) {
		gotAPIKey = r.Header.Get("X-Api-Key")
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	})
	defer ts.Close()

	body := strings.NewReader(`{"model":"test-model","max_tokens":100,"messages":[]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	proxy.ServeHTTP(w, req)

	if gotAPIKey != "backend-secret" {
		t.Fatalf("expected x-api-key=backend-secret, got: %q", gotAPIKey)
	}
	if gotAuth != "" {
		t.Fatalf("expected no Authorization header for anthropic, got: %q", gotAuth)
	}
}

func TestProxyHandler_AnthropicHeadersForwarded(t *testing.T) {
	var gotVersion, gotBeta string
	proxy, ts := newTestProxyHandler(t, BackendAnthropic, func(w http.ResponseWriter, r *http.Request) {
		gotVersion = r.Header.Get("Anthropic-Version")
		gotBeta = r.Header.Get("Anthropic-Beta")
		w.WriteHeader(http.StatusOK)
	})
	defer ts.Close()

	body := strings.NewReader(`{"model":"test-model","max_tokens":100,"messages":[]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("Anthropic-Beta", "prompt-caching-2024-07-31")
	w := httptest.NewRecorder()
	proxy.ServeHTTP(w, req)

	if gotVersion != "2023-06-01" {
		t.Fatalf("expected anthropic-version forwarded, got: %q", gotVersion)
	}
	if gotBeta != "prompt-caching-2024-07-31" {
		t.Fatalf("expected anthropic-beta forwarded, got: %q", gotBeta)
	}
}

func TestProxyHandler_AnthropicResponseHeaders(t *testing.T) {
	proxy, ts := newTestProxyHandler(t, BackendAnthropic, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Request-Id", "req_abc123")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"id":"msg_123"}`)
	})
	defer ts.Close()

	body := strings.NewReader(`{"model":"test-model","max_tokens":100,"messages":[]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	proxy.ServeHTTP(w, req)

	if got := w.Header().Get("Request-Id"); got != "req_abc123" {
		t.Fatalf("expected Request-Id header forwarded, got: %q", got)
	}
}

func TestProxyHandler_OpenAIUpstreamPath(t *testing.T) {
	var gotPath string
	proxy, ts := newTestProxyHandler(t, "", func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	})
	defer ts.Close()

	body := strings.NewReader(`{"model":"test-model","messages":[]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	proxy.ServeHTTP(w, req)

	// OpenAI backend has /v1 in base URL, proxy strips /v1 → upstream sees /v1/chat/completions
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("expected upstream path /v1/chat/completions, got: %q", gotPath)
	}
}

func TestProxyHandler_AnthropicUpstreamPath(t *testing.T) {
	var gotPath string
	proxy, ts := newTestProxyHandler(t, BackendAnthropic, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	})
	defer ts.Close()

	body := strings.NewReader(`{"model":"test-model","max_tokens":100,"messages":[]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	proxy.ServeHTTP(w, req)

	// Anthropic backend has no /v1 in base URL, proxy keeps full path → upstream sees /v1/messages
	if gotPath != "/v1/messages" {
		t.Fatalf("expected upstream path /v1/messages, got: %q", gotPath)
	}
}

func TestProxyHandler_AnthropicPrefixPath(t *testing.T) {
	var gotPath string
	proxy, ts := newTestProxyHandler(t, BackendAnthropic, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	})
	defer ts.Close()

	body := strings.NewReader(`{"model":"test-model","max_tokens":100,"messages":[]}`)
	req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	proxy.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got: %d", w.Code)
	}
	if gotPath != "/v1/messages" {
		t.Fatalf("expected upstream path /v1/messages, got: %q", gotPath)
	}
}

func TestProxyHandler_AnthropicPrefixRejectsOpenAI(t *testing.T) {
	proxy, ts := newTestProxyHandler(t, "", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	defer ts.Close()

	body := strings.NewReader(`{"model":"test-model","messages":[]}`)
	req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	proxy.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for openai model on /anthropic path, got: %d", w.Code)
	}
}
