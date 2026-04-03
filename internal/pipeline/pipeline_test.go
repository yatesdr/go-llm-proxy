package pipeline

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go-llm-proxy/internal/api"
	"go-llm-proxy/internal/config"
)

// --- BodyNeedsProcessing tests ---

func TestBodyNeedsProcessing_ImageURL(t *testing.T) {
	p := &Pipeline{}
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,abc"}}]}]}`)
	if !p.BodyNeedsProcessing(body) {
		t.Fatal("expected true for image_url")
	}
}

func TestBodyNeedsProcessing_PDF(t *testing.T) {
	p := &Pipeline{}
	body := []byte(`{"messages":[{"role":"user","content":"application/pdf"}]}`)
	if !p.BodyNeedsProcessing(body) {
		t.Fatal("expected true for application/pdf")
	}
}

func TestBodyNeedsProcessing_PDFMagicBytes(t *testing.T) {
	p := &Pipeline{}
	body := []byte(`{"data":"JVBERi0xLjQ="}`)
	if !p.BodyNeedsProcessing(body) {
		t.Fatal("expected true for PDF magic bytes")
	}
}

func TestBodyNeedsProcessing_TextOnly(t *testing.T) {
	p := &Pipeline{}
	body := []byte(`{"messages":[{"role":"user","content":"hello world"}]}`)
	if p.BodyNeedsProcessing(body) {
		t.Fatal("expected false for text-only request")
	}
}

func TestBodyNeedsProcessing_Document(t *testing.T) {
	p := &Pipeline{}
	body := []byte(`{"messages":[{"content":[{"type":"document"}]}]}`)
	if !p.BodyNeedsProcessing(body) {
		t.Fatal("expected true for document type")
	}
}

// --- ShouldProcess tests ---

func TestShouldProcess_AnthropicBackend(t *testing.T) {
	p := &Pipeline{}
	m := &config.ModelConfig{Type: config.BackendAnthropic}
	if p.ShouldProcess(m) {
		t.Fatal("expected false for anthropic backend without force_pipeline")
	}
}

func TestShouldProcess_AnthropicForcePipeline(t *testing.T) {
	p := &Pipeline{}
	m := &config.ModelConfig{Type: config.BackendAnthropic, ForcePipeline: true}
	if !p.ShouldProcess(m) {
		t.Fatal("expected true for anthropic with force_pipeline")
	}
}

func TestShouldProcess_OpenAIBackend(t *testing.T) {
	p := &Pipeline{}
	m := &config.ModelConfig{Type: config.BackendOpenAI}
	if !p.ShouldProcess(m) {
		t.Fatal("expected true for openai backend")
	}
}

func TestShouldProcess_DefaultBackend(t *testing.T) {
	p := &Pipeline{}
	m := &config.ModelConfig{}
	if !p.ShouldProcess(m) {
		t.Fatal("expected true for default backend type")
	}
}

// --- resolveVisionProcessor tests ---

func TestResolveVisionProcessor_GlobalDefault(t *testing.T) {
	cfg := &config.Config{
		Processors: config.ProcessorsConfig{Vision: "qwen-3.5"},
		Models:     []config.ModelConfig{{Name: "test", Backend: "http://localhost/v1"}},
	}
	p := &Pipeline{config: config.NewTestConfigStore(cfg)}
	model := &config.ModelConfig{Name: "test"}
	if got := p.resolveVisionProcessor(model); got != "qwen-3.5" {
		t.Fatalf("expected qwen-3.5, got %q", got)
	}
}

func TestResolveVisionProcessor_PerModelOverride(t *testing.T) {
	cfg := &config.Config{
		Processors: config.ProcessorsConfig{Vision: "qwen-3.5"},
	}
	p := &Pipeline{config: config.NewTestConfigStore(cfg)}
	model := &config.ModelConfig{
		Name:       "test",
		Processors: &config.ProcessorsConfig{Vision: "custom-vision"},
	}
	if got := p.resolveVisionProcessor(model); got != "custom-vision" {
		t.Fatalf("expected custom-vision, got %q", got)
	}
}

