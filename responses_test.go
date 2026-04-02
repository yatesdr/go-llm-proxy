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

// --- Input translation tests ---

func TestTranslateInput_StringInput(t *testing.T) {
	input := json.RawMessage(`"Hello, world!"`)
	msgs, err := translateInput(input, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0]["role"] != "user" || msgs[0]["content"] != "Hello, world!" {
		t.Fatalf("unexpected message: %v", msgs[0])
	}
}

func TestTranslateInput_StringInputWithInstructions(t *testing.T) {
	input := json.RawMessage(`"Hello"`)
	msgs, err := translateInput(input, "Be helpful.")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	if msgs[0]["role"] != "system" || msgs[0]["content"] != "Be helpful." {
		t.Fatalf("unexpected system message: %v", msgs[0])
	}
	if msgs[1]["role"] != "user" || msgs[1]["content"] != "Hello" {
		t.Fatalf("unexpected user message: %v", msgs[1])
	}
}

func TestTranslateInput_SimpleMessages(t *testing.T) {
	input := json.RawMessage(`[
		{"role": "user", "content": "Hi"},
		{"role": "assistant", "content": "Hello!"},
		{"role": "user", "content": "How are you?"}
	]`)
	msgs, err := translateInput(input, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(msgs))
	}
	if msgs[0]["role"] != "user" || msgs[0]["content"] != "Hi" {
		t.Fatalf("unexpected first message: %v", msgs[0])
	}
	if msgs[1]["role"] != "assistant" || msgs[1]["content"] != "Hello!" {
		t.Fatalf("unexpected second message: %v", msgs[1])
	}
}

func TestTranslateInput_TypedItems(t *testing.T) {
	input := json.RawMessage(`[
		{"role": "user", "content": "What's the weather?"},
		{"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "Let me check."}]},
		{"type": "function_call", "call_id": "call_1", "name": "get_weather", "arguments": "{\"location\":\"Paris\"}"},
		{"type": "function_call_output", "call_id": "call_1", "output": "Sunny, 22C"},
		{"role": "user", "content": "Thanks!"}
	]`)
	msgs, err := translateInput(input, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(msgs) != 4 {
		t.Fatalf("expected 4 messages, got %d: %v", len(msgs), msgs)
	}

	// First: user message.
	if msgs[0]["role"] != "user" {
		t.Fatalf("expected user role, got %v", msgs[0]["role"])
	}

	// Second: assistant message with merged tool_calls.
	if msgs[1]["role"] != "assistant" {
		t.Fatalf("expected assistant role, got %v", msgs[1]["role"])
	}
	if msgs[1]["content"] != "Let me check." {
		t.Fatalf("expected assistant content, got %v", msgs[1]["content"])
	}
	tcs, ok := msgs[1]["tool_calls"].([]any)
	if !ok || len(tcs) != 1 {
		t.Fatalf("expected 1 tool_call, got %v", msgs[1]["tool_calls"])
	}

	// Third: tool message.
	if msgs[2]["role"] != "tool" || msgs[2]["tool_call_id"] != "call_1" {
		t.Fatalf("expected tool message, got %v", msgs[2])
	}

	// Fourth: user message.
	if msgs[3]["role"] != "user" || msgs[3]["content"] != "Thanks!" {
		t.Fatalf("expected final user message, got %v", msgs[3])
	}
}

func TestTranslateInput_FunctionCallWithoutMessage(t *testing.T) {
	input := json.RawMessage(`[
		{"role": "user", "content": "Do it"},
		{"type": "function_call", "call_id": "call_1", "name": "run_cmd", "arguments": "{}"},
		{"type": "function_call_output", "call_id": "call_1", "output": "done"}
	]`)
	msgs, err := translateInput(input, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(msgs))
	}
	// Function call creates an empty assistant message.
	if msgs[1]["role"] != "assistant" {
		t.Fatalf("expected assistant message for tool_calls, got %v", msgs[1])
	}
	if msgs[2]["role"] != "tool" {
		t.Fatalf("expected tool message, got %v", msgs[2])
	}
}

