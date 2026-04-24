package handler

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go-llm-proxy/internal/config"
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

	if msgs[0]["role"] != "user" {
		t.Fatalf("expected user role, got %v", msgs[0]["role"])
	}
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
	if msgs[2]["role"] != "tool" || msgs[2]["tool_call_id"] != "call_1" {
		t.Fatalf("expected tool message, got %v", msgs[2])
	}
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
	result, _ := translateTools(tools)
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
	result, stripped := translateTools(tools)
	if len(result) != 1 {
		t.Fatalf("expected 1 tool (non-function skipped), got %d", len(result))
	}
	if len(stripped) != 1 || stripped[0] != "web_search_preview" {
		t.Fatalf("expected stripped web_search_preview, got %v", stripped)
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

func TestTranslateInput_ViewImageOutput(t *testing.T) {
	// Codex view_image returns function_call_output with image content items (not a string).
	input := json.RawMessage(`[
		{"role": "user", "content": "What's in this image?"},
		{"type": "function_call", "call_id": "call_vi1", "name": "view_image", "arguments": "{\"path\":\"/tmp/test.png\"}"},
		{"type": "function_call_output", "call_id": "call_vi1", "output": [{"type": "input_image", "image_url": "data:image/png;base64,iVBOR"}]}
	]`)
	msgs, err := translateInput(input, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(msgs))
	}
	// The tool output should be translated to Chat Completions image_url content.
	toolMsg := msgs[2]
	if toolMsg["role"] != "tool" {
		t.Fatalf("expected tool role, got %v", toolMsg["role"])
	}
	content, ok := toolMsg["content"].([]map[string]any)
	if !ok {
		t.Fatalf("expected content to be []map[string]any for image output, got %T: %v", toolMsg["content"], toolMsg["content"])
	}
	if len(content) != 1 {
		t.Fatalf("expected 1 content part, got %d", len(content))
	}
	if content[0]["type"] != "image_url" {
		t.Fatalf("expected image_url type, got %v", content[0]["type"])
	}
	imgURL, ok := content[0]["image_url"].(map[string]any)
	if !ok {
		t.Fatalf("expected image_url map, got %T", content[0]["image_url"])
	}
	if imgURL["url"] != "data:image/png;base64,iVBOR" {
		t.Fatalf("expected base64 image URL, got %v", imgURL["url"])
	}
}