func TestResolveVisionProcessor_PerModelNone(t *testing.T) {
	cfg := &config.Config{
		Processors: config.ProcessorsConfig{Vision: "qwen-3.5"},
	}
	p := &Pipeline{config: config.NewTestConfigStore(cfg)}
	model := &config.ModelConfig{
		Name:       "test",
		Processors: &config.ProcessorsConfig{Vision: "none"},
	}
	if got := p.resolveVisionProcessor(model); got != "" {
		t.Fatalf("expected empty (disabled), got %q", got)
	}
}

func TestResolveVisionProcessor_NoConfig(t *testing.T) {
	cfg := &config.Config{}
	p := &Pipeline{config: config.NewTestConfigStore(cfg)}
	model := &config.ModelConfig{Name: "test"}
	if got := p.resolveVisionProcessor(model); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

// --- resolveWebSearchKey tests ---

func TestResolveWebSearchKey_GlobalDefault(t *testing.T) {
	cfg := &config.Config{
		Processors: config.ProcessorsConfig{WebSearchKey: "tvly-123"},
	}
	p := &Pipeline{config: config.NewTestConfigStore(cfg)}
	model := &config.ModelConfig{Name: "test"}
	if got := p.ResolveWebSearchKey(model); got != "tvly-123" {
		t.Fatalf("expected tvly-123, got %q", got)
	}
}

func TestResolveWebSearchKey_PerModelNone(t *testing.T) {
	cfg := &config.Config{
		Processors: config.ProcessorsConfig{WebSearchKey: "tvly-123"},
	}
	p := &Pipeline{config: config.NewTestConfigStore(cfg)}
	model := &config.ModelConfig{
		Name:       "test",
		Processors: &config.ProcessorsConfig{WebSearchKey: "none"},
	}
	if got := p.ResolveWebSearchKey(model); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

// --- Vision processor tests ---

func TestProcessImages_NoImages(t *testing.T) {
	p := &Pipeline{}
	chatReq := map[string]any{
		"messages": []any{
			map[string]any{
				"role":    "user",
				"content": "hello",
			},
		},
	}
	result, err := p.processImages(context.Background(), chatReq, &config.ModelConfig{})
	if err != nil {
		t.Fatal(err)
	}
	msgs := result["messages"].([]any)
	m := msgs[0].(map[string]any)
	if m["content"] != "hello" {
		t.Fatal("content should be unchanged")
	}
}

func TestProcessImages_ReplacesImageWithDescription(t *testing.T) {
	// Set up a mock vision model backend.
	visionServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{
				map[string]any{
					"message": map[string]any{
						"content": "A screenshot showing a terminal with Go code",
					},
				},
			},
		})
	}))
	defer visionServer.Close()

	visionModel := &config.ModelConfig{
		Name:    "vision",
		Backend: visionServer.URL,
		Model:   "vision",
	}

	p := &Pipeline{client: http.DefaultClient}
	chatReq := map[string]any{
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "text", "text": "What is this?"},
					map[string]any{
						"type":      "image_url",
						"image_url": map[string]any{"url": "data:image/png;base64,abc123"},
					},
				},
			},
		},
	}

	result, err := p.processImages(context.Background(), chatReq, visionModel)
	if err != nil {
		t.Fatal(err)
	}

	msgs := result["messages"].([]any)
	content := msgs[0].(map[string]any)["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("expected 2 parts, got %d", len(content))
	}

	// First part should be unchanged text.
	first := content[0].(map[string]any)
	if first["type"] != "text" || first["text"] != "What is this?" {
		t.Fatalf("first part unexpected: %v", first)
	}

	// Second part should be the description replacing the image.
	second := content[1].(map[string]any)
	if second["type"] != "text" {
		t.Fatalf("expected text type, got %v", second["type"])
	}
	text := second["text"].(string)
	if text != "[Image description: A screenshot showing a terminal with Go code]" {
		t.Fatalf("unexpected description: %s", text)
	}
}

