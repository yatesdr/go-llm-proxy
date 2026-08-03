package config

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDetectOpenAI(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"id": "test-model", "max_model_len": 131072},
			},
		})
	}))
	defer ts.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	ctx, err := detectOpenAI(client, ModelConfig{Backend: ts.URL + "/v1", Model: "test-model"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ctx != 131072 {
		t.Fatalf("expected 131072, got %d", ctx)
	}
}

func TestDetectOpenAI_ModelNotFound(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Multiple models — no single-model fallback applies.
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"id": "model-a", "max_model_len": 8192},
				{"id": "model-b", "max_model_len": 16384},
			},
		})
	}))
	defer ts.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	_, err := detectOpenAI(client, ModelConfig{Backend: ts.URL + "/v1", Model: "nonexistent"})
	if err == nil {
		t.Fatal("expected error for model not found")
	}
}

func TestDetectOpenAI_SingleModelFallback(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"id": "internal-name", "max_model_len": 65536},
			},
		})
	}))
	defer ts.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	ctx, err := detectOpenAI(client, ModelConfig{Backend: ts.URL + "/v1", Model: "friendly-name"})
	if err != nil {
		t.Fatalf("expected single-model fallback, got error: %v", err)
	}
	if ctx != 65536 {
		t.Fatalf("expected 65536, got %d", ctx)
	}
}

func TestDetectAnthropic(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "test-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"id":               "claude-test",
			"max_input_tokens": 200000,
			"max_tokens":       8192,
		})
	}))
	defer ts.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	ctx, err := detectAnthropic(client, ModelConfig{Backend: ts.URL, Model: "claude-test", APIKey: "test-key", Type: BackendAnthropic})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ctx != 200000 {
		t.Fatalf("expected 200000, got %d", ctx)
	}
}

func TestDetectContextWindows_SkipsConfigured(t *testing.T) {
	cfg := &Config{
		Models: []ModelConfig{
			{Name: "test", Backend: "http://localhost:9999/v1", Model: "test", ContextWindow: 99999, Timeout: 300},
		},
	}
	cs := &ConfigStore{config: cfg}

	// Should not attempt any network calls (backend is unreachable).
	DetectContextWindows(cs)

	// Value should be unchanged.
	if cs.Get().Models[0].ContextWindow != 99999 {
		t.Fatalf("expected configured value preserved, got %d", cs.Get().Models[0].ContextWindow)
	}
}
