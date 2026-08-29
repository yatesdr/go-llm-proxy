package handler

import (
	"encoding/json"
	"testing"
)

// Regression: Responses-API clients (Codex) emit developer-role items
// mid-input. Translating them in place produces a second system message
// after position 0, which Qwen-style chat templates reject with
// "System message must be at the beginning.".
func TestTranslateInputDefersMidInputSystem(t *testing.T) {
	input := `[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]},
		{"type":"message","role":"developer","content":[{"type":"input_text","text":"extra dev rules"}]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"go on"}]}
	]`
	msgs, err := translateInput(json.RawMessage(input), "base instructions")
	if err != nil {
		t.Fatalf("translateInput: %v", err)
	}
	if len(msgs) == 0 {
		t.Fatal("no messages")
	}
	if msgs[0]["role"] != "system" {
		t.Fatalf("first message role = %v, want system", msgs[0]["role"])
	}
	for i, m := range msgs {
		if m["role"] == "system" && i > 0 {
			t.Fatalf("system message at index %d (mid-input system leaked through): %+v", i, msgs)
		}
	}
	sys, _ := msgs[0]["content"].(string)
	if sys != "base instructions\n\nextra dev rules" {
		t.Fatalf("merged system content = %q", sys)
	}
	if msgs[len(msgs)-1]["role"] != "user" {
		t.Fatalf("last message role = %v, want user", msgs[len(msgs)-1]["role"])
	}
}

// Mid-input developer with no instructions: a system message must be
// prepended, not appended.
func TestTranslateInputPrependsSystemWhenNoInstructions(t *testing.T) {
	input := `[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]},
		{"type":"message","role":"developer","content":"late dev note"},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"again"}]}
	]`
	msgs, err := translateInput(json.RawMessage(input), "")
	if err != nil {
		t.Fatalf("translateInput: %v", err)
	}
	if len(msgs) < 3 {
		t.Fatalf("expected >=3 messages, got %d: %+v", len(msgs), msgs)
	}
	if msgs[0]["role"] != "system" {
		t.Fatalf("first message role = %v, want system (prepended)", msgs[0]["role"])
	}
	sys, _ := msgs[0]["content"].(string)
	if sys != "late dev note" {
		t.Fatalf("prepended system content = %q", sys)
	}
	if msgs[1]["role"] != "user" {
		t.Fatalf("second message role = %v, want user", msgs[1]["role"])
	}
}

// First-position developer/system items must keep their original behavior
// (in-place system message, no merging needed).
func TestTranslateInputFirstSystemStaysInPlace(t *testing.T) {
	input := `[
		{"type":"message","role":"developer","content":[{"type":"input_text","text":"dev first"}]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}
	]`
	msgs, err := translateInput(json.RawMessage(input), "")
	if err != nil {
		t.Fatalf("translateInput: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d: %+v", len(msgs), msgs)
	}
	if msgs[0]["role"] != "system" {
		t.Fatalf("first message role = %v, want system", msgs[0]["role"])
	}
	// Array content (parts) is valid: translateContentForChat keeps the
	// input_text parts shape for non-assistant roles. Just require the text
	// to survive somewhere in the serialized content.
	blob, _ := json.Marshal(msgs[0]["content"])
	if !contains(string(blob), "dev first") {
		t.Fatalf("system content %s does not contain %q", blob, "dev first")
	}
}
