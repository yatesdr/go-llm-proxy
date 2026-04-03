package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// --- Translation unit tests ---

func TestTranslateAnthropicSystem_String(t *testing.T) {
	sys := json.RawMessage(`"You are helpful."`)
	if got := translateAnthropicSystem(sys); got != "You are helpful." {
		t.Fatalf("expected string passthrough, got %q", got)
	}
}

func TestTranslateAnthropicSystem_BlockArray(t *testing.T) {
	sys := json.RawMessage(`[{"type":"text","text":"Part 1","cache_control":{"type":"ephemeral"}},{"type":"text","text":"Part 2"}]`)
	got := translateAnthropicSystem(sys)
	if got != "Part 1\nPart 2" {
		t.Fatalf("expected concatenated text, got %q", got)
	}
}

func TestTranslateAnthropicSystem_Empty(t *testing.T) {
	if got := translateAnthropicSystem(nil); got != "" {
		t.Fatalf("expected empty for nil, got %q", got)
	}
	if got := translateAnthropicSystem(json.RawMessage(`null`)); got != "" {
		t.Fatalf("expected empty for null, got %q", got)
	}
}

func TestTranslateAnthropicMessages_SimpleExchange(t *testing.T) {
	msgs := []json.RawMessage{
		json.RawMessage(`{"role":"user","content":"Hello"}`),
		json.RawMessage(`{"role":"assistant","content":"Hi there!"}`),
	}
	result, err := translateAnthropicMessages(msgs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(result))
	}
	if result[0]["role"] != "user" || result[0]["content"] != "Hello" {
		t.Fatalf("unexpected user message: %v", result[0])
	}
	if result[1]["role"] != "assistant" || result[1]["content"] != "Hi there!" {
		t.Fatalf("unexpected assistant message: %v", result[1])
	}
}

func TestTranslateAnthropicMessages_ToolUseAndResult(t *testing.T) {
	msgs := []json.RawMessage{
		json.RawMessage(`{"role":"user","content":"What is the weather?"}`),
		json.RawMessage(`{"role":"assistant","content":[{"type":"text","text":"Let me check."},{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"location":"Paris"}}]}`),
		json.RawMessage(`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"22C and sunny"}]}`),
	}
	result, err := translateAnthropicMessages(msgs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 3 {
		t.Fatalf("expected 3 messages, got %d: %v", len(result), result)
	}

	// Assistant message with text + tool_calls.
	asst := result[1]
	if asst["role"] != "assistant" || asst["content"] != "Let me check." {
		t.Fatalf("unexpected assistant message: %v", asst)
	}
	tcs, ok := asst["tool_calls"].([]map[string]any)
	if !ok || len(tcs) != 1 {
		t.Fatalf("expected 1 tool_call, got %v", asst["tool_calls"])
	}
	if tcs[0]["id"] != "toolu_1" {
		t.Fatalf("expected tool call id toolu_1, got %v", tcs[0]["id"])
	}
	fn := tcs[0]["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Fatalf("expected get_weather, got %v", fn["name"])
	}
	// Arguments should be a JSON string.
	args := fn["arguments"].(string)
	if !strings.Contains(args, "Paris") {
		t.Fatalf("expected Paris in arguments, got %q", args)
	}

	// Tool result message.
	tool := result[2]
	if tool["role"] != "tool" || tool["tool_call_id"] != "toolu_1" {
		t.Fatalf("unexpected tool message: %v", tool)
	}
	if tool["content"] != "22C and sunny" {
		t.Fatalf("unexpected tool content: %v", tool["content"])
	}
}

func TestTranslateAnthropicMessages_ThinkingStripped(t *testing.T) {
	msgs := []json.RawMessage{
		json.RawMessage(`{"role":"assistant","content":[{"type":"thinking","thinking":"deep thought","signature":"abc"},{"type":"text","text":"Hello"}]}`),
	}
	result, err := translateAnthropicMessages(msgs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 message, got %d", len(result))
	}
	if result[0]["content"] != "Hello" {
		t.Fatalf("expected only text content after stripping thinking, got %v", result[0]["content"])
	}
	if result[0]["tool_calls"] != nil {
		t.Fatalf("expected no tool_calls, got %v", result[0]["tool_calls"])
	}
}

func TestTranslateAnthropicTools(t *testing.T) {
	tools := []json.RawMessage{
		json.RawMessage(`{"name":"get_weather","description":"Get weather","input_schema":{"type":"object","properties":{"location":{"type":"string"}},"required":["location"]},"cache_control":{"type":"ephemeral"}}`),
	}
	result := translateAnthropicToolsToChat(tools)
	if len(result) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(result))
	}
	if result[0]["type"] != "function" {
		t.Fatalf("expected function type, got %v", result[0]["type"])
	}
	fn := result[0]["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Fatalf("expected get_weather, got %v", fn["name"])
	}
	if fn["description"] != "Get weather" {
		t.Fatalf("expected description, got %v", fn["description"])
	}
}

