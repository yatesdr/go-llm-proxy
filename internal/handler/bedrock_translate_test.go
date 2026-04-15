package handler

import (
	"encoding/json"
	"testing"
)

// --- Request translation: Anthropic -> Converse ---

func TestBuildConverseRequest_BasicExchange(t *testing.T) {
	req := messagesRequest{
		Model:     "claude-bedrock",
		MaxTokens: 1024,
		System:    json.RawMessage(`"You are helpful."`),
		Messages: []json.RawMessage{
			json.RawMessage(`{"role":"user","content":"Hello"}`),
			json.RawMessage(`{"role":"assistant","content":"Hi there!"}`),
		},
	}
	got, err := buildConverseRequestFromAnthropic(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	sys, ok := got["system"].([]map[string]any)
	if !ok || len(sys) != 1 || sys[0]["text"] != "You are helpful." {
		t.Errorf("system mismatch: %#v", got["system"])
	}

	inf, ok := got["inferenceConfig"].(map[string]any)
	if !ok || inf["maxTokens"] != 1024 {
		t.Errorf("inferenceConfig mismatch: %#v", got["inferenceConfig"])
	}

	msgs, ok := got["messages"].([]map[string]any)
	if !ok || len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %#v", got["messages"])
	}
	if msgs[0]["role"] != "user" {
		t.Errorf("first message role: %v", msgs[0]["role"])
	}
	userContent, _ := msgs[0]["content"].([]map[string]any)
	if len(userContent) != 1 || userContent[0]["text"] != "Hello" {
		t.Errorf("user content mismatch: %#v", msgs[0]["content"])
	}
}

