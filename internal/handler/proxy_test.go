package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go-llm-proxy/internal/config"
)

func TestExtractModelFromJSON_Valid(t *testing.T) {
	body := []byte(`{"model":"gpt-4","messages":[]}`)
	if got := ExtractModelFromJSON(body); got != "gpt-4" {
		t.Fatalf("expected gpt-4, got: %q", got)
	}
}

func TestExtractModelFromJSON_Missing(t *testing.T) {
	body := []byte(`{"messages":[]}`)
	if got := ExtractModelFromJSON(body); got != "" {
		t.Fatalf("expected empty, got: %q", got)
	}
}

func TestExtractModelFromJSON_Invalid(t *testing.T) {
	body := []byte(`not json`)
	if got := ExtractModelFromJSON(body); got != "" {
		t.Fatalf("expected empty for invalid JSON, got: %q", got)
	}
}

func TestExtractModelFromJSON_Empty(t *testing.T) {
	body := []byte(`{}`)
	if got := ExtractModelFromJSON(body); got != "" {
		t.Fatalf("expected empty for empty object, got: %q", got)
	}
}

func TestRewriteModelName_Replaces(t *testing.T) {
	body := []byte(`{"model":"old-model","temperature":0.7}`)
	result := RewriteModelName(body, "new-model")

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
	result := RewriteModelName(body, "new")

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
	result := RewriteModelName(body, "new")
	if string(result) != string(body) {
		t.Fatalf("expected original body returned for invalid JSON")
	}
}

func TestRewriteModelName_NoModelField(t *testing.T) {
	body := []byte(`{"temperature":0.7}`)
	result := RewriteModelName(body, "new")
	if string(result) != string(body) {
		t.Fatalf("expected original body when no model field present")
	}
}

func TestFindModel_Found(t *testing.T) {
	cfg := &config.Config{
		Models: []config.ModelConfig{
			{Name: "model-a", Backend: "http://localhost/v1"},
			{Name: "model-b", Backend: "http://localhost/v1"},
		},
	}

	m := config.FindModel(cfg, "model-b")
	if m == nil || m.Name != "model-b" {
		t.Fatalf("expected model-b, got: %v", m)
	}
}

func TestFindModel_NotFound(t *testing.T) {
	cfg := &config.Config{
		Models: []config.ModelConfig{
			{Name: "model-a", Backend: "http://localhost/v1"},
		},
	}

	m := config.FindModel(cfg, "nonexistent")
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
		// /v1/messages is now handled by MessagesHandler, not ProxyHandler.
	}
	for _, p := range allowed {
		if !AllowedPaths.MatchString(p) {
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
		if AllowedPaths.MatchString(p) {
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
	if modelType == config.BackendAnthropic {
		backend = ts.URL
	}

	cfg := &config.Config{
		Models: []config.ModelConfig{
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
	cs := config.NewTestConfigStore(cfg)
	return NewProxyHandler(cs, nil, nil), ts
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

// Anthropic Messages API tests moved to messages_test.go (MessagesHandler).

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

	// OpenAI backend has /v1 in base URL, proxy strips /v1 -> upstream sees /v1/chat/completions
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("expected upstream path /v1/chat/completions, got: %q", gotPath)
	}
}

// Anthropic path/prefix tests moved to messages_test.go (MessagesHandler).
