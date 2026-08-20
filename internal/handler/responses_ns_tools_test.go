package handler

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestTranslateToolsNamespace: real ChatGPT desktop dump shape (16 tools from
// /tmp/reqtools/latest.json, reduced to one namespace + one plain function +
// one custom + one web_search).
func TestTranslateToolsNamespace(t *testing.T) {
	toolsJSON := []byte(`[
		{"type":"function","name":"exec_command","description":"Run a command","parameters":{"type":"object","properties":{"cmd":{"type":"string"}},"required":["cmd"]}},
		{"type":"namespace","name":"mcp__node_repl","description":"repl tools","tools":[
			{"type":"function","name":"js","description":"Execute JS","strict":false,"parameters":{"type":"object","properties":{"code":{"type":"string"}},"required":["code"],"additionalProperties":false}},
			{"type":"function","name":"js_reset","description":"Reset","parameters":{"type":"object","properties":{},"additionalProperties":false}}
		]},
		{"type":"namespace","name":"multi_agent_v1","description":"sub-agents","tools":[
			{"type":"function","name":"spawn_agent","description":"Spawn","parameters":{"type":"object","properties":{"message":{"type":"string"}},"additionalProperties":false}}
		]},
		{"type":"custom","name":"apply_patch","description":"Edit files","format":{"type":"text","syntax":"apply_patch","definition":"*** Begin Patch\\n..."}},
		{"type":"web_search","external_web_access":false}
	]`)

	var raw []json.RawMessage
	if err := json.Unmarshal(toolsJSON, &raw); err != nil {
		t.Fatal(err)
	}
	tools, stripped, mappings := translateTools(raw)

	// 5 input tools -> 1 + 2 + 1 + 1 = 5 flattened function tools
	if len(tools) != 5 {
		t.Fatalf("expected 5 flattened tools, got %d: %v", len(tools), tools)
	}
	if len(stripped) != 1 || stripped[0] != "web_search" {
		t.Fatalf("expected web_search stripped, got %v", stripped)
	}

	// Check flattened names
	names := map[string]bool{}
	for _, t := range tools {
		fn := t["function"].(map[string]any)
		names[fn["name"].(string)] = true
	}
	for _, want := range []string{"exec_command", "mcp__node_repljs", "mcp__node_repljs_reset", "multi_agent_v1spawn_agent", "apply_patch"} {
		if !names[want] {
			t.Errorf("missing flattened name %q; have %v", want, names)
		}
	}

	// Mappings: only non-plain tools
	if len(mappings) != 4 {
		t.Fatalf("expected 4 mappings, got %d: %v", len(mappings), mappings)
	}
	mm := toolMappingsByFlatName(mappings)

	it, name, ns := resolveToolCall(mm, "mcp__node_repljs")
	if it != "function_call" || name != "js" || ns != "mcp__node_repl" {
		t.Errorf("resolve mcp__node_repljs = (%q,%q,%q), want (function_call,js,mcp__node_repl)", it, name, ns)
	}
	it, name, ns = resolveToolCall(mm, "apply_patch")
	if it != "custom_tool_call" || name != "apply_patch" || ns != "" {
		t.Errorf("resolve apply_patch = (%q,%q,%q), want (custom_tool_call,apply_patch,)", it, name, ns)
	}
	// Plain tool: no mapping entry, falls through
	it, name, ns = resolveToolCall(mm, "exec_command")
	if it != "function_call" || name != "exec_command" || ns != "" {
		t.Errorf("resolve exec_command = (%q,%q,%q)", it, name, ns)
	}

	// custom tool_call item shape
	item := toolCallItemMap("fc_1", "call_1", "apply_patch", "", "custom_tool_call", `{"input":"*** Begin Patch"}`)
	if item["type"] != "custom_tool_call" || item["input"] != `{"input":"*** Begin Patch"}` {
		t.Errorf("custom item shape wrong: %v", item)
	}
	if _, has := item["arguments"]; has {
		t.Errorf("custom item must not have arguments field: %v", item)
	}

	// namespaced function item shape
	item = toolCallItemMap("fc_2", "call_2", "js", "mcp__node_repl", "function_call", `{"code":"1"}`)
	if item["type"] != "function_call" || item["namespace"] != "mcp__node_repl" || item["name"] != "js" {
		t.Errorf("namespaced item shape wrong: %v", item)
	}

	// customToolInput extraction
	in := customToolInput(`{"input":"*** Begin Patch\n*** Update File: x"}`)
	if !strings.HasPrefix(in, "*** Begin Patch") {
		t.Errorf("customToolInput = %q", in)
	}
	if customToolInput("not json") != "not json" {
		t.Errorf("customToolInput fallback broken")
	}

	// custom params schema
	p := string(customToolParams(&customToolFormat{Definition: "PATCH FORMAT"}))
	if !strings.Contains(p, `"input"`) || !strings.Contains(p, "PATCH FORMAT") || !strings.Contains(p, `"required"`) {
		t.Errorf("customToolParams = %s", p)
	}
}

// TestTranslateInputNamespacedHistory: function_call with namespace field must
// be flattened; custom_tool_call input must be wrapped.
func TestTranslateInputNamespacedHistory(t *testing.T) {
	input := []byte(`[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"do it"}]},
		{"type":"function_call","id":"fc_1","call_id":"call_1","name":"js","namespace":"mcp__node_repl","arguments":"{\"code\":\"1\"}"},
		{"type":"function_call_output","call_id":"call_1","output":"ok"},
		{"type":"custom_tool_call","id":"fc_2","call_id":"call_2","name":"apply_patch","input":"*** Begin Patch\n*** Update File: a.txt"},
		{"type":"custom_tool_call_output","call_id":"call_2","output":"done"}
	]`)

	msgs, err := translateInput(input, "")
	if err != nil {
		t.Fatal(err)
	}
	// Find the assistant tool_call with flattened name
	found := false
	var customArgs string
	for _, m := range msgs {
		if m["role"] != "assistant" {
			continue
		}
		tcs, _ := m["tool_calls"].([]any)
		for _, tcv := range tcs {
			tc := tcv.(map[string]any)
			fn := tc["function"].(map[string]any)
			if fn["name"] == "mcp__node_repljs" {
				found = true
			}
			if fn["name"] == "apply_patch" {
				customArgs = fn["arguments"].(string)
			}
		}
	}
	if !found {
		t.Errorf("namespaced function_call not flattened; messages: %v", msgs)
	}
	if customArgs != `{"input":"*** Begin Patch\n*** Update File: a.txt"}` {
		t.Errorf("custom_tool_call input not wrapped: %q", customArgs)
	}
}