func TestBuildConverseRequest_ToolUseAndResult(t *testing.T) {
	req := messagesRequest{
		MaxTokens: 1024,
		Messages: []json.RawMessage{
			json.RawMessage(`{"role":"user","content":"What's the weather in Paris?"}`),
			json.RawMessage(`{"role":"assistant","content":[{"type":"text","text":"Checking..."},{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"location":"Paris"}}]}`),
			json.RawMessage(`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"22C sunny"}]}`),
		},
	}
	got, err := buildConverseRequestFromAnthropic(req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	msgs := got["messages"].([]map[string]any)
	if len(msgs) != 3 {
		t.Fatalf("want 3 messages, got %d", len(msgs))
	}

	// Assistant turn must contain text + toolUse blocks.
	asst := msgs[1]["content"].([]map[string]any)
	if len(asst) != 2 {
		t.Fatalf("assistant content blocks: want 2, got %d: %#v", len(asst), asst)
	}
	if asst[0]["text"] != "Checking..." {
		t.Errorf("text block wrong: %#v", asst[0])
	}
	tu, ok := asst[1]["toolUse"].(map[string]any)
	if !ok {
		t.Fatalf("expected toolUse block: %#v", asst[1])
	}
	if tu["toolUseId"] != "toolu_1" || tu["name"] != "get_weather" {
		t.Errorf("toolUse fields: %#v", tu)
	}

	// Tool result must come back as toolResult on the user turn.
	usr := msgs[2]["content"].([]map[string]any)
	if len(usr) != 1 {
		t.Fatalf("user toolResult content: want 1 block, got %#v", usr)
	}
	tr, ok := usr[0]["toolResult"].(map[string]any)
	if !ok {
		t.Fatalf("expected toolResult: %#v", usr[0])
	}
	if tr["toolUseId"] != "toolu_1" {
		t.Errorf("toolUseId mismatch: %#v", tr)
	}
	trContent, _ := tr["content"].([]map[string]any)
	if len(trContent) != 1 || trContent[0]["text"] != "22C sunny" {
		t.Errorf("toolResult content: %#v", trContent)
	}
}

func TestBuildConverseRequest_ImageBlock(t *testing.T) {
	req := messagesRequest{
		Messages: []json.RawMessage{
			json.RawMessage(`{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KG=="}},{"type":"text","text":"What is this?"}]}`),
		},
	}
	got, err := buildConverseRequestFromAnthropic(req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	msgs := got["messages"].([]map[string]any)
	content := msgs[0]["content"].([]map[string]any)
	if len(content) != 2 {
		t.Fatalf("want 2 blocks, got %d: %#v", len(content), content)
	}
	img, ok := content[0]["image"].(map[string]any)
	if !ok {
		t.Fatalf("expected image block: %#v", content[0])
	}
	if img["format"] != "png" {
		t.Errorf("format: want png, got %v", img["format"])
	}
	src := img["source"].(map[string]any)
	if src["bytes"] != "iVBORw0KG==" {
		t.Errorf("bytes: %v", src["bytes"])
	}
}

func TestBuildConverseRequest_StripsThinking(t *testing.T) {
	req := messagesRequest{
		Messages: []json.RawMessage{
			json.RawMessage(`{"role":"assistant","content":[{"type":"thinking","thinking":"Let me think...","signature":"abc"},{"type":"text","text":"The answer is 42."}]}`),
		},
	}
	got, err := buildConverseRequestFromAnthropic(req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	msgs := got["messages"].([]map[string]any)
	content := msgs[0]["content"].([]map[string]any)
	if len(content) != 1 {
		t.Fatalf("expected thinking stripped, want 1 block got %d: %#v", len(content), content)
	}
	if content[0]["text"] != "The answer is 42." {
		t.Errorf("wrong remaining block: %#v", content[0])
	}
}

func TestBuildConverseRequest_Tools(t *testing.T) {
	req := messagesRequest{
		Tools: []json.RawMessage{
			json.RawMessage(`{"name":"get_weather","description":"Get weather","input_schema":{"type":"object","properties":{"location":{"type":"string"}}}}`),
		},
		ToolChoice: json.RawMessage(`{"type":"any"}`),
		Messages: []json.RawMessage{
			json.RawMessage(`{"role":"user","content":"hi"}`),
		},
	}
	got, err := buildConverseRequestFromAnthropic(req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	cfg, ok := got["toolConfig"].(map[string]any)
	if !ok {
		t.Fatalf("missing toolConfig: %#v", got)
	}
	tools := cfg["tools"].([]map[string]any)
	if len(tools) != 1 {
		t.Fatalf("want 1 tool, got %d", len(tools))
	}
	spec := tools[0]["toolSpec"].(map[string]any)
	if spec["name"] != "get_weather" {
		t.Errorf("tool name: %v", spec["name"])
	}
	schema, ok := spec["inputSchema"].(map[string]any)
	if !ok || schema["json"] == nil {
		t.Errorf("inputSchema wrapping wrong: %#v", spec["inputSchema"])
	}
	tc, ok := cfg["toolChoice"].(map[string]any)
	if !ok || tc["any"] == nil {
		t.Errorf("toolChoice: %#v", cfg["toolChoice"])
	}
}

func TestBuildConverseRequest_ToolsServerToolsStripped(t *testing.T) {
	req := messagesRequest{
		Tools: []json.RawMessage{
			json.RawMessage(`{"name":"web_search","type":"web_search_20250305"}`),
			json.RawMessage(`{"name":"get_weather","input_schema":{"type":"object"}}`),
		},
		Messages: []json.RawMessage{json.RawMessage(`{"role":"user","content":"hi"}`)},
	}
	got, _ := buildConverseRequestFromAnthropic(req)
	cfg := got["toolConfig"].(map[string]any)
	tools := cfg["tools"].([]map[string]any)
	if len(tools) != 1 {
		t.Fatalf("server tool should be stripped, got %d tools", len(tools))
	}
}

func TestBuildConverseRequest_ThinkingPassthrough(t *testing.T) {
	req := messagesRequest{
		Messages: []json.RawMessage{json.RawMessage(`{"role":"user","content":"hi"}`)},
		Thinking: json.RawMessage(`{"type":"enabled","budget_tokens":10000}`),
	}
	got, _ := buildConverseRequestFromAnthropic(req)
	addl, ok := got["additionalModelRequestFields"].(map[string]any)
	if !ok {
		t.Fatalf("missing additionalModelRequestFields: %#v", got)
	}
	if addl["thinking"] == nil {
		t.Errorf("thinking not forwarded: %#v", addl)
	}
}

// --- Response translation: Converse -> Anthropic ---

func TestBuildAnthropicResponseFromConverse_Text(t *testing.T) {
	body := []byte(`{
		"output":{"message":{"role":"assistant","content":[{"text":"Hello!"}]}},
		"stopReason":"end_turn",
		"usage":{"inputTokens":10,"outputTokens":5,"totalTokens":15}
	}`)
	got, usage, err := buildAnthropicResponseFromConverse(body, "claude-bedrock")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got["model"] != "claude-bedrock" {
		t.Errorf("model name should be friendly name, got %v", got["model"])
	}
	if got["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason: %v", got["stop_reason"])
	}
	content := got["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("want 1 block, got %d", len(content))
	}
	tb := content[0].(map[string]any)
	if tb["type"] != "text" || tb["text"] != "Hello!" {
		t.Errorf("text block: %#v", tb)
	}
	if usage.Input != 10 || usage.Output != 5 {
		t.Errorf("usage: %+v", usage)
	}
}

func TestBuildAnthropicResponseFromConverse_ToolUse(t *testing.T) {
	body := []byte(`{
		"output":{"message":{"role":"assistant","content":[
			{"text":"Let me check"},
			{"toolUse":{"toolUseId":"tu_1","name":"get_weather","input":{"location":"Paris"}}}
		]}},
		"stopReason":"tool_use",
		"usage":{"inputTokens":15,"outputTokens":8}
	}`)
	got, _, err := buildAnthropicResponseFromConverse(body, "claude-bedrock")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason: %v", got["stop_reason"])
	}
	content := got["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("want 2 blocks, got %d: %#v", len(content), content)
	}
	tu := content[1].(map[string]any)
	if tu["type"] != "tool_use" || tu["id"] != "tu_1" || tu["name"] != "get_weather" {
		t.Errorf("tool_use block: %#v", tu)
	}
}

func TestBuildAnthropicResponseFromConverse_ReasoningHoistedFirst(t *testing.T) {
	body := []byte(`{
		"output":{"message":{"role":"assistant","content":[
			{"text":"42"},
			{"reasoningContent":{"reasoningText":{"text":"thinking...","signature":"sig1"}}}
		]}},
		"stopReason":"end_turn",
		"usage":{"inputTokens":1,"outputTokens":1}
	}`)
	got, _, err := buildAnthropicResponseFromConverse(body, "x")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	content := got["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("want 2 blocks, got %d", len(content))
	}
	first := content[0].(map[string]any)
	if first["type"] != "thinking" {
		t.Errorf("thinking block must come first, got %#v", first)
	}
	if first["thinking"] != "thinking..." || first["signature"] != "sig1" {
		t.Errorf("thinking content: %#v", first)
	}
}

func TestMapConverseStopReason(t *testing.T) {
	cases := map[string]string{
		"end_turn":             "end_turn",
		"tool_use":             "tool_use",
		"max_tokens":           "max_tokens",
		"stop_sequence":        "stop_sequence",
		"guardrail_intervened": "end_turn",
		"content_filtered":     "end_turn",
		"unknown_future_value": "end_turn",
		"":                     "end_turn",
	}
	for in, want := range cases {
		if got := mapConverseStopReason(in); got != want {
			t.Errorf("mapConverseStopReason(%q) = %q, want %q", in, got, want)
		}
	}
}