func TestTranslateInput_PDFDataURLInInputImage(t *testing.T) {
	// Responses API clients sometimes send PDFs as input_image with a
	// data:application/pdf data URL. The translator should detect this
	// and emit a pdf_data part so the PDF pipeline (not the image pipeline)
	// handles it.
	input := json.RawMessage(`[
		{"role": "user", "content": [
			{"type": "input_text", "text": "Summarize this PDF"},
			{"type": "input_image", "image_url": "data:application/pdf;base64,JVBERi0xLjQK"}
		]}
	]`)
	msgs, err := translateInput(input, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	content, ok := msgs[0]["content"].([]map[string]any)
	if !ok {
		t.Fatalf("expected content to be []map[string]any, got %T", msgs[0]["content"])
	}
	if len(content) != 2 {
		t.Fatalf("expected 2 parts (text + pdf_data), got %d", len(content))
	}
	if content[0]["type"] != "text" {
		t.Fatalf("expected text part first, got %v", content[0])
	}
	if content[1]["type"] != "pdf_data" {
		t.Fatalf("expected pdf_data part (not image_url), got %v", content[1]["type"])
	}
	if content[1]["data"] != "JVBERi0xLjQK" {
		t.Fatalf("expected PDF base64 payload preserved, got %v", content[1]["data"])
	}
}

func TestTranslateInput_StructuredOutputWithSuccess(t *testing.T) {
	// Codex view_image may also send output as {"content":[...],"success":true}.
	input := json.RawMessage(`[
		{"role": "user", "content": "View this"},
		{"type": "function_call", "call_id": "call_vi2", "name": "view_image", "arguments": "{}"},
		{"type": "function_call_output", "call_id": "call_vi2", "output": {"content": [{"type": "input_image", "image_url": "data:image/jpeg;base64,/9j"}], "success": true}}
	]`)
	msgs, err := translateInput(input, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	toolMsg := msgs[2]
	content, ok := toolMsg["content"].([]map[string]any)
	if !ok {
		t.Fatalf("expected content to be []map[string]any, got %T: %v", toolMsg["content"], toolMsg["content"])
	}
	if len(content) != 1 || content[0]["type"] != "image_url" {
		t.Fatalf("expected image_url content, got %v", content)
	}
}

func TestTranslateInput_StringOutputStillWorks(t *testing.T) {
	// Regular string output should still work as before.
	input := json.RawMessage(`[
		{"role": "user", "content": "Run ls"},
		{"type": "function_call", "call_id": "call_ls", "name": "shell", "arguments": "{}"},
		{"type": "function_call_output", "call_id": "call_ls", "output": "file1.txt\nfile2.txt"}
	]`)
	msgs, err := translateInput(input, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	toolMsg := msgs[2]
	content, ok := toolMsg["content"].(string)
	if !ok {
		t.Fatalf("expected string content for text output, got %T: %v", toolMsg["content"], toolMsg["content"])
	}
	if content != "file1.txt\nfile2.txt" {
		t.Fatalf("expected ls output, got %v", content)
	}
}

// --- Integration tests ---

// newTestResponsesHandler creates a ResponsesHandler backed by a fake upstream
// that speaks Chat Completions. ResponsesMode is set to "translate" so the
// handler skips the native probe and goes straight to translation.
func newTestResponsesHandler(t *testing.T, upstream http.HandlerFunc) (*ResponsesHandler, *httptest.Server) {
	t.Helper()
	ts := httptest.NewServer(upstream)
	cfg := &config.Config{
		Models: []config.ModelConfig{
			{
				Name:          "test-model",
				Backend:       ts.URL + "/v1",
				APIKey:        "backend-secret",
				Model:         "test-model",
				Timeout:       10,
				Type:          "",
				ResponsesMode: "translate",
			},
		},
	}
	cs := config.NewTestConfigStore(cfg)
	return NewResponsesHandler(cs, nil, nil), ts
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

	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	msg := msgs[0].(map[string]any)
	if msg["role"] != "user" || msg["content"] != "Hello!" {
		t.Fatalf("unexpected message: %v", msg)
	}

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

func TestResponsesHandler_CustomRawAuthHeader(t *testing.T) {
	var gotAuth, gotCustom string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCustom = r.Header.Get("X-Litellm-Api-Key")
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
		})
	}))
	defer ts.Close()

	cfg := &config.Config{
		Models: []config.ModelConfig{{
			Name:           "test-model",
			Backend:        ts.URL + "/v1",
			APIKey:         "backend-secret",
			AuthHeaderName: "X-Litellm-Api-Key",
			AuthScheme:     config.AuthSchemeRaw,
			Model:          "test-model",
			Timeout:        10,
			ResponsesMode:  "translate",
		}},
	}
	handler := NewResponsesHandler(config.NewTestConfigStore(cfg), nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"test-model","input":"Hello!","stream":false}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotAuth != "" {
		t.Fatalf("expected no Authorization header, got %q", gotAuth)
	}
	if gotCustom != "backend-secret" {
		t.Fatalf("expected raw custom auth header, got %q", gotCustom)
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

	events := parseSSEEvents(w.Body.String())

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

	for _, e := range events {
		var d map[string]any
		if json.Unmarshal([]byte(e.data), &d) != nil {
			continue
		}
		if d["type"] != e.event {
			t.Errorf("event %q: expected type=%q in payload, got %v", e.event, e.event, d["type"])
		}
	}

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
	cfg := &config.Config{
		Models: []config.ModelConfig{
			{
				Name:    "claude-test",
				Backend: "http://localhost:9999",
				Model:   "claude-test",
				Timeout: 10,
				Type:    config.BackendAnthropic,
			},
		},
	}
	cs := config.NewTestConfigStore(cfg)
	handler := NewResponsesHandler(cs, nil, nil)

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
	cfg := &config.Config{
		Models: []config.ModelConfig{
			{Name: "model-a", Backend: "http://localhost:9999/v1", Model: "model-a", Timeout: 10},
		},
	}
	cs := config.NewTestConfigStore(cfg)
	handler := NewResponsesHandler(cs, nil, nil)

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
	cfg := &config.Config{}
	cs := config.NewTestConfigStore(cfg)
	handler := NewResponsesHandler(cs, nil, nil)

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
	cfg := &config.Config{}
	cs := config.NewTestConfigStore(cfg)
	handler := NewResponsesHandler(cs, nil, nil)

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

	cfg := &config.Config{
		Models: []config.ModelConfig{{
			Name: "test-model", Backend: ts.URL + "/v1",
			Model: "test-model", Timeout: 10,
		}},
	}
	cs := config.NewTestConfigStore(cfg)
	handler := NewResponsesHandler(cs, nil, nil)

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

	cfg := &config.Config{
		Models: []config.ModelConfig{{
			Name: "test-model", Backend: ts.URL + "/v1",
			Model: "test-model", Timeout: 10,
		}},
	}
	cs := config.NewTestConfigStore(cfg)
	handler := NewResponsesHandler(cs, nil, nil)

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
	firstCallCount := callCount

	req2 := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req2)

	if w2.Code != http.StatusOK {
		t.Fatalf("second request: expected 200, got %d", w2.Code)
	}
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

	cfg := &config.Config{
		Models: []config.ModelConfig{{
			Name: "test-model", Backend: ts.URL + "/v1",
			Model: "test-model", Timeout: 10,
			ResponsesMode: "translate",
		}},
	}
	cs := config.NewTestConfigStore(cfg)
	handler := NewResponsesHandler(cs, nil, nil)

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
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	cfg := &config.Config{
		Models: []config.ModelConfig{{
			Name: "test-model", Backend: ts.URL + "/v1",
			Model: "test-model", Timeout: 10,
			ResponsesMode: "native",
		}},
	}
	cs := config.NewTestConfigStore(cfg)
	handler := NewResponsesHandler(cs, nil, nil)

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

	cfg := &config.Config{
		Models: []config.ModelConfig{{
			Name: "test-model", Backend: ts.URL + "/v1",
			Model: "test-model", Timeout: 10,
		}},
	}
	cs := config.NewTestConfigStore(cfg)
	handler := NewResponsesHandler(cs, nil, nil)

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

	if len(output) != 3 {
		t.Fatalf("expected 3 output items (2 user + 1 summary), got %d", len(output))
	}

	first := output[0].(map[string]any)
	if first["role"] != "user" {
		t.Fatalf("expected first output to be user message, got %v", first["role"])
	}
	second := output[1].(map[string]any)
	if second["role"] != "user" {
		t.Fatalf("expected second output to be user message, got %v", second["role"])
	}

	summary := output[2].(map[string]any)
	if summary["role"] != "assistant" || summary["type"] != "message" {
		t.Fatalf("expected assistant message, got %v", summary)
	}

	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) < 2 {
		t.Fatalf("expected multiple messages in upstream request, got %d", len(msgs))
	}
	firstMsg := msgs[0].(map[string]any)
	if firstMsg["role"] != "system" || !strings.Contains(firstMsg["content"].(string), "Summarize") {
		t.Fatalf("expected summarization system prompt, got %v", firstMsg)
	}

	usage, _ := resp["usage"].(map[string]any)
	if usage == nil || usage["total_tokens"] != float64(120) {
		t.Fatalf("expected usage with total_tokens=120, got %v", resp["usage"])
	}
}

