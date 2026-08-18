package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"go-llm-proxy/internal/api"
	"go-llm-proxy/internal/auth"
	"go-llm-proxy/internal/config"
)

func newAnthropicProxyHandler(t *testing.T, upstream http.HandlerFunc) (*ProxyHandler, *config.ConfigStore, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(upstream)
	cs := config.NewTestConfigStore(&config.Config{
		Models: []config.ModelConfig{{
			Name: "test-model", Model: "upstream-model", Type: config.BackendAnthropic,
			Backend: server.URL, APIKey: "anthropic-secret", Timeout: 10,
		}},
	})
	return NewProxyHandler(cs, nil, nil), cs, server
}

func TestBuildAnthropicRequestFromChat(t *testing.T) {
	body := []byte(`{
		"model":"upstream-model",
		"messages":[
			{"role":"system","content":"System one"},
			{"role":"developer","content":"System two"},
			{"role":"user","content":[{"type":"text","text":"Weather?"}]},
			{"role":"assistant","content":"Checking","tool_calls":[{"id":"call_1","type":"function","function":{"name":"weather","arguments":"{\"city\":\"Detroit\"}"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"42 F"}
		],
		"tools":[{"type":"function","function":{"name":"weather","description":"Get weather","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}],
		"tool_choice":"required",
		"parallel_tool_calls":false,
		"max_completion_tokens":123,
		"temperature":0.2,
		"top_p":0.9,
		"stop":["END"],
		"user":"user-1"
	}`)

	translated, parsed, err := buildAnthropicRequestFromChat(body)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.MaxCompletionTokens == nil || translated["max_tokens"] != 123 {
		t.Fatalf("max_tokens translation: %#v", translated["max_tokens"])
	}
	if translated["system"] != "System one\nSystem two" {
		t.Fatalf("system: %#v", translated["system"])
	}

	messages := translated["messages"].([]map[string]any)
	if len(messages) != 3 {
		t.Fatalf("messages=%d: %#v", len(messages), messages)
	}
	assistantContent := messages[1]["content"].([]map[string]any)
	toolUse := assistantContent[1]
	if toolUse["type"] != "tool_use" || toolUse["id"] != "call_1" || toolUse["name"] != "weather" {
		t.Fatalf("assistant tool_use: %#v", toolUse)
	}
	input := toolUse["input"].(map[string]any)
	if input["city"] != "Detroit" {
		t.Fatalf("tool input: %#v", input)
	}
	toolResult := messages[2]["content"].([]map[string]any)[0]
	if toolResult["type"] != "tool_result" || toolResult["tool_use_id"] != "call_1" || toolResult["content"] != "42 F" {
		t.Fatalf("tool result: %#v", toolResult)
	}

	tools := translated["tools"].([]map[string]any)
	if len(tools) != 1 || tools[0]["name"] != "weather" {
		t.Fatalf("tools: %#v", tools)
	}
	choice := translated["tool_choice"].(map[string]any)
	if choice["type"] != "any" || choice["disable_parallel_tool_use"] != true {
		t.Fatalf("tool choice: %#v", choice)
	}
	metadata := translated["metadata"].(map[string]any)
	if metadata["user_id"] != "user-1" {
		t.Fatalf("metadata: %#v", metadata)
	}
}