func TestTranslateAnthropicTools_SkipsServerTools(t *testing.T) {
	tools := []json.RawMessage{
		json.RawMessage(`{"type":"web_search_20250305","name":"web_search"}`),
		json.RawMessage(`{"name":"get_weather","description":"Get weather","input_schema":{}}`),
	}
	result := translateAnthropicToolsToChat(tools)
	if len(result) != 1 {
		t.Fatalf("expected 1 tool (server tool stripped), got %d", len(result))
	}
}

func TestTranslateAnthropicToolChoice_AllMappings(t *testing.T) {
	cases := []struct {
		input    string
		expected string
	}{
		{`{"type":"auto"}`, `"auto"`},
		{`{"type":"any"}`, `"required"`},
		{`{"type":"none"}`, `"none"`},
		{`{"type":"tool","name":"fn1"}`, `{"type":"function","function":{"name":"fn1"}}`},
	}
	for _, c := range cases {
		got := translateAnthropicToolChoice(json.RawMessage(c.input), true)
		// Normalize whitespace for comparison.
		var gotParsed, expectedParsed any
		json.Unmarshal(got, &gotParsed)
		json.Unmarshal([]byte(c.expected), &expectedParsed)
		gotJSON, _ := json.Marshal(gotParsed)
		expJSON, _ := json.Marshal(expectedParsed)
		if string(gotJSON) != string(expJSON) {
			t.Errorf("input %s: expected %s, got %s", c.input, c.expected, string(got))
		}
	}
}

func TestTranslateAnthropicToolChoice_NoToolsStripped(t *testing.T) {
	got := translateAnthropicToolChoice(json.RawMessage(`{"type":"auto"}`), false)
	if got != nil {
		t.Fatalf("expected nil when no tools, got %s", got)
	}
}

func TestMapFinishToStopReason_AllMappings(t *testing.T) {
	cases := map[string]string{
		"stop":           "end_turn",
		"tool_calls":     "tool_use",
		"length":         "max_tokens",
		"content_filter": "end_turn",
		"":               "end_turn",
	}
	for input, expected := range cases {
		if got := mapFinishToStopReason(input); got != expected {
			t.Errorf("mapFinishToStopReason(%q) = %q, want %q", input, got, expected)
		}
	}
}

func TestBuildChatRequestFromAnthropic_FullRequest(t *testing.T) {
	req := messagesRequest{
		Model:     "test-model",
		System:    json.RawMessage(`"Be helpful."`),
		Messages:  []json.RawMessage{json.RawMessage(`{"role":"user","content":"Hello"}`)},
		MaxTokens: 1024,
		Stream:    true,
	}
	chatReq, err := buildChatRequestFromAnthropic(req, "backend-model")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if chatReq["model"] != "backend-model" {
		t.Fatalf("expected backend-model, got %v", chatReq["model"])
	}
	if chatReq["max_completion_tokens"] != 1024 {
		t.Fatalf("expected max_completion_tokens=1024, got %v", chatReq["max_completion_tokens"])
	}
	if chatReq["stream"] != true {
		t.Fatalf("expected stream=true")
	}
	msgs := chatReq["messages"].([]map[string]any)
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages (system + user), got %d", len(msgs))
	}
	if msgs[0]["role"] != "system" || msgs[0]["content"] != "Be helpful." {
		t.Fatalf("expected system message, got %v", msgs[0])
	}
}

// --- Integration tests ---

func newTestMessagesHandler(t *testing.T, modelType string, upstream http.HandlerFunc) (*MessagesHandler, *httptest.Server) {
	t.Helper()
	ts := httptest.NewServer(upstream)
	backend := ts.URL + "/v1"
	if modelType == BackendAnthropic {
		backend = ts.URL
	}
	cfg := &Config{
		Models: []ModelConfig{{
			Name: "test-model", Backend: backend, APIKey: "backend-secret",
			Model: "test-model", Timeout: 10, Type: modelType,
		}},
	}
	cs := &ConfigStore{config: cfg}
	return NewMessagesHandler(cs, nil), ts
}