func TestHandleCompact_RejectsAnthropicBackend(t *testing.T) {
	cfg := &config.Config{
		Models: []config.ModelConfig{{
			Name: "claude-test", Backend: "http://localhost:9999",
			Model: "claude-test", Timeout: 10, Type: config.BackendAnthropic,
		}},
	}
	cs := config.NewTestConfigStore(cfg)
	handler := NewResponsesHandler(cs, nil, nil)

	body := `{"model":"claude-test","input":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.HandleCompact(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for anthropic backend, got %d", w.Code)
	}
}

// --- Codex-specific translation tests ---

func TestTranslateInput_McpToolCallOutput(t *testing.T) {
	input := json.RawMessage(`[
		{"role": "user", "content": "Search for files"},
		{"type": "function_call", "call_id": "call_mcp1", "name": "mcp__fs__list_files", "arguments": "{\"path\":\"/tmp\"}"},
		{"type": "mcp_tool_call_output", "call_id": "call_mcp1", "output": "file1.txt\nfile2.txt"},
		{"role": "user", "content": "Thanks!"}
	]`)
	msgs, err := translateInput(input, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(msgs) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(msgs))
	}
	if msgs[2]["role"] != "tool" || msgs[2]["tool_call_id"] != "call_mcp1" {
		t.Fatalf("expected tool message for mcp_tool_call_output, got %v", msgs[2])
	}
	if msgs[2]["content"] != "file1.txt\nfile2.txt" {
		t.Fatalf("expected mcp output content, got %v", msgs[2]["content"])
	}
}

func TestTranslateInput_SkipsMcpListTools(t *testing.T) {
	input := json.RawMessage(`[
		{"role": "user", "content": "Hello"},
		{"type": "mcp_list_tools"},
		{"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "Hi"}]}
	]`)
	msgs, err := translateInput(input, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages (mcp_list_tools skipped), got %d", len(msgs))
	}
}

func TestTranslateInput_LocalShellCall(t *testing.T) {
	input := json.RawMessage(`[
		{"role": "user", "content": "List files"},
		{"type": "local_shell_call", "call_id": "call_shell1", "name": "shell", "action": {"type":"exec","command":["ls","-la"]}},
		{"type": "local_shell_call_output", "call_id": "call_shell1", "output": "total 0\ndrwxr-xr-x 2 user user 64 Jan 1 00:00 ."}
	]`)
	msgs, err := translateInput(input, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(msgs))
	}
	// The local_shell_call should produce an assistant message with tool_calls.
	if msgs[1]["role"] != "assistant" {
		t.Fatalf("expected assistant role for local_shell_call, got %v", msgs[1]["role"])
	}
	tcs, ok := msgs[1]["tool_calls"].([]any)
	if !ok || len(tcs) != 1 {
		t.Fatalf("expected 1 tool_call, got %v", msgs[1]["tool_calls"])
	}
	tc := tcs[0].(map[string]any)
	fn := tc["function"].(map[string]any)
	if fn["name"] != "shell" {
		t.Fatalf("expected function name 'shell', got %v", fn["name"])
	}
	// The output should produce a tool message.
	if msgs[2]["role"] != "tool" || msgs[2]["tool_call_id"] != "call_shell1" {
		t.Fatalf("expected tool message for local_shell_call_output, got %v", msgs[2])
	}
}

func TestTranslateInput_CustomToolCall(t *testing.T) {
	input := json.RawMessage(`[
		{"role": "user", "content": "Apply patch"},
		{"type": "custom_tool_call", "call_id": "call_ct1", "name": "apply_patch", "input": "--- a/file.txt\n+++ b/file.txt\n@@ -1 +1 @@\n-old\n+new"},
		{"type": "custom_tool_call_output", "call_id": "call_ct1", "output": "Patch applied successfully"}
	]`)
	msgs, err := translateInput(input, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(msgs))
	}
	if msgs[1]["role"] != "assistant" {
		t.Fatalf("expected assistant role for custom_tool_call, got %v", msgs[1]["role"])
	}
	tcs, ok := msgs[1]["tool_calls"].([]any)
	if !ok || len(tcs) != 1 {
		t.Fatalf("expected 1 tool_call for custom_tool_call, got %v", msgs[1]["tool_calls"])
	}
	tc := tcs[0].(map[string]any)
	fn := tc["function"].(map[string]any)
	if fn["name"] != "apply_patch" {
		t.Fatalf("expected function name 'apply_patch', got %v", fn["name"])
	}
	if msgs[2]["role"] != "tool" || msgs[2]["tool_call_id"] != "call_ct1" {
		t.Fatalf("expected tool message for custom_tool_call_output, got %v", msgs[2])
	}
}

func TestTranslateInput_SkipsCompactionAndWebSearch(t *testing.T) {
	input := json.RawMessage(`[
		{"role": "user", "content": "Hello"},
		{"type": "compaction", "encrypted_content": "opaque_data"},
		{"type": "web_search_call", "id": "ws_1", "status": "completed", "action": {"type":"search","query":"test"}},
		{"type": "image_generation_call", "id": "ig_1", "status": "completed"},
		{"type": "tool_search_call", "call_id": "ts_1", "execution": "client", "arguments": {"query": "test"}},
		{"type": "tool_search_output", "call_id": "ts_1", "output": "results"},
		{"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "Hi"}]}
	]`)
	msgs, err := translateInput(input, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages (all server-side items skipped), got %d", len(msgs))
	}
}

func TestTranslateInput_ConsecutiveToolCalls(t *testing.T) {
	// Codex can send multiple consecutive function_call items which should merge into one assistant message.
	input := json.RawMessage(`[
		{"role": "user", "content": "Do two things"},
		{"type": "function_call", "call_id": "call_a", "name": "fn_a", "arguments": "{}"},
		{"type": "function_call", "call_id": "call_b", "name": "fn_b", "arguments": "{}"},
		{"type": "function_call_output", "call_id": "call_a", "output": "done_a"},
		{"type": "function_call_output", "call_id": "call_b", "output": "done_b"}
	]`)
	msgs, err := translateInput(input, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// user + assistant(2 tool_calls) + tool_a + tool_b = 4 messages
	if len(msgs) != 4 {
		t.Fatalf("expected 4 messages, got %d: %v", len(msgs), msgs)
	}
	tcs, ok := msgs[1]["tool_calls"].([]any)
	if !ok || len(tcs) != 2 {
		t.Fatalf("expected 2 tool_calls merged in assistant, got %v", msgs[1]["tool_calls"])
	}
}

// --- Codex-specific streaming tests ---

func TestResponsesHandler_StreamingUsageFormat(t *testing.T) {
	// Verify that the response.completed event includes input_tokens_details and output_tokens_details.
	handler, ts := newTestResponsesHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		chunks := []string{
			`{"id":"c1","model":"test-model","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
			`{"id":"c1","model":"test-model","choices":[{"index":0,"delta":{"content":"Hi"},"finish_reason":null}]}`,
			`{"id":"c1","model":"test-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`{"id":"c1","model":"test-model","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`,
		}
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
			flusher.Flush()
		}
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
	})
	defer ts.Close()

	body := `{"model":"test-model","input":"Hello","stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	events := parseSSEEvents(w.Body.String())
	for _, e := range events {
		if e.event == "response.completed" {
			var d map[string]any
			json.Unmarshal([]byte(e.data), &d)
			resp := d["response"].(map[string]any)
			u := resp["usage"].(map[string]any)

			// Codex expects these fields to be present (even if null).
			if _, ok := u["input_tokens_details"]; !ok {
				t.Error("response.completed usage missing input_tokens_details field")
			}
			if _, ok := u["output_tokens_details"]; !ok {
				t.Error("response.completed usage missing output_tokens_details field")
			}
			if u["input_tokens"] != float64(5) {
				t.Errorf("expected input_tokens=5, got %v", u["input_tokens"])
			}
			if u["total_tokens"] != float64(7) {
				t.Errorf("expected total_tokens=7, got %v", u["total_tokens"])
			}
			return
		}
	}
	t.Error("response.completed event not found")
}

func TestResponsesHandler_NonStreamingUsageFormat(t *testing.T) {
	handler, ts := newTestResponsesHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-1", "model": "test-model", "created": 0,
			"choices": []map[string]any{{
				"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": "OK"},
			}},
			"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 3, "total_tokens": 13},
		})
	})
	defer ts.Close()

	body := `{"model":"test-model","input":"Hello","stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	u := resp["usage"].(map[string]any)
	if _, ok := u["input_tokens_details"]; !ok {
		t.Error("non-streaming usage missing input_tokens_details field")
	}
	if _, ok := u["output_tokens_details"]; !ok {
		t.Error("non-streaming usage missing output_tokens_details field")
	}
}

func TestResponsesHandler_StreamingReasoningItems(t *testing.T) {
	// Backend sends reasoning tokens; proxy should emit proper reasoning output items.
	handler, ts := newTestResponsesHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		chunks := []string{
			`{"id":"c1","model":"test-model","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
			`{"id":"c1","model":"test-model","choices":[{"index":0,"delta":{"reasoning":"Let me think about this..."},"finish_reason":null}]}`,
			`{"id":"c1","model":"test-model","choices":[{"index":0,"delta":{"reasoning":"Okay, I know the answer."},"finish_reason":null}]}`,
			`{"id":"c1","model":"test-model","choices":[{"index":0,"delta":{"content":"The answer is 42."},"finish_reason":null}]}`,
			`{"id":"c1","model":"test-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`{"id":"c1","model":"test-model","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":10,"total_tokens":15}}`,
		}
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
			flusher.Flush()
		}
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
	})
	defer ts.Close()

	body := `{"model":"test-model","input":"What is the meaning of life?","stream":true,"reasoning":{"effort":"high"}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	events := parseSSEEvents(w.Body.String())

	// Should have reasoning output_item.added event.
	var foundReasoningAdded, foundReasoningDelta, foundReasoningDone bool
	var foundTextDelta bool
	var reasoningDoneText string
	for _, e := range events {
		switch e.event {
		case "response.output_item.added":
			var d map[string]any
			json.Unmarshal([]byte(e.data), &d)
			item := d["item"].(map[string]any)
			if item["type"] == "reasoning" {
				foundReasoningAdded = true
			}
		case "response.reasoning_summary_text.delta":
			foundReasoningDelta = true
		case "response.output_item.done":
			var d map[string]any
			json.Unmarshal([]byte(e.data), &d)
			item := d["item"].(map[string]any)
			if item["type"] == "reasoning" {
				foundReasoningDone = true
				summary := item["summary"].([]any)
				if len(summary) > 0 {
					part := summary[0].(map[string]any)
					reasoningDoneText = part["text"].(string)
				}
			}
		case "response.output_text.delta":
			var d map[string]any
			json.Unmarshal([]byte(e.data), &d)
			if d["delta"] == "The answer is 42." {
				foundTextDelta = true
			}
		}
	}

	if !foundReasoningAdded {
		t.Error("expected response.output_item.added with type reasoning")
	}
	if !foundReasoningDelta {
		t.Error("expected response.reasoning_summary_text.delta events")
	}
	if !foundReasoningDone {
		t.Error("expected response.output_item.done with type reasoning")
	}
	expectedReasoning := "Let me think about this...Okay, I know the answer."
	if reasoningDoneText != expectedReasoning {
		t.Errorf("expected reasoning text %q, got %q", expectedReasoning, reasoningDoneText)
	}
	if !foundTextDelta {
		t.Error("expected text delta with content after reasoning phase")
	}

	// The completed event should have both reasoning and message in output.
	for _, e := range events {
		if e.event == "response.completed" {
			var d map[string]any
			json.Unmarshal([]byte(e.data), &d)
			resp := d["response"].(map[string]any)
			output := resp["output"].([]any)
			if len(output) != 2 {
				t.Errorf("expected 2 output items (reasoning + message), got %d", len(output))
			}
			if len(output) >= 2 {
				first := output[0].(map[string]any)
				second := output[1].(map[string]any)
				if first["type"] != "reasoning" {
					t.Errorf("expected first output item to be reasoning, got %v", first["type"])
				}
				if second["type"] != "message" {
					t.Errorf("expected second output item to be message, got %v", second["type"])
				}
			}
		}
	}
}

func TestResponsesHandler_StreamingErrorEvent(t *testing.T) {
	// When backend returns an error during streaming, proxy should emit response.failed.
	handler, ts := newTestResponsesHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":{"message":"Rate limit exceeded","type":"rate_limit_error"}}`))
	})
	defer ts.Close()

	body := `{"model":"test-model","input":"Hello","stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	// The proxy should return 200 with an SSE response.failed event.
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (SSE error event), got %d", w.Code)
	}
	events := parseSSEEvents(w.Body.String())
	var foundFailed bool
	for _, e := range events {
		if e.event == "response.failed" {
			foundFailed = true
			var d map[string]any
			json.Unmarshal([]byte(e.data), &d)
			resp := d["response"].(map[string]any)
			if resp["status"] != "failed" {
				t.Errorf("expected status=failed, got %v", resp["status"])
			}
			errObj := resp["error"].(map[string]any)
			if errObj["type"] != "upstream_error" {
				t.Errorf("expected error type=upstream_error, got %v", errObj["type"])
			}
		}
	}
	if !foundFailed {
		t.Error("expected response.failed event for upstream error")
	}
}

func TestResponsesHandler_StreamingIncompleteOnLength(t *testing.T) {
	handler, ts := newTestResponsesHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		chunks := []string{
			`{"id":"c1","model":"test-model","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
			`{"id":"c1","model":"test-model","choices":[{"index":0,"delta":{"content":"Truncated output"},"finish_reason":null}]}`,
			`{"id":"c1","model":"test-model","choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`,
		}
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
			flusher.Flush()
		}
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
	})
	defer ts.Close()

	body := `{"model":"test-model","input":"Write a long essay","stream":true,"max_output_tokens":10}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	events := parseSSEEvents(w.Body.String())
	var foundIncomplete bool
	for _, e := range events {
		if e.event == "response.incomplete" {
			foundIncomplete = true
			var d map[string]any
			json.Unmarshal([]byte(e.data), &d)
			resp := d["response"].(map[string]any)
			if resp["status"] != "incomplete" {
				t.Errorf("expected status=incomplete, got %v", resp["status"])
			}
			details := resp["incomplete_details"].(map[string]any)
			if details["reason"] != "max_output_tokens" {
				t.Errorf("expected reason=max_output_tokens, got %v", details["reason"])
			}
		}
	}
	if !foundIncomplete {
		t.Error("expected response.incomplete event for finish_reason=length")
	}
}

func TestResponsesHandler_StreamingMultipleToolCallTypes(t *testing.T) {
	// Simulate a response with both a text message and multiple parallel tool calls.
	handler, ts := newTestResponsesHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		chunks := []string{
			`{"id":"c1","model":"test-model","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
			`{"id":"c1","model":"test-model","choices":[{"index":0,"delta":{"content":"Let me run two commands."},"finish_reason":null}]}`,
			// Two parallel tool calls.
			`{"id":"c1","model":"test-model","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"shell","arguments":""}}]},"finish_reason":null}]}`,
			`{"id":"c1","model":"test-model","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"command\":[\"ls\"]}"}}]},"finish_reason":null}]}`,
			`{"id":"c1","model":"test-model","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_2","type":"function","function":{"name":"shell","arguments":""}}]},"finish_reason":null}]}`,
			`{"id":"c1","model":"test-model","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"{\"command\":[\"pwd\"]}"}}]},"finish_reason":null}]}`,
			`{"id":"c1","model":"test-model","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			`{"id":"c1","model":"test-model","choices":[],"usage":{"prompt_tokens":20,"completion_tokens":15,"total_tokens":35}}`,
		}
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
			flusher.Flush()
		}
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
	})
	defer ts.Close()

	body := `{"model":"test-model","input":"Run ls and pwd","stream":true,"tools":[{"type":"function","name":"shell","parameters":{}}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	events := parseSSEEvents(w.Body.String())

	// Count output_item.done events — should have 1 message + 2 function_calls = 3.
	var msgCount, fcCount int
	for _, e := range events {
		if e.event == "response.output_item.done" {
			var d map[string]any
			json.Unmarshal([]byte(e.data), &d)
			item := d["item"].(map[string]any)
			switch item["type"] {
			case "message":
				msgCount++
			case "function_call":
				fcCount++
			}
		}
	}
	if msgCount != 1 {
		t.Errorf("expected 1 message output_item.done, got %d", msgCount)
	}
	if fcCount != 2 {
		t.Errorf("expected 2 function_call output_item.done, got %d", fcCount)
	}

	// The completed event should have all 3 items in output.
	for _, e := range events {
		if e.event == "response.completed" {
			var d map[string]any
			json.Unmarshal([]byte(e.data), &d)
			resp := d["response"].(map[string]any)
			output := resp["output"].([]any)
			if len(output) != 3 {
				t.Errorf("expected 3 output items in response.completed, got %d", len(output))
			}
		}
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