func TestTranslateInput_DeveloperRole(t *testing.T) {
	input := json.RawMessage(`[{"role": "developer", "content": "Be concise"}]`)
	msgs, err := translateInput(input, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if msgs[0]["role"] != "system" {
		t.Fatalf("expected developer to map to system, got %v", msgs[0]["role"])
	}
}

func TestTranslateInput_SkipsReasoningItems(t *testing.T) {
	input := json.RawMessage(`[
		{"role": "user", "content": "Hello"},
		{"type": "reasoning", "id": "rs_1", "summary": [{"type": "summary_text", "text": "thinking..."}]},
		{"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "Hi"}]}
	]`)
	msgs, err := translateInput(input, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages (reasoning skipped), got %d", len(msgs))
	}
}

func TestTranslateInput_InvalidInput(t *testing.T) {
	_, err := translateInput(json.RawMessage(`123`), "")
	if err == nil {
		t.Fatal("expected error for numeric input")
	}
}

func TestTranslateInput_EmptyArray(t *testing.T) {
	_, err := translateInput(json.RawMessage(`[]`), "")
	if err == nil {
		t.Fatal("expected error for empty array")
	}
}

// --- Tool translation tests ---

func TestTranslateTools_FunctionTools(t *testing.T) {
	tools := []json.RawMessage{
		json.RawMessage(`{"type":"function","name":"get_weather","description":"Get weather","parameters":{"type":"object","properties":{}},"strict":true}`),
	}
	result := translateTools(tools)
	if len(result) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(result))
	}
	if result[0]["type"] != "function" {
		t.Fatalf("expected function type, got %v", result[0]["type"])
	}
	fn, ok := result[0]["function"].(map[string]any)
	if !ok {
		t.Fatalf("expected function map, got %T", result[0]["function"])
	}
	if fn["name"] != "get_weather" {
		t.Fatalf("expected get_weather, got %v", fn["name"])
	}
}

func TestTranslateTools_SkipsNonFunction(t *testing.T) {
	tools := []json.RawMessage{
		json.RawMessage(`{"type":"web_search_preview"}`),
		json.RawMessage(`{"type":"function","name":"fn1"}`),
	}
	result := translateTools(tools)
	if len(result) != 1 {
		t.Fatalf("expected 1 tool (non-function skipped), got %d", len(result))
	}
}

// --- Text format translation tests ---

func TestTranslateTextFormat_JSONSchema(t *testing.T) {
	text := json.RawMessage(`{"format":{"type":"json_schema","name":"output","schema":{"type":"object"},"strict":true}}`)
	result := translateTextFormat(text)
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	var rf map[string]any
	json.Unmarshal(result, &rf)
	if rf["type"] != "json_schema" {
		t.Fatalf("expected json_schema type, got %v", rf["type"])
	}
	js, ok := rf["json_schema"].(map[string]any)
	if !ok || js["name"] != "output" {
		t.Fatalf("unexpected json_schema structure: %v", rf)
	}
}

func TestTranslateTextFormat_JSONObject(t *testing.T) {
	text := json.RawMessage(`{"format":{"type":"json_object"}}`)
	result := translateTextFormat(text)
	var rf map[string]any
	json.Unmarshal(result, &rf)
	if rf["type"] != "json_object" {
		t.Fatalf("expected json_object, got %v", rf["type"])
	}
}

func TestTranslateTextFormat_Text(t *testing.T) {
	text := json.RawMessage(`{"format":{"type":"text"}}`)
	result := translateTextFormat(text)
	if result != nil {
		t.Fatalf("expected nil for text format, got %s", result)
	}
}

// --- Integration tests ---

// newTestResponsesHandler creates a ResponsesHandler backed by a fake upstream
// that speaks Chat Completions. ResponsesMode is set to "translate" so the
// handler skips the native probe and goes straight to translation.
func newTestResponsesHandler(t *testing.T, upstream http.HandlerFunc) (*ResponsesHandler, *httptest.Server) {
	t.Helper()
	ts := httptest.NewServer(upstream)
	cfg := &Config{
		Models: []ModelConfig{
			{
				Name:                 "test-model",
				Backend:              ts.URL + "/v1",
				APIKey:               "backend-secret",
				Model:                "test-model",
				Timeout:              10,
				Type:                 "",
				ResponsesMode: "translate",
			},
		},
	}
	cs := &ConfigStore{config: cfg}
	return NewResponsesHandler(cs, nil), ts
}

