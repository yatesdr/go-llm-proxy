package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go-llm-proxy/internal/config"
)

func TestCountTokens_TranslatedBackend(t *testing.T) {
	// Fake backend (unused for translated path, but needed for config).
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("should not call backend for translated estimate")
	}))
	defer backend.Close()

	cfg := &config.Config{
		Models: []config.ModelConfig{
			{Name: "test-model", Backend: backend.URL, Model: "test-model"},
		},
	}
	cs := config.NewTestConfigStore(cfg)
	h := NewCountTokensHandler(cs, nil)

	body := `{"model":"test-model","messages":[{"role":"user","content":"Hello, how are you doing today?"}]}`
	req := httptest.NewRequest("POST", "/v1/messages/count_tokens", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp.InputTokens <= 0 {
		t.Fatalf("expected positive token count, got %d", resp.InputTokens)
	}
}

func TestCountTokens_NativeBackend(t *testing.T) {
	// Fake Anthropic backend that returns a token count.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages/count_tokens" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("X-Api-Key") != "test-key" {
			t.Fatalf("expected X-Api-Key header, got %q", r.Header.Get("X-Api-Key"))
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"input_tokens": 42})
	}))
	defer backend.Close()

	cfg := &config.Config{
		Models: []config.ModelConfig{
			{Name: "claude-sonnet", Backend: backend.URL, Model: "claude-sonnet", Type: "anthropic", APIKey: "test-key", Timeout: 30},
		},
	}
	cs := config.NewTestConfigStore(cfg)
	h := NewCountTokensHandler(cs, nil)

	body := `{"model":"claude-sonnet","messages":[{"role":"user","content":"Hello"}]}`
	req := httptest.NewRequest("POST", "/v1/messages/count_tokens", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp.InputTokens != 42 {
		t.Fatalf("expected 42 tokens from native backend, got %d", resp.InputTokens)
	}
}

func TestCountTokens_UnknownModel(t *testing.T) {
	cfg := &config.Config{}
	cs := config.NewTestConfigStore(cfg)
	h := NewCountTokensHandler(cs, nil)

	body := `{"model":"nonexistent"}`
	req := httptest.NewRequest("POST", "/v1/messages/count_tokens", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}