func TestBuildChatResponseFromAnthropic(t *testing.T) {
	body := []byte(`{
		"id":"msg_123","type":"message","role":"assistant","model":"upstream-model",
		"content":[
			{"type":"thinking","thinking":"considering"},
			{"type":"text","text":"I'll check."},
			{"type":"tool_use","id":"toolu_1","name":"weather","input":{"city":"Detroit"}}
		],
		"stop_reason":"tool_use",
		"usage":{"input_tokens":7,"output_tokens":4,"cache_creation_input_tokens":2,"cache_read_input_tokens":3}
	}`)

	response, usage, err := buildChatResponseFromAnthropic(body, "test-model")
	if err != nil {
		t.Fatal(err)
	}
	if response.Object != "chat.completion" || response.Model != "test-model" || response.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("response metadata: %#v", response)
	}
	message := response.Choices[0].Message
	if message.Content == nil || *message.Content != "I'll check." {
		t.Fatalf("content: %#v", message.Content)
	}
	if message.Reasoning == nil || *message.Reasoning != "considering" {
		t.Fatalf("reasoning: %#v", message.Reasoning)
	}
	if len(message.ToolCalls) != 1 || message.ToolCalls[0].Function.Arguments != `{"city":"Detroit"}` {
		t.Fatalf("tool calls: %#v", message.ToolCalls)
	}
	if usage.PromptTokens != 12 || usage.CompletionTokens != 4 || usage.TotalTokens != 16 {
		t.Fatalf("usage: %#v", usage)
	}
	if usage.PromptTokensDetails == nil || usage.PromptTokensDetails.CachedTokens != 3 {
		t.Fatalf("cached usage: %#v", usage.PromptTokensDetails)
	}
}