func TestResponsesHandler_NonStreaming(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]any

	handler, ts := newTestResponsesHandler(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &gotBody)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":      "chatcmpl-123",
			"model":   "test-model",
			"created": 1234567890,
			"choices": []map[string]any{{
				"index":         0,
				"finish_reason": "stop",
				"message": map[string]any{
					"role":    "assistant",
					"content": "Hello back!",
				},
			}},
			"usage": map[string]any{
				"prompt_tokens":     10,
				"completion_tokens": 5,
				"total_tokens":      15,
			},
		})
	})
	defer ts.Close()

	body := `{"model":"test-model","input":"Hello!","stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("expected upstream path /v1/chat/completions, got %q", gotPath)
	}
	if gotAuth != "Bearer backend-secret" {
		t.Fatalf("expected Bearer auth, got %q", gotAuth)
	}

	// Check the upstream request was properly translated.
	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	msg := msgs[0].(map[string]any)
	if msg["role"] != "user" || msg["content"] != "Hello!" {
		t.Fatalf("unexpected message: %v", msg)
	}

	// Check response is valid Responses API format.
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["object"] != "response" {
		t.Fatalf("expected object=response, got %v", resp["object"])
	}
	if resp["status"] != "completed" {
		t.Fatalf("expected status=completed, got %v", resp["status"])
	}
	if resp["output_text"] != "Hello back!" {
		t.Fatalf("expected output_text, got %v", resp["output_text"])
	}
	usageMap, _ := resp["usage"].(map[string]any)
	if usageMap == nil || usageMap["total_tokens"] != float64(15) {
		t.Fatalf("unexpected usage: %v", resp["usage"])
	}
}

func TestResponsesHandler_Streaming(t *testing.T) {
	handler, ts := newTestResponsesHandler(t, func(w http.ResponseWriter, r *http.Request) {
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

	body := `{"model":"test-model","input":"Hi","stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("expected SSE content type, got %q", w.Header().Get("Content-Type"))
	}

	// Parse SSE events from the response.
	events := parseSSEEvents(w.Body.String())

	// Verify expected event types are present.
	expectedTypes := []string{
		"response.created",
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.completed",
	}
	for _, et := range expectedTypes {
		found := false
		for _, e := range events {
			if e.event == et {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing expected event type: %s", et)
		}
	}

	// Verify every SSE event payload includes a "type" field matching the event name.
	for _, e := range events {
		var d map[string]any
		if json.Unmarshal([]byte(e.data), &d) != nil {
			continue
		}
		if d["type"] != e.event {
			t.Errorf("event %q: expected type=%q in payload, got %v", e.event, e.event, d["type"])
		}
	}

	// Check text deltas accumulated correctly.
	var textDeltas []string
	for _, e := range events {
		if e.event == "response.output_text.delta" {
			var d map[string]any
			json.Unmarshal([]byte(e.data), &d)
			textDeltas = append(textDeltas, d["delta"].(string))
		}
	}
	fullText := strings.Join(textDeltas, "")
	if fullText != "Hello world" {
		t.Fatalf("expected 'Hello world', got %q", fullText)
	}

	// Check the completed event has usage.
	for _, e := range events {
		if e.event == "response.completed" {
			var d map[string]any
			json.Unmarshal([]byte(e.data), &d)
			resp := d["response"].(map[string]any)
			if resp["status"] != "completed" {
				t.Fatalf("expected completed status, got %v", resp["status"])
			}
			u := resp["usage"].(map[string]any)
			if u["total_tokens"] != float64(11) {
				t.Fatalf("expected total_tokens=11, got %v", u["total_tokens"])
			}
		}
	}
}

