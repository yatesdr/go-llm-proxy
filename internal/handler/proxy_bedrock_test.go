package handler

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go-llm-proxy/internal/api"
	"go-llm-proxy/internal/config"
)

func newBedrockProxyHandler(t *testing.T, modelID string, upstream http.HandlerFunc) (*ProxyHandler, *httptest.Server) {
	t.Helper()
	ts := httptest.NewServer(upstream)
	cfg := &config.Config{
		Models: []config.ModelConfig{{
			Name:         "claude-bedrock",
			Backend:      ts.URL,
			Model:        modelID,
			Timeout:      10,
			Type:         config.BackendBedrock,
			Region:       "us-east-1",
			AWSAccessKey: "AKIATEST",
			AWSSecretKey: "secret",
		}},
	}
	cs := config.NewTestConfigStore(cfg)
	return NewProxyHandler(cs, nil, nil), ts
}

func TestProxyBedrock_NonStreaming(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]any
	handler, ts := newBedrockProxyHandler(t, "anthropic.claude-3-sonnet", func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &gotBody)
		json.NewEncoder(w).Encode(map[string]any{
			"output": map[string]any{
				"message": map[string]any{
					"role":    "assistant",
					"content": []map[string]any{{"text": "Hi from Bedrock"}},
				},
			},
			"stopReason": "end_turn",
			"usage":      map[string]any{"inputTokens": 7, "outputTokens": 4, "totalTokens": 11},
		})
	})
	defer ts.Close()

	body := `{"model":"claude-bedrock","messages":[{"role":"user","content":"Hello"}],"max_tokens":50}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if gotPath != "/model/anthropic.claude-3-sonnet/converse" {
		t.Errorf("upstream path: %q", gotPath)
	}
	if !strings.HasPrefix(gotAuth, "AWS4-HMAC-SHA256 ") {
		t.Errorf("expected SigV4 auth, got %q", gotAuth)
	}
	if gotBody["messages"] == nil {
		t.Errorf("upstream body missing messages: %#v", gotBody)
	}

	var resp api.ChatResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response decode: %v", err)
	}
	if resp.Model != "claude-bedrock" {
		t.Errorf("model: %v", resp.Model)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("choices: %d", len(resp.Choices))
	}
	c := resp.Choices[0]
	if c.FinishReason != "stop" {
		t.Errorf("finish_reason: %v", c.FinishReason)
	}
	if c.Message.Content == nil || *c.Message.Content != "Hi from Bedrock" {
		t.Errorf("content: %v", c.Message.Content)
	}
	if resp.Usage.PromptTokens != 7 || resp.Usage.CompletionTokens != 4 {
		t.Errorf("usage: %+v", resp.Usage)
	}
}

func TestProxyBedrock_Streaming(t *testing.T) {
	var gotPath string
	handler, ts := newBedrockProxyHandler(t, "anthropic.claude-3-sonnet", func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		stream := buildBedrockStream(t, []struct{ Event, Payload string }{
			{"messageStart", `{"role":"assistant"}`},
			{"contentBlockDelta", `{"delta":{"text":"Hi"},"contentBlockIndex":0}`},
			{"contentBlockDelta", `{"delta":{"text":" there"},"contentBlockIndex":0}`},
			{"contentBlockStop", `{"contentBlockIndex":0}`},
			{"messageStop", `{"stopReason":"end_turn"}`},
			{"metadata", `{"usage":{"inputTokens":3,"outputTokens":2}}`},
		})
		w.Write(stream)
	})
	defer ts.Close()

	body := `{"model":"claude-bedrock","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	if gotPath != "/model/anthropic.claude-3-sonnet/converse-stream" {
		t.Errorf("upstream stream path: %q", gotPath)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("response Content-Type: %q", ct)
	}

	body_out := w.Body.String()

	// Split into chunks (excluding the [DONE] terminator).
	var deltas []map[string]any
	var finalUsage *api.ChunkUsage
	var sawFinish string
	for _, line := range strings.Split(body_out, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			continue
		}
		var chunk api.ChatChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			t.Errorf("bad chunk: %s — %v", data, err)
			continue
		}
		if len(chunk.Choices) > 0 {
			c := chunk.Choices[0]
			deltas = append(deltas, map[string]any{
				"role":    c.Delta.Role,
				"content": c.Delta.Content,
				"finish":  c.FinishReason,
			})
			if c.FinishReason != nil {
				sawFinish = *c.FinishReason
			}
		}
		if chunk.Usage != nil {
			finalUsage = chunk.Usage
		}
	}

	// First chunk should set role=assistant.
	if len(deltas) == 0 || deltas[0]["role"] != "assistant" {
		t.Errorf("first delta should be role=assistant, got %#v", deltas)
	}
	// Some chunk in the middle should contain text content.
	hasText := false
	for _, d := range deltas {
		if c, ok := d["content"].(*string); ok && c != nil && *c != "" {
			hasText = true
			break
		}
	}
	if !hasText {
		t.Errorf("no text content chunk in stream:\n%s", body_out)
	}
	if sawFinish != "stop" {
		t.Errorf("finish_reason: %q", sawFinish)
	}
	if finalUsage == nil || finalUsage.PromptTokens != 3 || finalUsage.CompletionTokens != 2 {
		t.Errorf("usage chunk: %+v", finalUsage)
	}
	if !strings.Contains(body_out, "data: [DONE]") {
		t.Errorf("missing [DONE] terminator")
	}
}