func TestProxyAnthropicChatNonStreaming(t *testing.T) {
	var gotPath, gotAPIKey, gotVersion, gotAccept string
	var gotBody map[string]any
	proxy, _, server := newAnthropicProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAPIKey = r.Header.Get("X-Api-Key")
		gotVersion = r.Header.Get("Anthropic-Version")
		gotAccept = r.Header.Get("Accept")
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Request-Id", "req_upstream")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "msg_1", "type": "message", "role": "assistant", "model": "upstream-model",
			"content":     []map[string]any{{"type": "text", "text": "OK"}},
			"stop_reason": "end_turn",
			"usage":       map[string]any{"input_tokens": 5, "output_tokens": 1},
		})
	})
	defer server.Close()

	body := `{"model":"test-model","messages":[{"role":"system","content":"Be brief"},{"role":"user","content":"hi"}],"max_tokens":10}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	proxy.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if gotPath != "/v1/messages" || gotAPIKey != "anthropic-secret" || gotVersion != defaultAnthropicVersion || gotAccept != "application/json" {
		t.Fatalf("upstream request path=%q key=%q version=%q accept=%q", gotPath, gotAPIKey, gotVersion, gotAccept)
	}
	if gotBody["model"] != "upstream-model" || gotBody["system"] != "Be brief" || gotBody["max_tokens"] != float64(10) {
		t.Fatalf("translated request: %#v", gotBody)
	}
	if w.Header().Get("Request-Id") != "req_upstream" {
		t.Fatalf("request id header: %q", w.Header().Get("Request-Id"))
	}
	var response api.ChatResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Object != "chat.completion" || response.Model != "test-model" || len(response.Choices) != 1 {
		t.Fatalf("translated response: %#v", response)
	}
	if response.Choices[0].Message.Content == nil || *response.Choices[0].Message.Content != "OK" {
		t.Fatalf("content: %#v", response.Choices[0].Message.Content)
	}
}

func TestProxyAnthropicChatStreamingTools(t *testing.T) {
	var gotPath string
	var gotStream bool
	proxy, _, server := newAnthropicProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		var request map[string]any
		json.NewDecoder(r.Body).Decode(&request)
		gotStream, _ = request["stream"].(bool)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		events := []struct {
			name string
			data string
		}{
			{"message_start", `{"type":"message_start","message":{"id":"msg_1","model":"upstream-model","usage":{"input_tokens":8,"output_tokens":1}}}`},
			{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
			{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Checking"}}`},
			{"content_block_stop", `{"type":"content_block_stop","index":0}`},
			{"content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"weather","input":{}}}`},
			{"content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}`},
			{"content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"Detroit\"}"}}`},
			{"content_block_stop", `{"type":"content_block_stop","index":1}`},
			{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":7}}`},
			{"message_stop", `{"type":"message_stop"}`},
		}
		for _, event := range events {
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.name, event.data)
			flusher.Flush()
		}
	})
	defer server.Close()

	body := `{"model":"test-model","messages":[{"role":"user","content":"weather?"}],"stream":true,"stream_options":{"include_usage":true},"tools":[{"type":"function","function":{"name":"weather","parameters":{"type":"object"}}}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	proxy.ServeHTTP(w, req)

	if w.Code != http.StatusOK || gotPath != "/v1/messages" || !gotStream {
		t.Fatalf("status=%d path=%q stream=%v body=%s", w.Code, gotPath, gotStream, w.Body.String())
	}
	if w.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("content type: %q", w.Header().Get("Content-Type"))
	}

	var role, content, toolName, arguments, finish string
	var usage *api.ChunkUsage
	for _, line := range strings.Split(w.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		var chunk api.ChatChunk
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk) != nil {
			continue
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Role != "" {
				role = choice.Delta.Role
			}
			if choice.Delta.Content != nil {
				content += *choice.Delta.Content
			}
			for _, call := range choice.Delta.ToolCalls {
				if call.Function != nil {
					if call.Function.Name != "" {
						toolName = call.Function.Name
					}
					arguments += call.Function.Arguments
				}
			}
			if choice.FinishReason != nil {
				finish = *choice.FinishReason
			}
		}
	}
	if role != "assistant" || content != "Checking" || toolName != "weather" || arguments != `{"city":"Detroit"}` || finish != "tool_calls" {
		t.Fatalf("translated stream role=%q content=%q tool=%q args=%q finish=%q\n%s", role, content, toolName, arguments, finish, w.Body.String())
	}
	if usage == nil || usage.PromptTokens != 8 || usage.CompletionTokens != 7 || usage.TotalTokens != 15 {
		t.Fatalf("usage: %#v", usage)
	}
	if !strings.Contains(w.Body.String(), "data: [DONE]") {
		t.Fatal("missing stream terminator")
	}
}

func TestProxyAnthropicChatSanitizesUpstreamError(t *testing.T) {
	proxy, _, server := newAnthropicProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		io.WriteString(w, `{"error":{"message":"secret internal detail"}}`)
	})
	defer server.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	proxy.ServeHTTP(w, req)

	if w.Code != http.StatusUnprocessableEntity || strings.Contains(w.Body.String(), "secret internal detail") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "backend returned HTTP 422") {
		t.Fatalf("unexpected error body: %s", w.Body.String())
	}
}

func TestProxyAnthropicChatFailover(t *testing.T) {
	var mu sync.Mutex
	var paths, hosts []string
	requestCount := 0
	upstream := func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requestCount++
		current := requestCount
		paths = append(paths, r.URL.Path)
		hosts = append(hosts, r.Host)
		mu.Unlock()
		if current == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"id": "msg_ok", "content": []map[string]any{{"type": "text", "text": "recovered"}},
			"stop_reason": "end_turn", "usage": map[string]any{"input_tokens": 1, "output_tokens": 1},
		})
	}
	first := httptest.NewServer(http.HandlerFunc(upstream))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(upstream))
	defer second.Close()

	cs := config.NewTestConfigStore(&config.Config{Models: []config.ModelConfig{{
		Name: "test-model", Model: "upstream-model", Type: config.BackendAnthropic, Timeout: 10,
		Backends: []config.BackendConfig{{URL: first.URL}, {URL: second.URL}},
	}}})
	proxy := NewProxyHandler(cs, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	proxy.ServeHTTP(w, req)

	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "recovered") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if requestCount != 2 || len(hosts) != 2 || hosts[0] == hosts[1] || paths[0] != "/v1/messages" || paths[1] != "/v1/messages" {
		t.Fatalf("requests=%d hosts=%v paths=%v", requestCount, hosts, paths)
	}
}

func TestProxyAnthropicChatHonorsModelACL(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer server.Close()
	cs := config.NewTestConfigStore(&config.Config{
		Keys: []config.KeyConfig{{Key: "restricted", Models: []string{"other-model"}}},
		Models: []config.ModelConfig{{
			Name: "test-model", Model: "upstream-model", Type: config.BackendAnthropic,
			Backend: server.URL, Timeout: 10,
		}},
	})
	handler := auth.AuthMiddleware(cs, NewProxyHandler(cs, nil, nil))
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer restricted")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden || called {
		t.Fatalf("status=%d called=%v body=%s", w.Code, called, w.Body.String())
	}
}

func TestProxyAnthropicChatResendUsesMessagesProtocol(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		json.NewEncoder(w).Encode(map[string]any{
			"id": "msg_1", "content": []map[string]any{{"type": "text", "text": "done"}},
			"stop_reason": "end_turn", "usage": map[string]any{"input_tokens": 2, "output_tokens": 1},
		})
	}))
	defer server.Close()
	model := &config.ModelConfig{
		Name: "test-model", Model: "upstream-model", Type: config.BackendAnthropic,
		Backend: server.URL, Timeout: 10,
	}
	response, err := sendChatCompletionsRequest(context.Background(), http.DefaultClient, map[string]any{
		"model": "upstream-model", "messages": []map[string]any{{"role": "user", "content": "continue"}},
	}, model)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/messages" || response.Model != "test-model" {
		t.Fatalf("path=%q response=%#v", gotPath, response)
	}
}

func TestProxyAnthropicChatStreamingResendUsesMessagesProtocol(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: message_start\n")
		io.WriteString(w, `data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":2,"output_tokens":1}}}`+"\n\n")
		io.WriteString(w, "event: content_block_delta\n")
		io.WriteString(w, `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"continued"}}`+"\n\n")
		io.WriteString(w, "event: message_delta\n")
		io.WriteString(w, `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`+"\n\n")
		io.WriteString(w, "event: message_stop\n")
		io.WriteString(w, `data: {"type":"message_stop"}`+"\n\n")
	}))
	defer server.Close()
	model := &config.ModelConfig{
		Name: "test-model", Model: "upstream-model", Type: config.BackendAnthropic,
		Backend: server.URL, Timeout: 10,
	}
	proxy := &ProxyHandler{client: http.DefaultClient}
	w := httptest.NewRecorder()
	proxy.reStreamFromBackend(context.Background(), w, w, true, map[string]any{
		"model": "upstream-model", "messages": []map[string]any{{"role": "user", "content": "continue"}},
	}, model)

	if gotPath != "/v1/messages" || !strings.Contains(w.Body.String(), "continued") || !strings.Contains(w.Body.String(), "data: [DONE]") {
		t.Fatalf("path=%q body=%s", gotPath, w.Body.String())
	}
}

