package handler

import (
	"encoding/json"
	"strings"
	"testing"
)

// Claude Code's tool search: deferred tools reach the model only after a
// ToolSearch result has referenced them; the placeholder never does.
func TestDeferredToolsLoadAfterToolReference(t *testing.T) {
	tools := []json.RawMessage{
		json.RawMessage(`{"name":"Read","description":"read","input_schema":{"type":"object"}}`),
		json.RawMessage(`{"name":"ToolSearch","description":"find tools","input_schema":{"type":"object"}}`),
		json.RawMessage(`{"name":"DeferredToolPlaceholder","description":"never call","input_schema":{"type":"object"},"defer_loading":true}`),
		json.RawMessage(`{"name":"mcp__mini__shout","description":"shout","input_schema":{"type":"object"},"defer_loading":true}`),
	}
	names := func(msgs []json.RawMessage) []string {
		got, _ := translateAnthropicToolsToChat(tools, referencedDeferredTools(msgs))
		var n []string
		for _, tl := range got {
			n = append(n, tl["function"].(map[string]any)["name"].(string))
		}
		return n
	}
	before := []json.RawMessage{json.RawMessage(`{"role":"user","content":"shout hello"}`)}
	if got := strings.Join(names(before), ","); got != "Read,ToolSearch" {
		t.Fatalf("before tool search: %s", got)
	}
	after := append(before,
		json.RawMessage(`{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"ToolSearch","input":{"query":"shout"}}]}`),
		json.RawMessage(`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"tool_reference","tool_name":"mcp__mini__shout"}]}]}`))
	if got := strings.Join(names(after), ","); got != "Read,ToolSearch,mcp__mini__shout" {
		t.Fatalf("after tool search: %s", got)
	}
	if text := extractToolResultContent(json.RawMessage(`[{"type":"tool_reference","tool_name":"mcp__mini__shout"}]`)); !strings.Contains(text, "mcp__mini__shout is now loaded") {
		t.Fatalf("tool_reference text: %q", text)
	}
}
