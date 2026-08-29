package handler

import (
	"encoding/json"
	"testing"
)

// Regression: Codex and other Responses-API clients emit top-level
// input_image items (no wrapping message) in multi-turn history. The
// translator must merge them into the most recent user message instead of
// silently dropping them (the pre-fix default case skipped them).
func TestTranslateInputTopLevelImageMergesIntoUser(t *testing.T) {
	input := `[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"describe this"}]},
		{"type":"input_image","image_url":"data:image/png;base64,AAAA","detail":"high"},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"and this one"}]},
		{"type":"input_image","image_url":"https://example.com/b.jpg"}
	]`
	msgs, err := translateInput(json.RawMessage(input), "")
	if err != nil {
		t.Fatalf("translateInput: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 user messages, got %d: %s", len(msgs), mustJSON(msgs))
	}
	for i, m := range msgs {
		if m["role"] != "user" {
			t.Fatalf("msg[%d] role = %v, want user", i, m["role"])
		}
	}
	b0 := mustJSON(msgs[0]["content"])
	if !contains(b0, "describe this") || !contains(b0, "AAAA") || !contains(b0, `"detail":"high"`) {
		t.Fatalf("msg[0] content missing text or first image: %s", b0)
	}
	b1 := mustJSON(msgs[1]["content"])
	if !contains(b1, "and this one") || !contains(b1, "https://example.com/b.jpg") {
		t.Fatalf("msg[1] content missing text or second image: %s", b1)
	}
}

// A top-level input_image with no preceding user message opens a new user
// message (does not error, does not attach to assistant/system).
func TestTranslateInputTopLevelImageOpensUserWhenNone(t *testing.T) {
	input := `[
		{"type":"input_image","image_url":"https://example.com/c.png"}
	]`
	msgs, err := translateInput(json.RawMessage(input), "")
	if err != nil {
		t.Fatalf("translateInput: %v", err)
	}
	if len(msgs) != 1 || msgs[0]["role"] != "user" {
		t.Fatalf("expected single user message, got %d: %s", len(msgs), mustJSON(msgs))
	}
	b := mustJSON(msgs[0]["content"])
	if !contains(b, "https://example.com/c.png") {
		t.Fatalf("image url missing: %s", b)
	}
}

// A PDF data URL masquerading as input_image must route to the PDF
// pipeline (pdf_data part), mirroring the nested input_image path.
func TestTranslateInputTopLevelPDFRoutesToPipeline(t *testing.T) {
	input := `[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"read this"}]},
		{"type":"input_image","image_url":"data:application/pdf;base64,AAAA"}
	]`
	msgs, err := translateInput(json.RawMessage(input), "")
	if err != nil {
		t.Fatalf("translateInput: %v", err)
	}
	b := mustJSON(msgs)
	if !contains(b, `"type":"pdf_data"`) {
		t.Fatalf("pdf_data part missing: %s", b)
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