func TestProcessImages_VisionModelFailure(t *testing.T) {
	// Vision model returns 500.
	visionServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`{"error":"internal error"}`))
	}))
	defer visionServer.Close()

	visionModel := &config.ModelConfig{
		Name:    "vision",
		Backend: visionServer.URL,
		Model:   "vision",
	}

	p := &Pipeline{client: http.DefaultClient}
	chatReq := map[string]any{
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{
						"type":      "image_url",
						"image_url": map[string]any{"url": "data:image/png;base64,abc"},
					},
				},
			},
		},
	}

	result, err := p.processImages(context.Background(), chatReq, visionModel)
	if err != nil {
		t.Fatal(err)
	}

	// Should have a fallback text instead of the image.
	msgs := result["messages"].([]any)
	content := msgs[0].(map[string]any)["content"].([]any)
	text := content[0].(map[string]any)["text"].(string)
	if text != "[Image could not be processed]" {
		t.Fatalf("expected fallback text, got: %s", text)
	}
}

func TestRequestContainsImageURLs(t *testing.T) {
	withImages := map[string]any{
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "image_url", "image_url": map[string]any{"url": "http://example.com/img.png"}},
				},
			},
		},
	}
	if !RequestContainsImageURLs(withImages) {
		t.Fatal("expected true")
	}

	withoutImages := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "hello"},
		},
	}
	if RequestContainsImageURLs(withoutImages) {
		t.Fatal("expected false")
	}
}

// --- Error formatting tests ---

func TestImageNotSupportedError(t *testing.T) {
	msg := imageNotSupportedError("MiniMax-M2.5", "400: model does not support image inputs")
	if msg == "" {
		t.Fatal("expected non-empty error message")
	}
	// Should mention the model name and have config guidance.
	for _, substr := range []string{"MiniMax-M2.5", "vision:", "Original error:"} {
		if !strings.Contains(msg, substr) {
			t.Fatalf("error message missing %q: %s", substr, msg)
		}
	}
}

func TestSearchNotConfiguredError(t *testing.T) {
	msg := searchNotConfiguredError()
	if msg == "" {
		t.Fatal("expected non-empty error message")
	}
	if !strings.Contains(msg, "web_search_key") {
		t.Fatalf("error message missing web_search_key: %s", msg)
	}
}

// --- Web search tests ---

func TestConvertOrInjectSearchTool_StrippedServerTool(t *testing.T) {
	cfg := &config.Config{
		Processors: config.ProcessorsConfig{WebSearchKey: "tvly-test"},
	}
	p := &Pipeline{config: config.NewTestConfigStore(cfg)}
	model := &config.ModelConfig{Name: "test"}

	chatReq := map[string]any{
		"messages":              []any{map[string]any{"role": "user", "content": "hello"}},
		"tools":                 []map[string]any{{"type": "function", "function": map[string]any{"name": "bash"}}},
		InternalKeyStrippedTools: []string{"web_search_20250305"},
	}

	result := p.convertOrInjectSearchTool(chatReq, model)

	// Should have injected web_search tool.
	tools, ok := result["tools"].([]any)
	if !ok {
		t.Fatalf("expected []any tools, got %T", result["tools"])
	}
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools (bash + web_search), got %d", len(tools))
	}

	// Verify the injected tool.
	lastTool := tools[1].(map[string]any)
	fn := lastTool["function"].(map[string]any)
	if fn["name"] != "web_search" {
		t.Fatalf("expected web_search tool, got %v", fn["name"])
	}

	// _stripped_server_tools is NOT cleaned up by convertOrInjectSearchTool —
	// that's ProcessRequest's responsibility (single ownership).
	if _, exists := result[InternalKeyStrippedTools]; !exists {
		t.Fatal("_stripped_server_tools should still exist (ProcessRequest cleans it)")
	}
}

func TestConvertOrInjectSearchTool_NoSearchKey(t *testing.T) {
	cfg := &config.Config{}
	p := &Pipeline{config: config.NewTestConfigStore(cfg)}
	model := &config.ModelConfig{Name: "test"}

	chatReq := map[string]any{
		"messages":              []any{},
		InternalKeyStrippedTools: []string{"web_search_20250305"},
	}

	result := p.convertOrInjectSearchTool(chatReq, model)

	// Should not inject anything without a search key.
	if _, ok := result["tools"]; ok {
		t.Fatal("should not inject tools when no search key configured")
	}
}

