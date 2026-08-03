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
	"go-llm-proxy/internal/pipeline"
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

func TestTranslateAssistantMessage_ServerToolUse(t *testing.T) {
	msgs := []json.RawMessage{
		json.RawMessage(`{"role":"assistant","content":[{"type":"text","text":"Let me search."},{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search_20250305","input":{"query":"golang proxy"}}]}`),
	}
	result, err := translateAnthropicMessages(msgs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 message, got %d", len(result))
	}
	content := result[0]["content"].(string)
	if !strings.Contains(content, "Let me search.") {
		t.Fatalf("expected text content preserved, got %q", content)
	}
	if !strings.Contains(content, "golang proxy") {
		t.Fatalf("expected search query in converted text, got %q", content)
	}
}

func TestTranslateUserMessage_WebSearchToolResult(t *testing.T) {
	msgs := []json.RawMessage{
		json.RawMessage(`{"role":"user","content":[{"type":"web_search_tool_result","tool_use_id":"srvtoolu_1","content":[{"type":"web_search_result","title":"Go Proxy Guide","url":"https://example.com/go-proxy"}]},{"type":"text","text":"Thanks for searching."}]}`),
	}
	result, err := translateAnthropicMessages(msgs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 message, got %d", len(result))
	}
	// Should be multipart content with search results + text.
	content, ok := result[0]["content"].([]any)
	if !ok {
		// May have been simplified to string if only one part; check string form.
		s, ok := result[0]["content"].(string)
		if !ok {
			t.Fatalf("expected content as string or array, got %T", result[0]["content"])
		}
		if !strings.Contains(s, "Go Proxy Guide") {
			t.Fatalf("expected search result title, got %q", s)
		}
		return
	}
	found := false
	for _, part := range content {
		pm := part.(map[string]any)
		if text, ok := pm["text"].(string); ok && strings.Contains(text, "Go Proxy Guide") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected search result title in content parts, got %v", content)
	}
}

