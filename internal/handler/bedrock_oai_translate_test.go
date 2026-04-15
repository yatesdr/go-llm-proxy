package handler

import (
	"encoding/json"
	"strings"
	"testing"
)

// --- Request: OAI Chat Completions -> Converse ---

func TestBuildConverseRequestFromChat_BasicExchange(t *testing.T) {
	body := []byte(`{
		"model": "claude-bedrock",
		"messages": [
			{"role": "system", "content": "You are helpful."},
			{"role": "user", "content": "Hi"}
		],
		"max_tokens": 100,
		"temperature": 0.5
	}`)
	got, parsed, err := buildConverseRequestFromChat(body)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if parsed.Stream {
		t.Errorf("stream wrongly true")
	}

	sys := got["system"].([]map[string]any)
	if sys[0]["text"] != "You are helpful." {
		t.Errorf("system: %v", sys)
	}

	msgs := got["messages"].([]map[string]any)
	if len(msgs) != 1 || msgs[0]["role"] != "user" {
		t.Errorf("messages: %#v", msgs)
	}
	content := msgs[0]["content"].([]map[string]any)
	if content[0]["text"] != "Hi" {
		t.Errorf("user content: %#v", content)
	}

	inf := got["inferenceConfig"].(map[string]any)
	if inf["maxTokens"] != 100 || inf["temperature"] != 0.5 {
		t.Errorf("inferenceConfig: %#v", inf)
	}
}

func TestBuildConverseRequestFromChat_MaxCompletionTokensWins(t *testing.T) {
	body := []byte(`{
		"messages": [{"role":"user","content":"hi"}],
		"max_tokens": 50,
		"max_completion_tokens": 200
	}`)
	got, _, _ := buildConverseRequestFromChat(body)
	inf := got["inferenceConfig"].(map[string]any)
	if inf["maxTokens"] != 200 {
		t.Errorf("max_completion_tokens should win, got maxTokens=%v", inf["maxTokens"])
	}
}

func TestBuildConverseRequestFromChat_StopAsString(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"hi"}],"stop":"END"}`)
	got, _, _ := buildConverseRequestFromChat(body)
	inf := got["inferenceConfig"].(map[string]any)
	stop := inf["stopSequences"].([]string)
	if len(stop) != 1 || stop[0] != "END" {
		t.Errorf("stopSequences: %v", stop)
	}
}

func TestBuildConverseRequestFromChat_StopAsArray(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"hi"}],"stop":["A","B"]}`)
	got, _, _ := buildConverseRequestFromChat(body)
	inf := got["inferenceConfig"].(map[string]any)
	stop := inf["stopSequences"].([]string)
	if len(stop) != 2 || stop[0] != "A" || stop[1] != "B" {
		t.Errorf("stopSequences: %v", stop)
	}
}

func TestBuildConverseRequestFromChat_ToolCallRoundTrip(t *testing.T) {
	body := []byte(`{
		"messages": [
			{"role":"user","content":"What's the weather?"},
			{"role":"assistant","content":"Checking...","tool_calls":[
				{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}
			]},
			{"role":"tool","tool_call_id":"call_1","content":"22C sunny"}
		]
	}`)
	got, _, err := buildConverseRequestFromChat(body)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	msgs := got["messages"].([]map[string]any)
	if len(msgs) != 3 {
		t.Fatalf("want 3 messages, got %d: %#v", len(msgs), msgs)
	}

	// Assistant: text + toolUse blocks.
	asst := msgs[1]["content"].([]map[string]any)
	if len(asst) != 2 {
		t.Fatalf("assistant blocks: want 2, got %d: %#v", len(asst), asst)
	}
	if asst[0]["text"] != "Checking..." {
		t.Errorf("assistant text: %#v", asst[0])
	}
	tu := asst[1]["toolUse"].(map[string]any)
	if tu["toolUseId"] != "call_1" || tu["name"] != "get_weather" {
		t.Errorf("toolUse: %#v", tu)
	}
	// input must be a parsed object (json.RawMessage), not a string.
	if _, ok := tu["input"].(json.RawMessage); !ok {
		t.Errorf("toolUse input should be json.RawMessage, got %T", tu["input"])
	}

	// Tool message → user message with toolResult block.
	if msgs[2]["role"] != "user" {
		t.Errorf("tool message should become user message, got role=%v", msgs[2]["role"])
	}
	usr := msgs[2]["content"].([]map[string]any)
	if len(usr) != 1 {
		t.Fatalf("tool result blocks: %#v", usr)
	}
	tr := usr[0]["toolResult"].(map[string]any)
	if tr["toolUseId"] != "call_1" {
		t.Errorf("toolResult ID: %#v", tr)
	}
	trContent := tr["content"].([]map[string]any)
	if trContent[0]["text"] != "22C sunny" {
		t.Errorf("toolResult content: %#v", trContent)
	}
}

