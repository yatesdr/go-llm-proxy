package handler

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"go-llm-proxy/internal/auth"
	"go-llm-proxy/internal/config"
)

func TestAliasRequestsAcrossModelHandlers(t *testing.T) {
	var mu sync.Mutex
	var upstreamModels []string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("decode upstream request: %v", err)
		}
		mu.Lock()
		upstreamModels = append(upstreamModels, req.Model)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-alias", "model": "upstream-model",
			"choices": []map[string]any{{
				"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": "ok"},
			}},
			"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
		})
	}))
	defer backend.Close()

	cfg := &config.Config{
		Models: []config.ModelConfig{{
			Name: "canonical", Model: "upstream-model", Backend: backend.URL + "/v1",
			Timeout: 10, ResponsesMode: config.ResponsesModeTranslate,
		}},
		Aliases: map[string]string{"reviewer": "canonical"},
		Keys:    []config.KeyConfig{{Key: "restricted", Models: []string{"canonical"}}},
	}
	cs := config.NewTestConfigStore(cfg)

	tests := []struct {
		name, path, body string
		handler          http.Handler
	}{
		{"chat/completions", "/v1/chat/completions", `{"model":"reviewer","messages":[{"role":"user","content":"hi"}]}`, NewProxyHandler(cs, nil, nil)},
		{"responses", "/v1/responses", `{"model":"reviewer","input":"hi","stream":false}`, NewResponsesHandler(cs, nil, nil)},
		{"messages", "/v1/messages", `{"model":"reviewer","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, NewMessagesHandler(cs, nil, nil)},
		{"count_tokens", "/v1/messages/count_tokens", `{"model":"reviewer","messages":[{"role":"user","content":"hi"}]}`, NewCountTokensHandler(cs, nil)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tt.path, strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer restricted")
			w := httptest.NewRecorder()
			auth.AuthMiddleware(cs, tt.handler).ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
			}
		})
	}

	mu.Lock()
	defer mu.Unlock()
	if len(upstreamModels) != 3 {
		t.Fatalf("upstream requests = %d, want 3", len(upstreamModels))
	}
	for _, got := range upstreamModels {
		if got != "upstream-model" {
			t.Errorf("upstream model = %q, want upstream-model", got)
		}
	}
}

func TestUnknownModelsCapturedAcrossHandlers(t *testing.T) {
	cfg := &config.Config{Models: []config.ModelConfig{{Name: "known", Backend: "http://localhost:1/v1"}}}
	cs := config.NewTestConfigStore(cfg)
	tests := []struct {
		id, endpoint, path, body string
		handler                  http.Handler
	}{
		{"unknown-chat", "chat/completions", "/v1/chat/completions", `{"model":"unknown-chat","messages":[]}`, NewProxyHandler(cs, nil, nil)},
		{"unknown-responses", "responses", "/v1/responses", `{"model":"unknown-responses","input":"hi"}`, NewResponsesHandler(cs, nil, nil)},
		{"unknown-messages", "messages", "/v1/messages", `{"model":"unknown-messages","max_tokens":1,"messages":[]}`, NewMessagesHandler(cs, nil, nil)},
		{"unknown-count", "count_tokens", "/v1/messages/count_tokens", `{"model":"unknown-count"}`, NewCountTokensHandler(cs, nil)},
	}
	for _, tt := range tests {
		Remove(tt.id)
		t.Cleanup(func() { Remove(tt.id) })
		req := httptest.NewRequest(http.MethodPost, tt.path, strings.NewReader(tt.body))
		w := httptest.NewRecorder()
		tt.handler.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Fatalf("%s status = %d", tt.id, w.Code)
		}
	}

	entries := Snapshot()
	byID := make(map[string]UnknownModel, len(entries))
	for _, entry := range entries {
		byID[entry.ID] = entry
	}
	for _, tt := range tests {
		entry, ok := byID[tt.id]
		if !ok {
			t.Errorf("missing registry entry %q", tt.id)
			continue
		}
		if entry.Count != 1 || entry.LastEndpoint != tt.endpoint {
			t.Errorf("entry %q = %+v", tt.id, entry)
		}
	}
}