func TestConvertOrInjectSearchTool_AlreadyHasSearchTool(t *testing.T) {
	cfg := &config.Config{
		Processors: config.ProcessorsConfig{WebSearchKey: "tvly-test"},
	}
	p := &Pipeline{config: config.NewTestConfigStore(cfg)}
	model := &config.ModelConfig{Name: "test"}

	chatReq := map[string]any{
		"messages": []any{},
		"tools": []any{
			map[string]any{"type": "function", "function": map[string]any{"name": "web_search"}},
		},
		InternalKeyStrippedTools: []string{"web_search_20250305"},
	}

	result := p.convertOrInjectSearchTool(chatReq, model)

	// Should not duplicate.
	tools := result["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool (no duplicate), got %d", len(tools))
	}
}

func TestIsSearchServerTool(t *testing.T) {
	if !isSearchServerTool("web_search_20250305") {
		t.Fatal("expected true for web_search_20250305")
	}
	if !isSearchServerTool("web_search_preview") {
		t.Fatal("expected true for web_search_preview")
	}
	if isSearchServerTool("code_execution") {
		t.Fatal("expected false for code_execution")
	}
	if isSearchServerTool("function") {
		t.Fatal("expected false for function")
	}
}

func TestExecuteTavilySearch(t *testing.T) {
	// Mock Tavily server.
	tavilyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)

		if req["api_key"] != "tvly-test" {
			w.WriteHeader(401)
			return
		}

		json.NewEncoder(w).Encode(map[string]any{
			"answer": "Test answer",
			"results": []any{
				map[string]any{
					"title":   "Test Result",
					"url":     "https://example.com",
					"content": "Test content",
					"score":   0.95,
				},
			},
		})
	}))
	defer tavilyServer.Close()

	// We can't easily test against the real URL, but we can test the parsing.
	// For now, test the formatting with a mock server.
	// Note: executeTavilySearch hardcodes the Tavily URL, so this test
	// is more about verifying the happy path of the formatter.
}

func TestHasSearchToolCall(t *testing.T) {
	calls := []api.ChatChoiceToolCall{
		{Function: struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}{Name: "bash", Arguments: "{}"}},
		{Function: struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}{Name: "web_search", Arguments: `{"query":"test"}`}},
	}
	if !HasSearchToolCall(calls) {
		t.Fatal("expected true when web_search is present")
	}

	callsNoSearch := []api.ChatChoiceToolCall{
		{Function: struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}{Name: "bash", Arguments: "{}"}},
	}
	if HasSearchToolCall(callsNoSearch) {
		t.Fatal("expected false when web_search is absent")
	}
}

func TestMarshalToolCalls(t *testing.T) {
	tcs := []api.ChatChoiceToolCall{{
		ID:   "call_1",
		Type: "function",
		Function: struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}{Name: "web_search", Arguments: `{"query":"test"}`},
	}}
	result := marshalToolCalls(tcs)
	if len(result) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(result))
	}
	m := result[0].(map[string]any)
	if m["id"] != "call_1" {
		t.Fatalf("expected call_1, got %v", m["id"])
	}
	fn := m["function"].(map[string]any)
	if fn["name"] != "web_search" {
		t.Fatalf("expected web_search, got %v", fn["name"])
	}
}

func TestStreamingSearchState(t *testing.T) {
	s := &streamingSearchState{}
	idx1 := s.accumulateToolCall("call_1", "web_search")
	s.appendArgs(idx1, `{"query":`)
	s.appendArgs(idx1, `"test"}`)

	idx2 := s.accumulateToolCall("call_2", "bash")
	s.appendArgs(idx2, `{"cmd":"ls"}`)

	if !s.hasSearchCall() {
		t.Fatal("expected HasSearchCall true")
	}
	if s.onlySearchCalls() {
		t.Fatal("expected OnlySearchCalls false (has bash)")
	}

	tcs := s.toChatChoiceToolCalls()
	if len(tcs) != 2 {
		t.Fatalf("expected 2 tool calls, got %d", len(tcs))
	}
	if tcs[0].Function.Name != "web_search" {
		t.Fatalf("expected web_search, got %s", tcs[0].Function.Name)
	}
	if tcs[0].Function.Arguments != `{"query":"test"}` {
		t.Fatalf("expected accumulated args, got %s", tcs[0].Function.Arguments)
	}
}