func TestBuildConverseRequestFromChat_MergesConsecutiveToolMessages(t *testing.T) {
	body := []byte(`{
		"messages": [
			{"role":"user","content":"q"},
			{"role":"assistant","tool_calls":[
				{"id":"a","type":"function","function":{"name":"f","arguments":"{}"}},
				{"id":"b","type":"function","function":{"name":"g","arguments":"{}"}}
			]},
			{"role":"tool","tool_call_id":"a","content":"R1"},
			{"role":"tool","tool_call_id":"b","content":"R2"}
		]
	}`)
	got, _, _ := buildConverseRequestFromChat(body)
	msgs := got["messages"].([]map[string]any)
	if len(msgs) != 3 {
		t.Fatalf("want 3 messages (user, assistant, merged-tool-results), got %d", len(msgs))
	}
	last := msgs[2]
	if last["role"] != "user" {
		t.Errorf("merged tool results should land on user role, got %v", last["role"])
	}
	blocks := last["content"].([]map[string]any)
	if len(blocks) != 2 {
		t.Errorf("want 2 toolResult blocks merged, got %d: %#v", len(blocks), blocks)
	}
}

func TestBuildConverseRequestFromChat_ImageDataURL(t *testing.T) {
	body := []byte(`{
		"messages": [{
			"role": "user",
			"content": [
				{"type":"text","text":"What is this?"},
				{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}}
			]
		}]
	}`)
	got, _, _ := buildConverseRequestFromChat(body)
	msgs := got["messages"].([]map[string]any)
	content := msgs[0]["content"].([]map[string]any)
	if len(content) != 2 {
		t.Fatalf("want 2 blocks, got %d: %#v", len(content), content)
	}
	img, ok := content[1]["image"].(map[string]any)
	if !ok {
		t.Fatalf("expected image block: %#v", content[1])
	}
	if img["format"] != "png" {
		t.Errorf("format: %v", img["format"])
	}
	src := img["source"].(map[string]any)
	if src["bytes"] == "" {
		t.Errorf("bytes empty: %#v", src)
	}
}

func TestBuildConverseRequestFromChat_ImageURLDropped(t *testing.T) {
	body := []byte(`{
		"messages": [{
			"role": "user",
			"content": [
				{"type":"text","text":"hi"},
				{"type":"image_url","image_url":{"url":"https://example.com/cat.jpg"}}
			]
		}]
	}`)
	got, _, _ := buildConverseRequestFromChat(body)
	content := got["messages"].([]map[string]any)[0]["content"].([]map[string]any)
	// Remote URL is dropped (Bedrock doesn't fetch); only text block remains.
	if len(content) != 1 || content[0]["text"] != "hi" {
		t.Errorf("expected only text block, got %#v", content)
	}
}

func TestBuildConverseRequestFromChat_Tools(t *testing.T) {
	body := []byte(`{
		"messages":[{"role":"user","content":"hi"}],
		"tools":[
			{"type":"function","function":{"name":"get_weather","description":"Weather","parameters":{"type":"object"}}}
		],
		"tool_choice":"required"
	}`)
	got, _, _ := buildConverseRequestFromChat(body)
	cfg := got["toolConfig"].(map[string]any)
	tools := cfg["tools"].([]map[string]any)
	if len(tools) != 1 {
		t.Fatalf("want 1 tool, got %d", len(tools))
	}
	spec := tools[0]["toolSpec"].(map[string]any)
	if spec["name"] != "get_weather" {
		t.Errorf("tool name: %v", spec["name"])
	}
	tc := cfg["toolChoice"].(map[string]any)
	if tc["any"] == nil {
		t.Errorf("required → any expected, got %#v", tc)
	}
}

func TestBuildConverseRequestFromChat_ToolChoiceForceFunction(t *testing.T) {
	body := []byte(`{
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"f","parameters":{}}}],
		"tool_choice":{"type":"function","function":{"name":"f"}}
	}`)
	got, _, _ := buildConverseRequestFromChat(body)
	cfg := got["toolConfig"].(map[string]any)
	tc := cfg["toolChoice"].(map[string]any)
	tool := tc["tool"].(map[string]any)
	if tool["name"] != "f" {
		t.Errorf("forced tool: %#v", tc)
	}
}

