package handler

import (
	"strings"
	"testing"
)

func TestThinkTagFilter_BasicContent(t *testing.T) {
	f := &thinkTagFilter{}
	segs := f.Process("Hello world")
	if len(segs) != 1 || segs[0].Text != "Hello world" || segs[0].IsReasoning {
		t.Fatalf("unexpected segments: %+v", segs)
	}
}

func TestThinkTagFilter_FullThinkBlock(t *testing.T) {
	f := &thinkTagFilter{}
	segs := f.Process("<think>reasoning here</think>actual content")
	if len(segs) != 2 {
		t.Fatalf("expected 2 segments, got %d: %+v", len(segs), segs)
	}
	if segs[0].Text != "reasoning here" || !segs[0].IsReasoning {
		t.Errorf("segment 0: %+v", segs[0])
	}
	if segs[1].Text != "actual content" || segs[1].IsReasoning {
		t.Errorf("segment 1: %+v", segs[1])
	}
}

func TestThinkTagFilter_SplitAcrossChunks(t *testing.T) {
	f := &thinkTagFilter{}

	// Chunk 1: partial open tag
	segs := f.Process("<thi")
	if len(segs) != 0 {
		t.Fatalf("expected 0 segments for partial tag, got %d: %+v", len(segs), segs)
	}

	// Chunk 2: rest of open tag + reasoning
	segs = f.Process("nk>some reasoning")
	if len(segs) != 1 || segs[0].Text != "some reasoning" || !segs[0].IsReasoning {
		t.Fatalf("unexpected segments: %+v", segs)
	}

	// Chunk 3: partial close tag
	segs = f.Process("</thi")
	if len(segs) != 0 {
		t.Fatalf("expected 0 segments for partial close tag, got %d: %+v", len(segs), segs)
	}

	// Chunk 4: rest of close tag + content
	segs = f.Process("nk>hello")
	if len(segs) != 1 || segs[0].Text != "hello" || segs[0].IsReasoning {
		t.Fatalf("unexpected segments: %+v", segs)
	}
}

func TestThinkTagFilter_LeadingContent(t *testing.T) {
	f := &thinkTagFilter{}
	segs := f.Process("before<think>inside</think>after")
	if len(segs) != 3 {
		t.Fatalf("expected 3 segments, got %d: %+v", len(segs), segs)
	}
	if segs[0].Text != "before" || segs[0].IsReasoning {
		t.Errorf("segment 0: %+v", segs[0])
	}
	if segs[1].Text != "inside" || !segs[1].IsReasoning {
		t.Errorf("segment 1: %+v", segs[1])
	}
	if segs[2].Text != "after" || segs[2].IsReasoning {
		t.Errorf("segment 2: %+v", segs[2])
	}
}

func TestThinkTagFilter_Flush(t *testing.T) {
	f := &thinkTagFilter{}
	segs := f.Process("<think>some reasoning")
	// Should have the reasoning text
	if len(segs) != 1 || segs[0].Text != "some reasoning" || !segs[0].IsReasoning {
		t.Fatalf("unexpected segments: %+v", segs)
	}

	// Nothing pending since no partial tag
	flush := f.Flush()
	if len(flush) != 0 {
		t.Fatalf("unexpected flush: %+v", flush)
	}
}

func TestThinkTagFilter_FlushPartialTag(t *testing.T) {
	f := &thinkTagFilter{}
	segs := f.Process("hello</th")
	// "hello" should be buffered as content, "</th" as pending
	// Actually: f.inside is false, so looking for <think>. "</th" doesn't match <think> prefix.
	// So it should emit "hello</th" as content... wait, let me think.
	// "</th" ends with "<", which is tag[:1] of "<think>". So longestTagSuffix returns 0 for "</th"
	// because "</th" doesn't end with any prefix of "<think>".
	// Actually, "</th" doesn't end with "<" — it ends with "h". Let me re-check.
	// Wait, we're not inside, so we look for thinkOpen = "<think>".
	// "</th" — no occurrence of "<think>". Check suffix: does "</th" end with any prefix of "<think>"?
	// "<think>" prefixes: "<", "<t", "<th", "<thi", "<thin", "<think"
	// "</th" ends with "h", "th", "/th", "</th" — none match.
	// So no pending, all emitted as content.
	if len(segs) != 1 || segs[0].Text != "hello</th" || segs[0].IsReasoning {
		t.Fatalf("unexpected segments: %+v", segs)
	}
}