func TestMessagesHandler_NonStreaming(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	handler, ts := newTestMessagesHandler(t, "", func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-1", "model": "test-model", "created": 0,
			"choices": []map[string]any{{
				"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": "Hello back!"},
			}},
			"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
		})
	})
	defer ts.Close()

	body := `{"model":"test-model","max_tokens":100,"messages":[{"role":"user","content":"Hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("expected /v1/chat/completions, got %q", gotPath)
	}

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["type"] != "message" {
		t.Fatalf("expected type=message, got %v", resp["type"])
	}
	if resp["stop_reason"] != "end_turn" {
		t.Fatalf("expected stop_reason=end_turn, got %v", resp["stop_reason"])
	}
	content := resp["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(content))
	}
	textBlock := content[0].(map[string]any)
	if textBlock["type"] != "text" || textBlock["text"] != "Hello back!" {
		t.Fatalf("unexpected content block: %v", textBlock)
	}
}

func TestMessagesHandler_NonStreaming_ToolCalls(t *testing.T) {
	handler, ts := newTestMessagesHandler(t, "", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-1", "model": "test-model", "created": 0,
			"choices": []map[string]any{{
				"index": 0, "finish_reason": "tool_calls",
				"message": map[string]any{
					"role": "assistant", "content": nil,
					"tool_calls": []map[string]any{{
						"id": "call_1", "type": "function",
						"function": map[string]any{"name": "get_weather", "arguments": `{"location":"Paris"}`},
					}},
				},
			}},
		})
	})
	defer ts.Close()

	body := `{"model":"test-model","max_tokens":1000,"messages":[{"role":"user","content":"Weather?"}],"tools":[{"name":"get_weather","input_schema":{}}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["stop_reason"] != "tool_use" {
		t.Fatalf("expected stop_reason=tool_use, got %v", resp["stop_reason"])
	}
	content := resp["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(content))
	}
	toolUse := content[0].(map[string]any)
	if toolUse["type"] != "tool_use" {
		t.Fatalf("expected tool_use block, got %v", toolUse["type"])
	}
	// input must be an object, not a string.
	input := toolUse["input"]
	if _, ok := input.(map[string]any); !ok {
		t.Fatalf("expected input as object, got %T: %v", input, input)
	}
}

func TestMessagesHandler_Streaming(t *testing.T) {
	handler, ts := newTestMessagesHandler(t, "", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		chunks := []string{
			`{"id":"chatcmpl-1","model":"test-model","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
			`{"id":"chatcmpl-1","model":"test-model","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}`,
			`{"id":"chatcmpl-1","model":"test-model","choices":[{"index":0,"delta":{"content":" world"},"finish_reason":null}]}`,
			`{"id":"chatcmpl-1","model":"test-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`{"id":"chatcmpl-1","model":"test-model","choices":[],"usage":{"prompt_tokens":8,"completion_tokens":3,"total_tokens":11}}`,
		}
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
			flusher.Flush()
		}
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
	})
	defer ts.Close()

	body := `{"model":"test-model","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("expected SSE content type, got %q", w.Header().Get("Content-Type"))
	}

	events := parseSSEEvents(w.Body.String())

	// Verify required event types.
	required := []string{
		"message_start", "content_block_start", "content_block_delta",
		"content_block_stop", "message_delta", "message_stop",
	}
	for _, et := range required {
		found := false
		for _, e := range events {
			if e.event == et {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing required event type: %s", et)
		}
	}

	// Verify every event has type field matching event name.
	for _, e := range events {
		var d map[string]any
		if json.Unmarshal([]byte(e.data), &d) != nil {
			continue
		}
		if d["type"] != e.event {
			t.Errorf("event %q: expected type=%q, got %v", e.event, e.event, d["type"])
		}
	}

	// Verify text deltas.
	var textDeltas []string
	for _, e := range events {
		if e.event == "content_block_delta" {
			var d map[string]any
			json.Unmarshal([]byte(e.data), &d)
			delta := d["delta"].(map[string]any)
			if delta["type"] == "text_delta" {
				textDeltas = append(textDeltas, delta["text"].(string))
			}
		}
	}
	if strings.Join(textDeltas, "") != "Hello world" {
		t.Fatalf("expected 'Hello world', got %q", strings.Join(textDeltas, ""))
	}

	// Verify message_delta has stop_reason.
	for _, e := range events {
		if e.event == "message_delta" {
			var d map[string]any
			json.Unmarshal([]byte(e.data), &d)
			delta := d["delta"].(map[string]any)
			if delta["stop_reason"] != "end_turn" {
				t.Fatalf("expected stop_reason=end_turn, got %v", delta["stop_reason"])
			}
		}
	}
}

func TestMessagesHandler_StreamingToolCalls(t *testing.T) {
	handler, ts := newTestMessagesHandler(t, "", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		chunks := []string{
			`{"id":"chatcmpl-1","model":"test-model","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
			`{"id":"chatcmpl-1","model":"test-model","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]},"finish_reason":null}]}`,
			`{"id":"chatcmpl-1","model":"test-model","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"loc"}}]},"finish_reason":null}]}`,
			`{"id":"chatcmpl-1","model":"test-model","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ation\":\"Paris\"}"}}]},"finish_reason":null}]}`,
			`{"id":"chatcmpl-1","model":"test-model","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		}
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
			flusher.Flush()
		}
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
	})
	defer ts.Close()

	body := `{"model":"test-model","max_tokens":1000,"stream":true,"messages":[{"role":"user","content":"Weather?"}],"tools":[{"name":"get_weather","input_schema":{}}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	events := parseSSEEvents(w.Body.String())

	// Should have tool_use content_block_start and input_json_delta.
	var hasToolStart, hasJsonDelta bool
	for _, e := range events {
		if e.event == "content_block_start" {
			var d map[string]any
			json.Unmarshal([]byte(e.data), &d)
			cb := d["content_block"].(map[string]any)
			if cb["type"] == "tool_use" {
				hasToolStart = true
			}
		}
		if e.event == "content_block_delta" {
			var d map[string]any
			json.Unmarshal([]byte(e.data), &d)
			delta := d["delta"].(map[string]any)
			if delta["type"] == "input_json_delta" {
				hasJsonDelta = true
			}
		}
	}
	if !hasToolStart {
		t.Error("missing tool_use content_block_start")
	}
	if !hasJsonDelta {
		t.Error("missing input_json_delta content_block_delta")
	}

	// message_delta should have stop_reason=tool_use.
	for _, e := range events {
		if e.event == "message_delta" {
			var d map[string]any
			json.Unmarshal([]byte(e.data), &d)
			delta := d["delta"].(map[string]any)
			if delta["stop_reason"] != "tool_use" {
				t.Fatalf("expected stop_reason=tool_use, got %v", delta["stop_reason"])
			}
		}
	}
}

// --- Native passthrough tests (moved from proxy_test.go) ---

func TestMessagesHandler_NativePassthrough_AuthHeaders(t *testing.T) {
	var gotAPIKey, gotAuth string
	handler, ts := newTestMessagesHandler(t, BackendAnthropic, func(w http.ResponseWriter, r *http.Request) {
		gotAPIKey = r.Header.Get("X-Api-Key")
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`)
	})
	defer ts.Close()

	body := `{"model":"test-model","max_tokens":100,"messages":[{"role":"user","content":"Hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotAPIKey != "backend-secret" {
		t.Fatalf("expected x-api-key=backend-secret, got %q", gotAPIKey)
	}
	if gotAuth != "" {
		t.Fatalf("expected no Authorization header for anthropic, got %q", gotAuth)
	}
}

func TestMessagesHandler_NativePassthrough_HeadersForwarded(t *testing.T) {
	var gotVersion, gotBeta string
	handler, ts := newTestMessagesHandler(t, BackendAnthropic, func(w http.ResponseWriter, r *http.Request) {
		gotVersion = r.Header.Get("Anthropic-Version")
		gotBeta = r.Header.Get("Anthropic-Beta")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[],"stop_reason":"end_turn"}`)
	})
	defer ts.Close()

	body := `{"model":"test-model","max_tokens":100,"messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("Anthropic-Beta", "prompt-caching-2024-07-31")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if gotVersion != "2023-06-01" {
		t.Fatalf("expected anthropic-version forwarded, got %q", gotVersion)
	}
	if gotBeta != "prompt-caching-2024-07-31" {
		t.Fatalf("expected anthropic-beta forwarded, got %q", gotBeta)
	}
}

func TestMessagesHandler_NativePassthrough_UpstreamPath(t *testing.T) {
	var gotPath string
	handler, ts := newTestMessagesHandler(t, BackendAnthropic, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[],"stop_reason":"end_turn"}`)
	})
	defer ts.Close()

	body := `{"model":"test-model","max_tokens":100,"messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if gotPath != "/v1/messages" {
		t.Fatalf("expected upstream path /v1/messages, got %q", gotPath)
	}
}

func TestMessagesHandler_TranslateModeSkipsProbe(t *testing.T) {
	var paths []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-1", "model": "test-model", "created": 0,
			"choices": []map[string]any{{
				"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": "OK"},
			}},
		})
	}))
	defer ts.Close()

	cfg := &Config{
		Models: []ModelConfig{{
			Name: "test-model", Backend: ts.URL + "/v1",
			Model: "test-model", Timeout: 10,
			MessagesMode: "translate",
		}},
	}
	cs := &ConfigStore{config: cfg}
	handler := NewMessagesHandler(cs, nil)

	body := `{"model":"test-model","max_tokens":100,"messages":[{"role":"user","content":"Hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if len(paths) != 1 || paths[0] != "/v1/chat/completions" {
		t.Fatalf("expected only /v1/chat/completions, got %v", paths)
	}
}