func TestBuildConverseRequestFromChat_DeveloperRoleAsSystem(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"developer","content":"Be terse."},
			{"role":"user","content":"hi"}
		]
	}`)
	got, _, _ := buildConverseRequestFromChat(body)
	sys := got["system"].([]map[string]any)
	if sys[0]["text"] != "Be terse." {
		t.Errorf("developer role should map to system, got %#v", sys)
	}
}

// --- Response: Converse -> OAI Chat Completions ---

func TestBuildChatResponseFromConverse_Text(t *testing.T) {
	body := []byte(`{
		"output":{"message":{"role":"assistant","content":[{"text":"Hello!"}]}},
		"stopReason":"end_turn",
		"usage":{"inputTokens":10,"outputTokens":2,"totalTokens":12}
	}`)
	resp, usage, err := buildChatResponseFromConverse(body, "claude-bedrock")
	if err != nil {
		t.Fatalf("err: %v", err)
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
	if c.Message.Content == nil || *c.Message.Content != "Hello!" {
		t.Errorf("content: %v", c.Message.Content)
	}
	if resp.Usage.PromptTokens != 10 || resp.Usage.CompletionTokens != 2 {
		t.Errorf("usage: %+v", resp.Usage)
	}
	if usage.Input != 10 || usage.Output != 2 {
		t.Errorf("converse usage: %+v", usage)
	}
}

func TestBuildChatResponseFromConverse_ToolUse(t *testing.T) {
	body := []byte(`{
		"output":{"message":{"role":"assistant","content":[
			{"text":"Let me check"},
			{"toolUse":{"toolUseId":"tu_1","name":"get_weather","input":{"city":"Paris"}}}
		]}},
		"stopReason":"tool_use",
		"usage":{"inputTokens":1,"outputTokens":1}
	}`)
	resp, _, err := buildChatResponseFromConverse(body, "x")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	c := resp.Choices[0]
	if c.FinishReason != "tool_calls" {
		t.Errorf("finish_reason: %v", c.FinishReason)
	}
	if len(c.Message.ToolCalls) != 1 {
		t.Fatalf("tool_calls: %#v", c.Message.ToolCalls)
	}
	tc := c.Message.ToolCalls[0]
	if tc.ID != "tu_1" || tc.Function.Name != "get_weather" {
		t.Errorf("tool call: %+v", tc)
	}
	// Arguments must be a JSON string (as OAI demands).
	if !strings.HasPrefix(tc.Function.Arguments, "{") {
		t.Errorf("arguments should be JSON string, got %q", tc.Function.Arguments)
	}
}

func TestBuildChatResponseFromConverse_ReasoningPreserved(t *testing.T) {
	body := []byte(`{
		"output":{"message":{"role":"assistant","content":[
			{"reasoningContent":{"reasoningText":{"text":"thinking..."}}},
			{"text":"42"}
		]}},
		"stopReason":"end_turn",
		"usage":{"inputTokens":1,"outputTokens":1}
	}`)
	resp, _, _ := buildChatResponseFromConverse(body, "x")
	c := resp.Choices[0]
	if c.Message.Reasoning == nil || *c.Message.Reasoning != "thinking..." {
		t.Errorf("reasoning: %v", c.Message.Reasoning)
	}
	if c.Message.Content == nil || *c.Message.Content != "42" {
		t.Errorf("content: %v", c.Message.Content)
	}
}

func TestMapConverseStopReasonToChat(t *testing.T) {
	cases := map[string]string{
		"end_turn":             "stop",
		"stop_sequence":        "stop",
		"tool_use":             "tool_calls",
		"max_tokens":           "length",
		"guardrail_intervened": "content_filter",
		"content_filtered":     "content_filter",
		"":                     "stop",
	}
	for in, want := range cases {
		if got := mapConverseStopReasonToChat(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}

func TestParseDataURL(t *testing.T) {
	mt, data, ok := parseDataURL("data:image/jpeg;base64,/9j/2wBDAAg=")
	if !ok || mt != "image/jpeg" || data == "" {
		t.Errorf("good URL: ok=%v mt=%q data=%q", ok, mt, data)
	}
	if _, _, ok := parseDataURL("https://example.com/x.png"); ok {
		t.Errorf("non-data URL should fail")
	}
	if _, _, ok := parseDataURL("data:image/png,abc"); ok {
		t.Errorf("missing base64 marker should fail")
	}
	if _, _, ok := parseDataURL("data:image/png;base64,!!!"); ok {
		t.Errorf("invalid base64 should fail")
	}
}