func TestNativeAnthropicMessagesRouteUnaffected(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		json.NewDecoder(r.Body).Decode(&gotBody)
		json.NewEncoder(w).Encode(map[string]any{
			"id": "msg_native", "type": "message", "role": "assistant", "model": "upstream-model",
			"content":     []map[string]any{{"type": "text", "text": "native"}},
			"stop_reason": "end_turn", "usage": map[string]any{"input_tokens": 1, "output_tokens": 1},
		})
	}))
	defer server.Close()
	cs := config.NewTestConfigStore(&config.Config{Models: []config.ModelConfig{{
		Name: "test-model", Model: "upstream-model", Type: config.BackendAnthropic,
		Backend: server.URL, Timeout: 10,
	}}})
	handler := NewMessagesHandler(cs, nil, nil)
	for _, route := range []string{"/v1/messages", "/anthropic/v1/messages"} {
		t.Run(route, func(t *testing.T) {
			gotPath = ""
			gotBody = nil
			req := httptest.NewRequest(http.MethodPost, route, strings.NewReader(`{"model":"test-model","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			if w.Code != http.StatusOK || gotPath != "/v1/messages" || gotBody["model"] != "upstream-model" {
				t.Fatalf("status=%d path=%q upstream_body=%#v response=%s", w.Code, gotPath, gotBody, w.Body.String())
			}
		})
	}
}