func TestResponsesHandler_StreamingToolCalls(t *testing.T) {
	handler, ts := newTestResponsesHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)

		chunks := []string{
			`{"id":"chatcmpl-1","model":"test-model","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
			`{"id":"chatcmpl-1","model":"test-model","choices":[{"index":0,"delta":{"content":"Let me check."},"finish_reason":null}]}`,
			`{"id":"chatcmpl-1","model":"test-model","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_abc","type":"function","function":{"name":"get_weather","arguments":""}}]},"finish_reason":null}]}`,
			`{"id":"chatcmpl-1","model":"test-model","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"loc"}}]},"finish_reason":null}]}`,
			`{"id":"chatcmpl-1","model":"test-model","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ation\":\"Paris\"}"}}]},"finish_reason":null}]}`,
			`{"id":"chatcmpl-1","model":"test-model","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			`{"id":"chatcmpl-1","model":"test-model","choices":[],"usage":{"prompt_tokens":20,"completion_tokens":10,"total_tokens":30}}`,
		}
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
			flusher.Flush()
		}
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
	})
	defer ts.Close()

	body := `{"model":"test-model","input":"What's the weather?","stream":true,"tools":[{"type":"function","name":"get_weather","parameters":{}}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	events := parseSSEEvents(w.Body.String())

	// Should have message item done (with text) and function call item done.
	var msgDone, fcDone bool
	var fcArgs string
	for _, e := range events {
		if e.event == "response.output_item.done" {
			var d map[string]any
			json.Unmarshal([]byte(e.data), &d)
			item := d["item"].(map[string]any)
			switch item["type"] {
			case "message":
				msgDone = true
			case "function_call":
				fcDone = true
				fcArgs = item["arguments"].(string)
			}
		}
	}
	if !msgDone {
		t.Error("missing message output_item.done event")
	}
	if !fcDone {
		t.Error("missing function_call output_item.done event")
	}
	if fcArgs != `{"location":"Paris"}` {
		t.Fatalf("expected function args, got %q", fcArgs)
	}
}

func TestResponsesHandler_RejectsAnthropicBackend(t *testing.T) {
	cfg := &Config{
		Models: []ModelConfig{
			{
				Name:    "claude-test",
				Backend: "http://localhost:9999",
				Model:   "claude-test",
				Timeout: 10,
				Type:    BackendAnthropic,
			},
		},
	}
	cs := &ConfigStore{config: cfg}
	handler := NewResponsesHandler(cs, nil)

	body := `{"model":"claude-test","input":"Hello","stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for anthropic backend, got %d", w.Code)
	}
}

func TestResponsesHandler_UnknownModel(t *testing.T) {
	cfg := &Config{
		Models: []ModelConfig{
			{Name: "model-a", Backend: "http://localhost:9999/v1", Model: "model-a", Timeout: 10},
		},
	}
	cs := &ConfigStore{config: cfg}
	handler := NewResponsesHandler(cs, nil)

	body := `{"model":"nonexistent","input":"Hello","stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown model, got %d", w.Code)
	}
}

func TestResponsesHandler_MissingModel(t *testing.T) {
	cfg := &Config{}
	cs := &ConfigStore{config: cfg}
	handler := NewResponsesHandler(cs, nil)

	body := `{"input":"Hello","stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing model, got %d", w.Code)
	}
}

func TestResponsesHandler_MethodNotAllowed(t *testing.T) {
	cfg := &Config{}
	cs := &ConfigStore{config: cfg}
	handler := NewResponsesHandler(cs, nil)

	req := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", w.Code)
	}
}

func TestResponsesHandler_InstructionsAsSystem(t *testing.T) {
	var gotBody map[string]any
	handler, ts := newTestResponsesHandler(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-1", "model": "test-model", "created": 0,
			"choices": []map[string]any{{"index": 0, "finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": "OK"}}},
		})
	})
	defer ts.Close()

	body := `{"model":"test-model","input":"Hello","instructions":"Be brief.","stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages (system + user), got %d", len(msgs))
	}
	first := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "Be brief." {
		t.Fatalf("expected system message with instructions, got %v", first)
	}
}