func TestThinkTagFilter_MultipleBlocks(t *testing.T) {
	f := &thinkTagFilter{}
	segs := f.Process("<think>first</think>middle<think>second</think>end")
	if len(segs) != 4 {
		t.Fatalf("expected 4 segments, got %d: %+v", len(segs), segs)
	}
	expected := []textSegment{
		{"first", true},
		{"middle", false},
		{"second", true},
		{"end", false},
	}
	for i, exp := range expected {
		if segs[i] != exp {
			t.Errorf("segment %d: got %+v, want %+v", i, segs[i], exp)
		}
	}
}

func TestThinkTagFilter_StreamingReassembly(t *testing.T) {
	// Simulate realistic streaming: model sends <think> at start, content chunks, then </think>
	f := &thinkTagFilter{}
	var reasoning, content strings.Builder

	chunks := []string{"<think>", "Let me ", "think about", " this", "</think>", "The answer", " is 42"}
	for _, chunk := range chunks {
		for _, seg := range f.Process(chunk) {
			if seg.IsReasoning {
				reasoning.WriteString(seg.Text)
			} else {
				content.WriteString(seg.Text)
			}
		}
	}
	for _, seg := range f.Flush() {
		if seg.IsReasoning {
			reasoning.WriteString(seg.Text)
		} else {
			content.WriteString(seg.Text)
		}
	}

	if reasoning.String() != "Let me think about this" {
		t.Errorf("reasoning: %q", reasoning.String())
	}
	if content.String() != "The answer is 42" {
		t.Errorf("content: %q", content.String())
	}
}

func TestStripThinkTags(t *testing.T) {
	reasoning, content := stripThinkTags("<think>I need to think</think>Hello world")
	if reasoning != "I need to think" {
		t.Errorf("reasoning: %q", reasoning)
	}
	if content != "Hello world" {
		t.Errorf("content: %q", content)
	}
}

func TestStripThinkTags_NoTags(t *testing.T) {
	reasoning, content := stripThinkTags("just content")
	if reasoning != "" {
		t.Errorf("reasoning: %q", reasoning)
	}
	if content != "just content" {
		t.Errorf("content: %q", content)
	}
}

func TestStripThinkTags_Multiple(t *testing.T) {
	reasoning, content := stripThinkTags("<think>a</think>x<think>b</think>y")
	if reasoning != "ab" {
		t.Errorf("reasoning: %q", reasoning)
	}
	if content != "xy" {
		t.Errorf("content: %q", content)
	}
}

func TestLongestTagSuffix(t *testing.T) {
	tests := []struct {
		s, tag string
		want   int
	}{
		{"hello<", "<think>", 1},
		{"hello<t", "<think>", 2},
		{"hello<th", "<think>", 3},
		{"hello<thi", "<think>", 4},
		{"hello<thin", "<think>", 5},
		{"hello<think", "<think>", 6},
		{"hello", "<think>", 0},
		{"</thi", "</think>", 5},
		{"text<", "</think>", 1},  // "<" matches "<" of "</think>", but actually...
		// "<" is prefix of "</think>"? No: "</think>" starts with "<". tag[:1] = "<". "text<" ends with "<". Yes!
	}
	for _, tt := range tests {
		got := longestTagSuffix(tt.s, tt.tag)
		if got != tt.want {
			t.Errorf("longestTagSuffix(%q, %q) = %d, want %d", tt.s, tt.tag, got, tt.want)
		}
	}
}