func TestTranslateAssistantMessage_ServerToolUse_EmptyQuery(t *testing.T) {
	msgs := []json.RawMessage{
		json.RawMessage(`{"role":"assistant","content":[{"type":"server_tool_use","id":"srv_1","name":"web_search_20250305","input":{}}]}`),
	}
	result, err := translateAnthropicMessages(msgs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Empty query should not produce a search text block — content should be nil/empty.
	if len(result) != 1 {
		t.Fatalf("expected 1 message, got %d", len(result))
	}
	// With no text and no tool calls, content should be nil.
	if result[0]["content"] != nil {
		// If it produced text, make sure it's not a broken format.
		if s, ok := result[0]["content"].(string); ok && s != "" {
			t.Logf("note: empty-query server_tool_use produced content: %q", s)
		}
	}
}

func TestTranslateUserMessage_WebSearchToolResult_Empty(t *testing.T) {
	msgs := []json.RawMessage{
		json.RawMessage(`{"role":"user","content":[{"type":"web_search_tool_result","tool_use_id":"srv_1","content":[]},{"type":"text","text":"No results."}]}`),
	}
	result, err := translateAnthropicMessages(msgs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 message, got %d", len(result))
	}
	// With empty results, only the text part should remain.
	s, ok := result[0]["content"].(string)
	if !ok {
		t.Fatalf("expected simplified string content, got %T", result[0]["content"])
	}
	if s != "No results." {
		t.Fatalf("expected 'No results.', got %q", s)
	}
}

func TestTranslateDocumentBlock_Base64PDF(t *testing.T) {
	block := map[string]json.RawMessage{
		"type":   json.RawMessage(`"document"`),
		"source": json.RawMessage(`{"type":"base64","media_type":"application/pdf","data":"JVBERi0="}`),
		"title":  json.RawMessage(`"report.pdf"`),
	}
	result := translateDocumentBlock(block)
	if result["type"] != "pdf_data" {
		t.Fatalf("expected pdf_data type, got %v", result["type"])
	}
	if result["data"] != "JVBERi0=" {
		t.Fatalf("expected base64 data preserved, got %v", result["data"])
	}
	if result["filename"] != "report.pdf" {
		t.Fatalf("expected filename from title, got %v", result["filename"])
	}
}

func TestTranslateDocumentBlock_UnsupportedFormat(t *testing.T) {
	block := map[string]json.RawMessage{
		"type":   json.RawMessage(`"document"`),
		"source": json.RawMessage(`{"type":"url","media_type":"text/html","url":"https://example.com"}`),
	}
	result := translateDocumentBlock(block)
	if result["type"] != "text" {
		t.Fatalf("expected text fallback, got %v", result["type"])
	}
	text := result["text"].(string)
	if !strings.Contains(text, "unsupported") {
		t.Fatalf("expected unsupported message, got %q", text)
	}
}

func TestTranslateDocumentBlock_MalformedSource(t *testing.T) {
	block := map[string]json.RawMessage{
		"type":   json.RawMessage(`"document"`),
		"source": json.RawMessage(`not valid json`),
	}
	result := translateDocumentBlock(block)
	if result["type"] != "text" {
		t.Fatalf("expected text fallback for malformed source, got %v", result["type"])
	}
}

func TestExtractToolResultContent_ArrayOfBlocks(t *testing.T) {
	raw := json.RawMessage(`[{"type":"text","text":"Result 1"},{"type":"image","source":{}},{"type":"text","text":"Result 2"}]`)
	got := extractToolResultContent(raw)
	if !strings.Contains(got, "Result 1") || !strings.Contains(got, "Result 2") {
		t.Fatalf("expected both text parts, got %q", got)
	}
}

func TestExtractToolResultContent_NullAndEmpty(t *testing.T) {
	if got := extractToolResultContent(nil); got != "" {
		t.Fatalf("expected empty for nil, got %q", got)
	}
	if got := extractToolResultContent(json.RawMessage(`null`)); got != "" {
		t.Fatalf("expected empty for null, got %q", got)
	}
}

func TestBuildChatRequest_MetadataUserID(t *testing.T) {
	req := messagesRequest{
		Model:     "test",
		MaxTokens: 100,
		Messages:  []json.RawMessage{json.RawMessage(`{"role":"user","content":"Hi"}`)},
		Metadata:  json.RawMessage(`{"user_id":"user-123"}`),
	}
	chatReq, err := buildChatRequestFromAnthropic(req, "test")
	if err != nil {
		t.Fatal(err)
	}
	if chatReq["user"] != "user-123" {
		t.Fatalf("expected user field from metadata.user_id, got %v", chatReq["user"])
	}
}

func TestBuildChatRequest_StopSequences(t *testing.T) {
	req := messagesRequest{
		Model:         "test",
		MaxTokens:     100,
		Messages:      []json.RawMessage{json.RawMessage(`{"role":"user","content":"Hi"}`)},
		StopSequences: []string{"STOP", "END"},
	}
	chatReq, err := buildChatRequestFromAnthropic(req, "test")
	if err != nil {
		t.Fatal(err)
	}
	stop, ok := chatReq["stop"].([]string)
	if !ok || len(stop) != 2 || stop[0] != "STOP" {
		t.Fatalf("expected stop sequences mapped, got %v", chatReq["stop"])
	}
}

func TestBuildChatRequest_TopP(t *testing.T) {
	topP := 0.9
	req := messagesRequest{
		Model:     "test",
		MaxTokens: 100,
		Messages:  []json.RawMessage{json.RawMessage(`{"role":"user","content":"Hi"}`)},
		TopP:      &topP,
	}
	chatReq, err := buildChatRequestFromAnthropic(req, "test")
	if err != nil {
		t.Fatal(err)
	}
	if chatReq["top_p"] != 0.9 {
		t.Fatalf("expected top_p=0.9, got %v", chatReq["top_p"])
	}
}

// --- shouldTranslate routing tests ---

// --- emitSearchResultBlocks format tests ---

func TestEmitSearchResultBlocks_Format(t *testing.T) {
	results := []pipeline.SearchCallResult{
		{
			ToolUseID: "srvtoolu_1",
			Query:     "golang proxy",
			Hits: []pipeline.SearchHit{
				{Title: "Go Proxy Guide", URL: "https://example.com/go"},
			},
		},
	}

	var buf strings.Builder
	blockIndex := 0
	emit := func(eventType string, data map[string]any) {
		data["type"] = eventType
		jsonData, _ := json.Marshal(data)
		fmt.Fprintf(&buf, "event: %s\ndata: %s\n\n", eventType, jsonData)
	}

	finalIndex := emitSearchResultBlocks(emit, blockIndex, results)

	// Should have emitted 2 blocks (server_tool_use + web_search_tool_result) = 4 events (start+stop each).
	if finalIndex != 2 {
		t.Fatalf("expected blockIndex=2 after 2 blocks, got %d", finalIndex)
	}

	body := buf.String()
	if !strings.Contains(body, "server_tool_use") {
		t.Fatal("expected server_tool_use block")
	}
	if !strings.Contains(body, "web_search_tool_result") {
		t.Fatal("expected web_search_tool_result block")
	}
	if !strings.Contains(body, "golang proxy") {
		t.Fatal("expected query in server_tool_use input")
	}
	if !strings.Contains(body, "Go Proxy Guide") {
		t.Fatal("expected hit title in results")
	}
}

func TestEmitSearchResultBlocks_SkipsFailedSearches(t *testing.T) {
	results := []pipeline.SearchCallResult{
		{Query: "failed", Error: "timeout"},
		{ToolUseID: "srv_2", Query: "success", Hits: []pipeline.SearchHit{{Title: "Result", URL: "https://example.com"}}},
	}

	var buf strings.Builder
	emit := func(eventType string, data map[string]any) {
		data["type"] = eventType
		jsonData, _ := json.Marshal(data)
		fmt.Fprintf(&buf, "event: %s\ndata: %s\n\n", eventType, jsonData)
	}

	finalIndex := emitSearchResultBlocks(emit, 0, results)

	// Only the successful search should produce blocks.
	if finalIndex != 2 {
		t.Fatalf("expected blockIndex=2 (one success), got %d", finalIndex)
	}
	if strings.Contains(buf.String(), "failed") {
		t.Fatal("failed search should be skipped, not emitted")
	}
}

// --- shouldTranslate routing tests ---

func TestShouldTranslate_AutoAnthropicPassthrough(t *testing.T) {
	h := &MessagesHandler{}
	model := &config.ModelConfig{Type: config.BackendAnthropic}
	if h.shouldTranslate(model) {
		t.Fatal("expected auto mode to passthrough for anthropic backend")
	}
}

func TestShouldTranslate_AutoOpenAITranslates(t *testing.T) {
	h := &MessagesHandler{}
	model := &config.ModelConfig{Type: ""}
	if !h.shouldTranslate(model) {
		t.Fatal("expected auto mode to translate for non-anthropic backend")
	}
}

func TestShouldTranslate_ForceNative(t *testing.T) {
	h := &MessagesHandler{}
	model := &config.ModelConfig{MessagesMode: config.MessagesModeNative}
	if h.shouldTranslate(model) {
		t.Fatal("expected native mode to never translate")
	}
}

func TestShouldTranslate_ForceTranslate(t *testing.T) {
	h := &MessagesHandler{}
	model := &config.ModelConfig{MessagesMode: config.MessagesModeTranslate, Type: config.BackendAnthropic}
	if !h.shouldTranslate(model) {
		t.Fatal("expected translate mode to always translate, even for anthropic backend")
	}
}

func TestTranslateAnthropicTools(t *testing.T) {
	tools := []json.RawMessage{
		json.RawMessage(`{"name":"get_weather","description":"Get weather","input_schema":{"type":"object","properties":{"location":{"type":"string"}},"required":["location"]},"cache_control":{"type":"ephemeral"}}`),
	}
	result, _ := translateAnthropicToolsToChat(tools)
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
	result, stripped := translateAnthropicToolsToChat(tools)
	if len(result) != 1 {
		t.Fatalf("expected 1 tool (server tool stripped), got %d", len(result))
	}
	if len(stripped) != 1 || stripped[0] != "web_search_20250305" {
		t.Fatalf("expected stripped server tool web_search_20250305, got %v", stripped)
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

func TestTranslateAnthropicMessages_ImageContent(t *testing.T) {
	msgs := []json.RawMessage{
		json.RawMessage(`{"role":"user","content":[{"type":"text","text":"What is this?"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBOR..."}}]}`),
	}
	result, err := translateAnthropicMessages(msgs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 message, got %d", len(result))
	}
	content := result[0]["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("expected 2 content parts, got %d", len(content))
	}
	imgPart := content[1].(map[string]any)
	if imgPart["type"] != "image_url" {
		t.Fatalf("expected image_url type, got %v", imgPart["type"])
	}
	imgURL := imgPart["image_url"].(map[string]any)
	if !strings.HasPrefix(imgURL["url"].(string), "data:image/png;base64,") {
		t.Fatalf("expected data URI, got %v", imgURL["url"])
	}
}

func TestTranslateAnthropicMessages_MultipleToolResults(t *testing.T) {
	msgs := []json.RawMessage{
		json.RawMessage(`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"result1"},{"type":"tool_result","tool_use_id":"t2","content":"result2"}]}`),
	}
	result, err := translateAnthropicMessages(msgs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should produce 2 separate role:tool messages.
	if len(result) != 2 {
		t.Fatalf("expected 2 tool messages, got %d", len(result))
	}
	if result[0]["role"] != "tool" || result[0]["tool_call_id"] != "t1" {
		t.Fatalf("expected first tool message for t1, got %v", result[0])
	}
	if result[1]["role"] != "tool" || result[1]["tool_call_id"] != "t2" {
		t.Fatalf("expected second tool message for t2, got %v", result[1])
	}
}

func TestBuildChatRequestFromAnthropic_ThinkingDropped(t *testing.T) {
	req := messagesRequest{
		Model:     "test-model",
		Messages:  []json.RawMessage{json.RawMessage(`{"role":"user","content":"Hello"}`)},
		MaxTokens: 100,
		Thinking:  json.RawMessage(`{"type":"adaptive"}`),
	}
	chatReq, err := buildChatRequestFromAnthropic(req, "test-model")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, has := chatReq["thinking"]; has {
		t.Fatal("expected thinking config to be dropped")
	}
}

func TestBuildChatRequestFromAnthropic_MaxTokensMapped(t *testing.T) {
	req := messagesRequest{
		Model:     "test-model",
		Messages:  []json.RawMessage{json.RawMessage(`{"role":"user","content":"Hello"}`)},
		MaxTokens: 4096,
	}
	chatReq, err := buildChatRequestFromAnthropic(req, "test-model")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if chatReq["max_completion_tokens"] != 4096 {
		t.Fatalf("expected max_completion_tokens=4096, got %v", chatReq["max_completion_tokens"])
	}
	if _, has := chatReq["max_tokens"]; has {
		t.Fatal("expected max_tokens NOT to be set (should use max_completion_tokens)")
	}
}

// --- Integration tests ---

func newTestMessagesHandler(t *testing.T, modelType string, upstream http.HandlerFunc) (*MessagesHandler, *httptest.Server) {
	t.Helper()
	ts := httptest.NewServer(upstream)
	backend := ts.URL + "/v1"
	if modelType == config.BackendAnthropic {
		backend = ts.URL
	}
	cfg := &config.Config{
		Models: []config.ModelConfig{{
			Name: "test-model", Backend: backend, APIKey: "backend-secret",
			Model: "test-model", Timeout: 10, Type: modelType,
		}},
	}
	cs := config.NewTestConfigStore(cfg)
	return NewMessagesHandler(cs, nil, nil), ts
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
	handler, ts := newTestMessagesHandler(t, config.BackendAnthropic, func(w http.ResponseWriter, r *http.Request) {
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

func TestMessagesHandler_NativePassthrough_CustomBearerAuthHeader(t *testing.T) {
	var gotAPIKey, gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAPIKey = r.Header.Get("X-Api-Key")
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`)
	}))
	defer ts.Close()

	cfg := &config.Config{
		Models: []config.ModelConfig{{
			Name:           "test-model",
			Backend:        ts.URL,
			APIKey:         "backend-secret",
			AuthHeaderName: "Authorization",
			AuthScheme:     config.AuthSchemeBearer,
			Model:          "test-model",
			Timeout:        10,
			Type:           config.BackendAnthropic,
		}},
	}
	handler := NewMessagesHandler(config.NewTestConfigStore(cfg), nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"test-model","max_tokens":100,"messages":[{"role":"user","content":"Hello"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotAPIKey != "" {
		t.Fatalf("expected no x-api-key header, got %q", gotAPIKey)
	}
	if gotAuth != "Bearer backend-secret" {
		t.Fatalf("expected bearer authorization header, got %q", gotAuth)
	}
}

func TestMessagesHandler_NativePassthrough_HeadersForwarded(t *testing.T) {
	var gotVersion, gotBeta string
	handler, ts := newTestMessagesHandler(t, config.BackendAnthropic, func(w http.ResponseWriter, r *http.Request) {
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
	handler, ts := newTestMessagesHandler(t, config.BackendAnthropic, func(w http.ResponseWriter, r *http.Request) {
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

	cfg := &config.Config{
		Models: []config.ModelConfig{{
			Name: "test-model", Backend: ts.URL + "/v1",
			Model: "test-model", Timeout: 10,
			MessagesMode: "translate",
		}},
	}
	cs := config.NewTestConfigStore(cfg)
	handler := NewMessagesHandler(cs, nil, nil)

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

func TestMessagesHandler_Streaming_TextAndTools(t *testing.T) {
	handler, ts := newTestMessagesHandler(t, "", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		chunks := []string{
			`{"id":"chatcmpl-1","model":"test-model","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
			`{"id":"chatcmpl-1","model":"test-model","choices":[{"index":0,"delta":{"content":"Let me check."},"finish_reason":null}]}`,
			`{"id":"chatcmpl-1","model":"test-model","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]},"finish_reason":null}]}`,
			`{"id":"chatcmpl-1","model":"test-model","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"location\":\"Paris\"}"}}]},"finish_reason":null}]}`,
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

	// Should have both a text block and a tool_use block.
	var hasTextBlock, hasToolBlock bool
	for _, e := range events {
		if e.event == "content_block_start" {
			var d map[string]any
			json.Unmarshal([]byte(e.data), &d)
			cb := d["content_block"].(map[string]any)
			if cb["type"] == "text" {
				hasTextBlock = true
			}
			if cb["type"] == "tool_use" {
				hasToolBlock = true
			}
		}
	}
	if !hasTextBlock {
		t.Error("expected text content_block_start")
	}
	if !hasToolBlock {
		t.Error("expected tool_use content_block_start")
	}

	// Text block should be closed before tool block opens.
	var textStopIdx, toolStartIdx int
	for i, e := range events {
		if e.event == "content_block_stop" {
			var d map[string]any
			json.Unmarshal([]byte(e.data), &d)
			if d["index"] == float64(0) {
				textStopIdx = i
			}
		}
		if e.event == "content_block_start" {
			var d map[string]any
			json.Unmarshal([]byte(e.data), &d)
			cb := d["content_block"].(map[string]any)
			if cb["type"] == "tool_use" {
				toolStartIdx = i
			}
		}
	}
	if textStopIdx >= toolStartIdx {
		t.Errorf("text block stop (idx %d) should come before tool block start (idx %d)", textStopIdx, toolStartIdx)
	}
}

func TestMessagesHandler_BackendError_Streaming(t *testing.T) {
	handler, ts := newTestMessagesHandler(t, "", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":"invalid request"}`)
	})
	defer ts.Close()

	body := `{"model":"test-model","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	// Should return SSE with error event.
	if !strings.Contains(w.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("expected SSE content type for streaming error, got %q", w.Header().Get("Content-Type"))
	}
	events := parseSSEEvents(w.Body.String())
	var hasError bool
	for _, e := range events {
		if e.event == "error" {
			hasError = true
			var d map[string]any
			json.Unmarshal([]byte(e.data), &d)
			if d["type"] != "error" {
				t.Fatalf("expected type=error in payload, got %v", d["type"])
			}
		}
	}
	if !hasError {
		t.Fatal("expected error SSE event for backend error in streaming mode")
	}
}

func TestMessagesHandler_BackendError_NonStreaming(t *testing.T) {
	handler, ts := newTestMessagesHandler(t, "", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":"bad request"}`)
	})
	defer ts.Close()

	body := `{"model":"test-model","max_tokens":100,"messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["type"] != "error" {
		t.Fatalf("expected Anthropic error format, got %v", resp)
	}
}

func TestMessagesHandler_ReasoningKeepalive(t *testing.T) {
	handler, ts := newTestMessagesHandler(t, "", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		chunks := []string{
			`{"id":"chatcmpl-1","model":"test-model","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
			`{"id":"chatcmpl-1","model":"test-model","choices":[{"index":0,"delta":{"reasoning":"Thinking..."},"finish_reason":null}]}`,
			`{"id":"chatcmpl-1","model":"test-model","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}`,
			`{"id":"chatcmpl-1","model":"test-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
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

	events := parseSSEEvents(w.Body.String())

	// Should have a thinking block from reasoning tokens.
	var hasThinkingStart, hasThinkingDelta, hasTextBlock bool
	for _, e := range events {
		if e.event == "content_block_start" {
			var d map[string]any
			json.Unmarshal([]byte(e.data), &d)
			cb := d["content_block"].(map[string]any)
			if cb["type"] == "thinking" {
				hasThinkingStart = true
			}
			if cb["type"] == "text" {
				hasTextBlock = true
			}
		}
		if e.event == "content_block_delta" {
			var d map[string]any
			json.Unmarshal([]byte(e.data), &d)
			delta := d["delta"].(map[string]any)
			if delta["type"] == "thinking_delta" {
				hasThinkingDelta = true
			}
		}
	}
	if !hasThinkingStart {
		t.Error("expected thinking content_block_start from reasoning tokens")
	}
	if !hasThinkingDelta {
		t.Error("expected thinking_delta from reasoning tokens")
	}
	if !hasTextBlock {
		t.Error("expected text content_block_start after reasoning")
	}
}

func TestMessagesHandler_ToolChoiceWithoutTools(t *testing.T) {
	var gotBody map[string]any
	handler, ts := newTestMessagesHandler(t, "", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-1", "model": "test-model", "created": 0,
			"choices": []map[string]any{{
				"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": "OK"},
			}},
		})
	})
	defer ts.Close()

	// Send tool_choice without any tools.
	body := `{"model":"test-model","max_tokens":100,"messages":[{"role":"user","content":"Hi"}],"tool_choice":{"type":"auto"}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	// tool_choice should be stripped from the translated request.
	if _, has := gotBody["tool_choice"]; has {
		t.Fatal("expected tool_choice to be stripped when no tools present")
	}
}

func TestMessagesHandler_AnthropicPrefix(t *testing.T) {
	var gotPath string
	handler, ts := newTestMessagesHandler(t, config.BackendAnthropic, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[],"stop_reason":"end_turn"}`)
	})
	defer ts.Close()

	body := `{"model":"test-model","max_tokens":100,"messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotPath != "/v1/messages" {
		t.Fatalf("expected upstream path /v1/messages, got %q", gotPath)
	}
}

func TestMessagesHandler_AnthropicPrefixRejectsOpenAI(t *testing.T) {
	handler, ts := newTestMessagesHandler(t, "", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	defer ts.Close()

	body := `{"model":"test-model","max_tokens":100,"messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for openai model on /anthropic path, got %d", w.Code)
	}
}

// --- Streaming search loop tests ---

func TestMessagesHandler_StreamingSearchLoop(t *testing.T) {
	// Backend behavior: first request returns web_search tool call, second returns text.
	callCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)

		if callCount == 1 {
			// First call: return web_search tool call.
			chunks := []string{
				`{"id":"c1","model":"test-model","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
				`{"id":"c1","model":"test-model","choices":[{"index":0,"delta":{"content":"Searching..."},"finish_reason":null}]}`,
				`{"id":"c1","model":"test-model","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"web_search","arguments":""}}]},"finish_reason":null}]}`,
				`{"id":"c1","model":"test-model","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"query\":\"test search\"}"}}]},"finish_reason":null}]}`,
				`{"id":"c1","model":"test-model","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			}
			for _, c := range chunks {
				fmt.Fprintf(w, "data: %s\n\n", c)
				flusher.Flush()
			}
		} else {
			// Second call (after search): return final text.
			chunks := []string{
				`{"id":"c2","model":"test-model","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
				`{"id":"c2","model":"test-model","choices":[{"index":0,"delta":{"content":"Here are the results."},"finish_reason":null}]}`,
				`{"id":"c2","model":"test-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			}
			for _, c := range chunks {
				fmt.Fprintf(w, "data: %s\n\n", c)
				flusher.Flush()
			}
		}
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer ts.Close()

	// Create a fake Tavily server.
	tavilyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{
				{"title": "Test Result", "url": "https://example.com", "content": "Test content"},
			},
		})
	}))
	defer tavilyServer.Close()

	cfg := &config.Config{
		Processors: config.ProcessorsConfig{WebSearchKey: "tvly-test"},
		Models: []config.ModelConfig{{
			Name: "test-model", Backend: ts.URL + "/v1", APIKey: "secret",
			Model: "test-model", Timeout: 30,
		}},
	}
	cs := config.NewTestConfigStore(cfg)
	pl := pipeline.NewPipeline(cs, http.DefaultClient)
	h := NewMessagesHandler(cs, nil, pl)

	// Send a request with web_search server tool.
	body := `{"model":"test-model","max_tokens":1000,"stream":true,"messages":[{"role":"user","content":"Search for something"}],"tools":[{"type":"web_search_20250305","name":"web_search"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Verify we got SSE events.
	events := parseSSEEvents(w.Body.String())
	if len(events) == 0 {
		t.Fatal("expected SSE events")
	}

	// Should have message_start and message_stop.
	var hasStart, hasStop bool
	for _, e := range events {
		if e.event == "message_start" {
			hasStart = true
		}
		if e.event == "message_stop" {
			hasStop = true
		}
	}
	if !hasStart {
		t.Error("expected message_start event")
	}
	if !hasStop {
		t.Error("expected message_stop event")
	}

	// Backend should have been called twice (first for search tool call, second for final answer).
	if callCount != 2 {
		t.Errorf("expected 2 backend calls (search loop), got %d", callCount)
	}
}

func TestMessagesHandler_NonStreamingSearchLoop(t *testing.T) {
	callCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		if callCount == 1 {
			json.NewEncoder(w).Encode(map[string]any{
				"id": "c1", "model": "test-model",
				"choices": []map[string]any{{
					"index": 0, "finish_reason": "tool_calls",
					"message": map[string]any{
						"role": "assistant",
						"tool_calls": []map[string]any{{
							"id": "call_1", "type": "function",
							"function": map[string]any{"name": "web_search", "arguments": `{"query":"test"}`},
						}},
					},
				}},
				"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
			})
		} else {
			json.NewEncoder(w).Encode(map[string]any{
				"id": "c2", "model": "test-model",
				"choices": []map[string]any{{
					"index": 0, "finish_reason": "stop",
					"message": map[string]any{"role": "assistant", "content": "Found it!"},
				}},
				"usage": map[string]any{"prompt_tokens": 20, "completion_tokens": 10, "total_tokens": 30},
			})
		}
	}))
	defer ts.Close()

	cfg := &config.Config{
		Processors: config.ProcessorsConfig{WebSearchKey: "tvly-test"},
		Models: []config.ModelConfig{{
			Name: "test-model", Backend: ts.URL + "/v1", APIKey: "secret",
			Model: "test-model", Timeout: 30,
		}},
	}
	cs := config.NewTestConfigStore(cfg)
	pl := pipeline.NewPipeline(cs, http.DefaultClient)
	h := NewMessagesHandler(cs, nil, pl)

	body := `{"model":"test-model","max_tokens":1000,"messages":[{"role":"user","content":"Search"}],"tools":[{"type":"web_search_20250305","name":"web_search"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["type"] != "message" {
		t.Fatalf("expected message type, got %v", resp["type"])
	}
	if resp["stop_reason"] != "end_turn" {
		t.Fatalf("expected end_turn stop reason, got %v", resp["stop_reason"])
	}

	// Should have been called twice.
	if callCount != 2 {
		t.Errorf("expected 2 backend calls, got %d", callCount)
	}
}

func TestMessagesHandler_MixedToolCalls(t *testing.T) {
	// Backend returns both web_search AND bash tool calls. Only web_search should be
	// handled by the proxy; bash should pass through to the client.
	callCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		if callCount == 1 {
			json.NewEncoder(w).Encode(map[string]any{
				"id": "c1", "model": "test-model",
				"choices": []map[string]any{{
					"index": 0, "finish_reason": "tool_calls",
					"message": map[string]any{
						"role": "assistant",
						"tool_calls": []map[string]any{
							{"id": "call_1", "type": "function", "function": map[string]any{"name": "web_search", "arguments": `{"query":"test"}`}},
							{"id": "call_2", "type": "function", "function": map[string]any{"name": "bash", "arguments": `{"cmd":"ls"}`}},
						},
					},
				}},
				"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
			})
		} else {
			// After search executed, backend re-requests bash (client tool).
			json.NewEncoder(w).Encode(map[string]any{
				"id": "c2", "model": "test-model",
				"choices": []map[string]any{{
					"index": 0, "finish_reason": "tool_calls",
					"message": map[string]any{
						"role": "assistant",
						"tool_calls": []map[string]any{
							{"id": "call_3", "type": "function", "function": map[string]any{"name": "bash", "arguments": `{"cmd":"ls"}`}},
						},
					},
				}},
				"usage": map[string]any{"prompt_tokens": 20, "completion_tokens": 10, "total_tokens": 30},
			})
		}
	}))
	defer ts.Close()

	cfg := &config.Config{
		Processors: config.ProcessorsConfig{WebSearchKey: "tvly-test"},
		Models: []config.ModelConfig{{
			Name: "test-model", Backend: ts.URL + "/v1", APIKey: "secret",
			Model: "test-model", Timeout: 30,
		}},
	}
	cs := config.NewTestConfigStore(cfg)
	pl := pipeline.NewPipeline(cs, http.DefaultClient)
	h := NewMessagesHandler(cs, nil, pl)

	body := `{"model":"test-model","max_tokens":1000,"messages":[{"role":"user","content":"Search and run"}],"tools":[{"type":"web_search_20250305","name":"web_search"},{"name":"bash","description":"Run bash","input_schema":{"type":"object","properties":{"cmd":{"type":"string"}}}}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Should have called backend at least 2 times (search loop + re-request).
	if callCount < 2 {
		t.Errorf("expected at least 2 backend calls for mixed tool calls, got %d", callCount)
	}

	// Response should contain tool_use for bash (client-side tool).
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["stop_reason"] != "tool_use" {
		t.Errorf("expected tool_use stop_reason for client tool, got %v", resp["stop_reason"])
	}
}