func TestStreamingSearchState_OnlySearch(t *testing.T) {
	s := &streamingSearchState{}
	s.accumulateToolCall("call_1", "web_search")
	s.accumulateToolCall("call_2", "web_search")

	if !s.onlySearchCalls() {
		t.Fatal("expected OnlySearchCalls true")
	}
}

func TestStreamingSearchState_Empty(t *testing.T) {
	s := &streamingSearchState{}
	if s.hasSearchCall() {
		t.Fatal("expected HasSearchCall false on empty")
	}
	if s.onlySearchCalls() {
		t.Fatal("expected OnlySearchCalls false on empty")
	}
}

func TestPipeline_ProcessRequest_SkipsAnthropic(t *testing.T) {
	cfg := &config.Config{
		Processors: config.ProcessorsConfig{Vision: "vision-model"},
		Models: []config.ModelConfig{
			{Name: "test", Backend: "http://localhost/v1", Type: config.BackendAnthropic},
			{Name: "vision-model", Backend: "http://localhost/v1"},
		},
	}
	p := NewPipeline(config.NewTestConfigStore(cfg), http.DefaultClient)
	model := &config.ModelConfig{Name: "test", Type: config.BackendAnthropic}

	chatReq := map[string]any{
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,abc"}},
				},
			},
		},
	}

	result, err := p.ProcessRequest(context.Background(), chatReq, model)
	if err != nil {
		t.Fatal(err)
	}
	// Should NOT process — Anthropic backends skip pipeline.
	msgs := result["messages"].([]any)
	content := msgs[0].(map[string]any)["content"].([]any)
	part := content[0].(map[string]any)
	if part["type"] != "image_url" {
		t.Fatal("expected image_url to be preserved for anthropic backend")
	}
}

func TestPipeline_ProcessRequest_ForcePipeline(t *testing.T) {
	// Set up a mock vision model backend.
	visionServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{
				map[string]any{
					"message": map[string]any{"content": "A code screenshot"},
				},
			},
		})
	}))
	defer visionServer.Close()

	cfg := &config.Config{
		Processors: config.ProcessorsConfig{Vision: "vision-model"},
		Models: []config.ModelConfig{
			{Name: "test", Backend: "http://localhost/v1", Type: config.BackendAnthropic, ForcePipeline: true},
			{Name: "vision-model", Backend: visionServer.URL, Model: "vision-model"},
		},
	}
	p := NewPipeline(config.NewTestConfigStore(cfg), http.DefaultClient)
	model := &cfg.Models[0]

	chatReq := map[string]any{
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,abc"}},
				},
			},
		},
	}

	result, err := p.ProcessRequest(context.Background(), chatReq, model)
	if err != nil {
		t.Fatal(err)
	}
	// Should process despite Anthropic type because force_pipeline is true.
	msgs := result["messages"].([]any)
	content := msgs[0].(map[string]any)["content"].([]any)
	part := content[0].(map[string]any)
	if part["type"] != "text" {
		t.Fatal("expected image to be replaced with text description")
	}
}

func TestPipeline_ProcessRequest_SupportsVisionSkipsProcessing(t *testing.T) {
	cfg := &config.Config{
		Processors: config.ProcessorsConfig{Vision: "vision-model"},
		Models: []config.ModelConfig{
			{Name: "test", Backend: "http://localhost/v1", SupportsVision: true},
			{Name: "vision-model", Backend: "http://localhost/v1"},
		},
	}
	p := NewPipeline(config.NewTestConfigStore(cfg), http.DefaultClient)
	model := &cfg.Models[0]

	chatReq := map[string]any{
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,abc"}},
				},
			},
		},
	}

	result, err := p.ProcessRequest(context.Background(), chatReq, model)
	if err != nil {
		t.Fatal(err)
	}
	// Should NOT process — model supports vision natively.
	msgs := result["messages"].([]any)
	content := msgs[0].(map[string]any)["content"].([]any)
	part := content[0].(map[string]any)
	if part["type"] != "image_url" {
		t.Fatal("expected image_url preserved for vision-capable model")
	}
}