func TestProxyBedrock_StreamingToolCalls(t *testing.T) {
	handler, ts := newBedrockProxyHandler(t, "anthropic.claude-3-sonnet", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		stream := buildBedrockStream(t, []struct{ Event, Payload string }{
			{"messageStart", `{"role":"assistant"}`},
			{"contentBlockStart", `{"start":{"toolUse":{"toolUseId":"tu_1","name":"get_weather"}},"contentBlockIndex":0}`},
			{"contentBlockDelta", `{"delta":{"toolUse":{"input":"{\"city\":"}},"contentBlockIndex":0}`},
			{"contentBlockDelta", `{"delta":{"toolUse":{"input":"\"Paris\"}"}},"contentBlockIndex":0}`},
			{"contentBlockStop", `{"contentBlockIndex":0}`},
			{"messageStop", `{"stopReason":"tool_use"}`},
			{"metadata", `{"usage":{"inputTokens":5,"outputTokens":8}}`},
		})
		w.Write(stream)
	})
	defer ts.Close()

	body := `{"model":"claude-bedrock","stream":true,"messages":[{"role":"user","content":"weather?"}],"tools":[{"type":"function","function":{"name":"get_weather"}}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	body_out := w.Body.String()

	// Reconstruct tool call by finding name + arguments fragments.
	var name string
	var argFragments []string
	var sawFinish string
	for _, line := range strings.Split(body_out, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			continue
		}
		var chunk api.ChatChunk
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		for _, c := range chunk.Choices {
			for _, tc := range c.Delta.ToolCalls {
				if tc.Function != nil {
					if tc.Function.Name != "" {
						name = tc.Function.Name
					}
					if tc.Function.Arguments != "" {
						argFragments = append(argFragments, tc.Function.Arguments)
					}
				}
			}
			if c.FinishReason != nil {
				sawFinish = *c.FinishReason
			}
		}
	}

	if name != "get_weather" {
		t.Errorf("tool name: %q", name)
	}
	args := strings.Join(argFragments, "")
	if args != `{"city":"Paris"}` {
		t.Errorf("reconstructed args: %q", args)
	}
	if sawFinish != "tool_calls" {
		t.Errorf("finish_reason: %q", sawFinish)
	}
}

func TestProxyBedrock_BedrockAPIKeyAuth(t *testing.T) {
	var gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		json.NewEncoder(w).Encode(map[string]any{
			"output": map[string]any{
				"message": map[string]any{
					"role":    "assistant",
					"content": []map[string]any{{"text": "ok"}},
				},
			},
			"stopReason": "end_turn",
			"usage":      map[string]any{"inputTokens": 1, "outputTokens": 1},
		})
	}))
	defer ts.Close()

	cfg := &config.Config{
		Models: []config.ModelConfig{{
			Name: "claude-bedrock", Backend: ts.URL, Model: "x", Timeout: 10,
			Type: config.BackendBedrock, Region: "us-east-1",
			APIKey: "bdrk-key",
		}},
	}
	handler := NewProxyHandler(config.NewTestConfigStore(cfg), nil, nil)

	body := `{"model":"claude-bedrock","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if gotAuth != "Bearer bdrk-key" {
		t.Errorf("expected bearer auth, got %q", gotAuth)
	}
}

func TestProxyBedrock_RejectsNonChatCompletionsPath(t *testing.T) {
	cfg := &config.Config{
		Models: []config.ModelConfig{{
			Name: "claude-bedrock", Backend: "http://example", Model: "x", Timeout: 10,
			Type: config.BackendBedrock, Region: "us-east-1",
			AWSAccessKey: "k", AWSSecretKey: "s",
		}},
	}
	handler := NewProxyHandler(config.NewTestConfigStore(cfg), nil, nil)

	// Use /v1/embeddings (allowed by AllowedPaths but not chat/completions)
	// to verify the bedrock-specific path check fires before the generic
	// allowed-path check.
	body := `{"model":"claude-bedrock","input":"hi"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for non-chat path on bedrock, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "/v1/chat/completions") {
		t.Errorf("error message should hint at supported path, got %s", w.Body.String())
	}
}

func TestProxyBedrock_UpstreamErrorSanitized(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"message":"ValidationException: account 999999999 not allowed"}`))
	}))
	defer ts.Close()

	cfg := &config.Config{
		Models: []config.ModelConfig{{
			Name: "claude-bedrock", Backend: ts.URL, Model: "x", Timeout: 10,
			Type: config.BackendBedrock, Region: "us-east-1",
			AWSAccessKey: "k", AWSSecretKey: "s",
		}},
	}
	handler := NewProxyHandler(config.NewTestConfigStore(cfg), nil, nil)

	body := `{"model":"claude-bedrock","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "999999999") {
		t.Errorf("upstream error body leaked: %s", w.Body.String())
	}
}