func TestResponsesHandler_ReasoningEffort(t *testing.T) {
	var gotBody map[string]any
	handler, ts := newTestResponsesHandler(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-1", "model": "test-model", "created": 0,
			"choices": []map[string]any{{"index": 0, "finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": "OK"}}},
		})
	})
	defer ts.Close()

	body := `{"model":"test-model","input":"Hello","reasoning":{"effort":"high"},"stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if gotBody["reasoning_effort"] != "high" {
		t.Fatalf("expected reasoning_effort=high, got %v", gotBody["reasoning_effort"])
	}
}

func TestResponsesHandler_MaxOutputTokens(t *testing.T) {
	var gotBody map[string]any
	handler, ts := newTestResponsesHandler(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-1", "model": "test-model", "created": 0,
			"choices": []map[string]any{{"index": 0, "finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": "OK"}}},
		})
	})
	defer ts.Close()

	body := `{"model":"test-model","input":"Hello","max_output_tokens":1000,"stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if gotBody["max_completion_tokens"] != float64(1000) {
		t.Fatalf("expected max_completion_tokens=1000, got %v", gotBody["max_completion_tokens"])
	}
}

// --- Native passthrough / auto-detect tests ---

func TestResponsesHandler_NativePassthrough(t *testing.T) {
	var gotPath string
	// Backend that supports /v1/responses natively.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "resp_native", "object": "response", "status": "completed",
			"output": []any{map[string]any{
				"type": "message", "role": "assistant", "status": "completed",
				"content": []any{map[string]any{"type": "output_text", "text": "Native response"}},
			}},
		})
	}))
	defer ts.Close()

	cfg := &Config{
		Models: []ModelConfig{{
			Name: "test-model", Backend: ts.URL + "/v1",
			Model: "test-model", Timeout: 10,
		}},
	}
	cs := &ConfigStore{config: cfg}
	handler := NewResponsesHandler(cs, nil)

	body := `{"model":"test-model","input":"Hello","stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotPath != "/v1/responses" {
		t.Fatalf("expected native path /v1/responses, got %q", gotPath)
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["id"] != "resp_native" {
		t.Fatalf("expected native response ID passed through, got %v", resp["id"])
	}
}

func TestResponsesHandler_FallbackOn404(t *testing.T) {
	var callCount int
	// Backend returns 404 for /responses, 200 for /chat/completions.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if r.URL.Path == "/v1/responses" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-1", "model": "test-model", "created": 0,
			"choices": []map[string]any{{
				"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": "Translated response"},
			}},
			"usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8},
		})
	}))
	defer ts.Close()

	cfg := &Config{
		Models: []ModelConfig{{
			Name: "test-model", Backend: ts.URL + "/v1",
			Model: "test-model", Timeout: 10,
		}},
	}
	cs := &ConfigStore{config: cfg}
	handler := NewResponsesHandler(cs, nil)

	// First request: probes native (404) → falls back to translation.
	body := `{"model":"test-model","input":"Hello","stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["object"] != "response" {
		t.Fatalf("expected translated response, got %v", resp["object"])
	}
	if resp["output_text"] != "Translated response" {
		t.Fatalf("expected translated content, got %v", resp["output_text"])
	}
	firstCallCount := callCount // Should be 2 (probe + translation).

	// Second request: cached — goes straight to translation, no probe.
	req2 := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req2)

	if w2.Code != http.StatusOK {
		t.Fatalf("second request: expected 200, got %d", w2.Code)
	}
	// Should only have made 1 additional call (translation only, no probe).
	if callCount != firstCallCount+1 {
		t.Fatalf("expected cache to skip probe: call count went from %d to %d (expected +1)", firstCallCount, callCount)
	}
}

func TestResponsesHandler_TranslateModeSkipsProbe(t *testing.T) {
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
			ResponsesMode: "translate",
		}},
	}
	cs := &ConfigStore{config: cfg}
	handler := NewResponsesHandler(cs, nil)

	body := `{"model":"test-model","input":"Hello","stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
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

func TestResponsesHandler_NativeModeNoFallback(t *testing.T) {
	// Backend returns 404 for /responses — native mode should NOT fall back.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	cfg := &Config{
		Models: []ModelConfig{{
			Name: "test-model", Backend: ts.URL + "/v1",
			Model: "test-model", Timeout: 10,
			ResponsesMode: "native",
		}},
	}
	cs := &ConfigStore{config: cfg}
	handler := NewResponsesHandler(cs, nil)

	body := `{"model":"test-model","input":"Hello","stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 for native mode with unsupported backend, got %d", w.Code)
	}
}