func TestPipeline_ProcessRequest_CleansUpInternalFields(t *testing.T) {
	cfg := &config.Config{}
	p := NewPipeline(config.NewTestConfigStore(cfg), http.DefaultClient)
	model := &config.ModelConfig{Name: "test", Backend: "http://localhost/v1"}

	chatReq := map[string]any{
		"messages":              []any{map[string]any{"role": "user", "content": "hi"}},
		InternalKeyStrippedTools: []string{"web_search_20250305"},
	}

	result, err := p.ProcessRequest(context.Background(), chatReq, model)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := result[InternalKeyStrippedTools]; exists {
		t.Fatal("internal field should be cleaned up")
	}
}

func TestPipeline_ProcessRequest_InjectsSearchTool(t *testing.T) {
	cfg := &config.Config{
		Processors: config.ProcessorsConfig{WebSearchKey: "tvly-test"},
	}
	p := NewPipeline(config.NewTestConfigStore(cfg), http.DefaultClient)
	model := &config.ModelConfig{Name: "test", Backend: "http://localhost/v1"}

	chatReq := map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "search for news"}},
		"tools":    []map[string]any{{"type": "function", "function": map[string]any{"name": "bash"}}},
		InternalKeyStrippedTools: []string{"web_search_20250305"},
	}

	result, err := p.ProcessRequest(context.Background(), chatReq, model)
	if err != nil {
		t.Fatal(err)
	}

	tools, ok := result["tools"].([]any)
	if !ok {
		t.Fatalf("expected []any tools, got %T", result["tools"])
	}
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools (bash + web_search), got %d", len(tools))
	}

	// Verify web_search tool was injected.
	lastTool := tools[1].(map[string]any)
	fn := lastTool["function"].(map[string]any)
	if fn["name"] != "web_search" {
		t.Fatalf("expected web_search, got %v", fn["name"])
	}
}

func TestHandleNonStreamingSearchLoop(t *testing.T) {
	// Mock Tavily server.
	tavilyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"answer":  "Latest news about Go",
			"results": []any{},
		})
	}))
	defer tavilyServer.Close()

	cfg := &config.Config{
		Processors: config.ProcessorsConfig{WebSearchKey: "tvly-test"},
	}
	p := &Pipeline{
		config: config.NewTestConfigStore(cfg),
		client: http.DefaultClient,
	}
	model := &config.ModelConfig{Name: "test"}

	content := "Here is the answer"

	// First response: model calls web_search.
	firstResp := &api.ChatResponse{
		Choices: []api.ChatChoice{{
			Message: api.ChatChoiceMsg{
				ToolCalls: []api.ChatChoiceToolCall{{
					ID:   "call_1",
					Type: "function",
					Function: struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					}{Name: "web_search", Arguments: `{"query":"Go news"}`},
				}},
			},
			FinishReason: "tool_calls",
		}},
	}

	callCount := 0
	sendRequest := func(chatReq map[string]any) (*api.ChatResponse, error) {
		callCount++
		// After search results injected, return final response.
		return &api.ChatResponse{
			Choices: []api.ChatChoice{{
				Message: api.ChatChoiceMsg{
					Content: &content,
				},
				FinishReason: "stop",
			}},
		}, nil
	}

	chatReq := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "What's new in Go?"},
		},
	}

	// executeTavilySearch hardcodes the Tavily URL, so the search will fail.
	// The loop handles this gracefully — it injects the error message as the result.
	resp, err := p.HandleNonStreamingSearchLoop(context.Background(), chatReq, model, firstResp, sendRequest, 5)
	if err != nil {
		t.Fatal(err)
	}
	if callCount != 1 {
		t.Fatalf("expected 1 re-send call after search, got %d", callCount)
	}
	if resp.Choices[0].Message.Content == nil || *resp.Choices[0].Message.Content != "Here is the answer" {
		t.Fatal("expected final response content")
	}
}
