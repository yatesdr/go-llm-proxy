package handler

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go-llm-proxy/internal/config"
)

// newBedrockTestHandler wires a MessagesHandler to a fake Bedrock-shaped
// backend. The fake backend records the inbound request and serves whatever
// upstream specifies.
func newBedrockTestHandler(t *testing.T, modelID string, upstream http.HandlerFunc) (*MessagesHandler, *httptest.Server) {
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
	return NewMessagesHandler(cs, nil, nil), ts
}

func TestBedrockHandler_NonStreaming(t *testing.T) {
	var gotPath string
	var gotAuth string
	var gotBody map[string]any
	handler, ts := newBedrockTestHandler(t, "anthropic.claude-3-sonnet", func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"output": map[string]any{
				"message": map[string]any{
					"role":    "assistant",
					"content": []map[string]any{{"text": "Hello back!"}},
				},
			},
			"stopReason": "end_turn",
			"usage":      map[string]any{"inputTokens": 10, "outputTokens": 5, "totalTokens": 15},
		})
	})
	defer ts.Close()

	body := `{"model":"claude-bedrock","max_tokens":100,"messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if gotPath != "/model/anthropic.claude-3-sonnet/converse" {
		t.Errorf("upstream path: %q", gotPath)
	}
	if !strings.HasPrefix(gotAuth, "AWS4-HMAC-SHA256 ") {
		t.Errorf("expected SigV4 Authorization header, got %q", gotAuth)
	}
	// Body must be Converse-shaped.
	if gotBody["messages"] == nil {
		t.Errorf("upstream missing messages: %#v", gotBody)
	}
	if _, ok := gotBody["inferenceConfig"].(map[string]any); !ok {
		t.Errorf("upstream missing inferenceConfig: %#v", gotBody)
	}

	// Response must be Anthropic-shaped.
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["type"] != "message" || resp["model"] != "claude-bedrock" {
		t.Errorf("response top-level: %v", resp)
	}
	if resp["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason: %v", resp["stop_reason"])
	}
	content := resp["content"].([]any)
	if len(content) != 1 || content[0].(map[string]any)["text"] != "Hello back!" {
		t.Errorf("content: %#v", content)
	}
	usage := resp["usage"].(map[string]any)
	if usage["input_tokens"].(float64) != 10 || usage["output_tokens"].(float64) != 5 {
		t.Errorf("usage: %v", usage)
	}
}

func TestBedrockHandler_StreamingPath(t *testing.T) {
	var gotPath string
	handler, ts := newBedrockTestHandler(t, "anthropic.claude-3-sonnet", func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		stream := buildBedrockStream(t, []struct{ Event, Payload string }{
			{"messageStart", `{"role":"assistant"}`},
			{"contentBlockDelta", `{"delta":{"text":"Hi"},"contentBlockIndex":0}`},
			{"contentBlockStop", `{"contentBlockIndex":0}`},
			{"messageStop", `{"stopReason":"end_turn"}`},
			{"metadata", `{"usage":{"inputTokens":1,"outputTokens":1}}`},
		})
		w.Write(stream)
	})
	defer ts.Close()

	body := `{"model":"claude-bedrock","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	if gotPath != "/model/anthropic.claude-3-sonnet/converse-stream" {
		t.Errorf("expected converse-stream path, got %q", gotPath)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("response Content-Type: %q", ct)
	}
	body_out := w.Body.String()
	for _, want := range []string{
		"event: message_start",
		"event: content_block_start",
		"event: content_block_delta",
		"event: message_delta",
		"event: message_stop",
	} {
		if !strings.Contains(body_out, want) {
			t.Errorf("missing %q in SSE output:\n%s", want, body_out)
		}
	}
}

func TestBedrockHandler_BedrockAPIKeyAuth(t *testing.T) {
	// When api_key is set, we must use bearer auth (no SigV4).
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
			Name:    "claude-bedrock",
			Backend: ts.URL,
			Model:   "anthropic.claude-3-sonnet",
			Timeout: 10,
			Type:    config.BackendBedrock,
			Region:  "us-east-1",
			APIKey:  "bdrk-secret-key",
		}},
	}
	handler := NewMessagesHandler(config.NewTestConfigStore(cfg), nil, nil)

	body := `{"model":"claude-bedrock","max_tokens":10,"messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if gotAuth != "Bearer bdrk-secret-key" {
		t.Errorf("expected bearer auth, got %q", gotAuth)
	}
}

func TestBedrockHandler_UpstreamError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"message":"ValidationException: account 999999999 is not allowed"}`))
	}))
	defer ts.Close()

	cfg := &config.Config{
		Models: []config.ModelConfig{{
			Name: "claude-bedrock", Backend: ts.URL, Model: "x", Timeout: 10,
			Type: config.BackendBedrock, Region: "us-east-1",
			AWSAccessKey: "k", AWSSecretKey: "s",
		}},
	}
	handler := NewMessagesHandler(config.NewTestConfigStore(cfg), nil, nil)

	body := `{"model":"claude-bedrock","max_tokens":10,"messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
	// Sanitized: must NOT leak the AWS account ID.
	if strings.Contains(w.Body.String(), "999999999") {
		t.Errorf("upstream error body leaked to client: %s", w.Body.String())
	}
}

func TestBuildBedrockURL(t *testing.T) {
	cases := []struct {
		backend, modelID string
		stream           bool
		want             string
	}{
		{"https://bedrock-runtime.us-east-1.amazonaws.com", "anthropic.claude-3-sonnet", false,
			"https://bedrock-runtime.us-east-1.amazonaws.com/model/anthropic.claude-3-sonnet/converse"},
		{"https://bedrock-runtime.us-east-1.amazonaws.com", "anthropic.claude-3-sonnet", true,
			"https://bedrock-runtime.us-east-1.amazonaws.com/model/anthropic.claude-3-sonnet/converse-stream"},
		// Inference profile IDs contain colons. Per RFC 3986 colons are
		// permitted unencoded inside a path segment, so url.PathEscape leaves
		// them alone — Bedrock accepts this. (The SigV4 canonical URI will
		// match because both sides use the same encoding.)
		{"https://bedrock-runtime.us-east-1.amazonaws.com", "us.anthropic.claude-sonnet-4-20250514-v1:0", false,
			"https://bedrock-runtime.us-east-1.amazonaws.com/model/us.anthropic.claude-sonnet-4-20250514-v1:0/converse"},
		// Trailing slash on backend is normalized.
		{"https://bedrock-runtime.us-east-1.amazonaws.com/", "x", false,
			"https://bedrock-runtime.us-east-1.amazonaws.com/model/x/converse"},
	}
	for _, tc := range cases {
		got, err := buildBedrockURL(&config.ModelConfig{
			Name: "m", Backend: tc.backend, Model: tc.modelID,
		}, tc.stream)
		if err != nil {
			t.Errorf("err for %v: %v", tc, err)
			continue
		}
		if got != tc.want {
			t.Errorf("buildBedrockURL(%q, stream=%v):\n  got:  %s\n  want: %s",
				tc.modelID, tc.stream, got, tc.want)
		}
	}
}