func TestHandleCompact_NativePassthrough(t *testing.T) {
	var gotPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "resp_compact", "object": "response.compaction",
			"output": []any{map[string]any{"type": "compaction", "encrypted_content": "opaque"}},
		})
	}))
	defer ts.Close()

	cfg := &Config{
		Models: []ModelConfig{{
			Name: "test-model", Backend: ts.URL + "/v1",
			Model: "test-model", Timeout: 10,
		}},
	}
	cs := &ConfigStore{config: cfg}
	handler := NewResponsesHandler(cs, nil)

	body := `{"model":"test-model","input":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.HandleCompact(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotPath != "/v1/responses/compact" {
		t.Fatalf("expected native compact path, got %q", gotPath)
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["object"] != "response.compaction" {
		t.Fatalf("expected native compact response passed through, got %v", resp["object"])
	}
}

// --- Compact handler tests ---

func TestHandleCompact_Summarizes(t *testing.T) {
	var gotBody map[string]any
	handler, ts := newTestResponsesHandler(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-1", "model": "test-model", "created": 0,
			"choices": []map[string]any{{
				"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": "Summary: user asked to fix a bug in main.go."},
			}},
			"usage": map[string]any{"prompt_tokens": 100, "completion_tokens": 20, "total_tokens": 120},
		})
	})
	defer ts.Close()

	body := `{
		"model": "test-model",
		"input": [
			{"role": "user", "content": "Fix the bug in main.go"},
			{"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "I found the issue."}]},
			{"type": "function_call", "call_id": "call_1", "name": "edit_file", "arguments": "{}"},
			{"type": "function_call_output", "call_id": "call_1", "output": "ok"},
			{"role": "user", "content": "Thanks, now add tests"}
		]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.HandleCompact(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)

	if resp["object"] != "response.compaction" {
		t.Fatalf("expected object=response.compaction, got %v", resp["object"])
	}

	output, ok := resp["output"].([]any)
	if !ok {
		t.Fatalf("expected output array, got %T", resp["output"])
	}

	// Should have 2 user messages preserved + 1 summary assistant message.
	if len(output) != 3 {
		t.Fatalf("expected 3 output items (2 user + 1 summary), got %d", len(output))
	}

	// Check user messages are preserved.
	first := output[0].(map[string]any)
	if first["role"] != "user" {
		t.Fatalf("expected first output to be user message, got %v", first["role"])
	}
	second := output[1].(map[string]any)
	if second["role"] != "user" {
		t.Fatalf("expected second output to be user message, got %v", second["role"])
	}

	// Check summary message.
	summary := output[2].(map[string]any)
	if summary["role"] != "assistant" || summary["type"] != "message" {
		t.Fatalf("expected assistant message, got %v", summary)
	}

	// Check the upstream request included the summarization system prompt.
	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) < 2 {
		t.Fatalf("expected multiple messages in upstream request, got %d", len(msgs))
	}
	firstMsg := msgs[0].(map[string]any)
	if firstMsg["role"] != "system" || !strings.Contains(firstMsg["content"].(string), "Summarize") {
		t.Fatalf("expected summarization system prompt, got %v", firstMsg)
	}

	// Check usage is present.
	usage, _ := resp["usage"].(map[string]any)
	if usage == nil || usage["total_tokens"] != float64(120) {
		t.Fatalf("expected usage with total_tokens=120, got %v", resp["usage"])
	}
}

func TestHandleCompact_RejectsAnthropicBackend(t *testing.T) {
	cfg := &Config{
		Models: []ModelConfig{{
			Name: "claude-test", Backend: "http://localhost:9999",
			Model: "claude-test", Timeout: 10, Type: BackendAnthropic,
		}},
	}
	cs := &ConfigStore{config: cfg}
	handler := NewResponsesHandler(cs, nil)

	body := `{"model":"claude-test","input":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.HandleCompact(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for anthropic backend, got %d", w.Code)
	}
}

// --- SSE parsing helpers ---

type sseEvent struct {
	event string
	data  string
}

func parseSSEEvents(body string) []sseEvent {
	var events []sseEvent
	var currentEvent string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "event: ") {
			currentEvent = strings.TrimSpace(line[7:])
		} else if strings.HasPrefix(line, "data: ") {
			events = append(events, sseEvent{
				event: currentEvent,
				data:  line[6:],
			})
		}
	}
	return events
}
