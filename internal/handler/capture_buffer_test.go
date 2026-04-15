package handler

import (
	"bytes"
	"strings"
	"testing"
)

func TestCaptureBuffer_NonStreaming_CapsAtPrefix(t *testing.T) {
	b := newCaptureBuffer(false)
	// Write twice the cap; anything past prefixCap must be dropped.
	data := bytes.Repeat([]byte{'x'}, maxUsageCaptureBytes+1024)
	b.write(data)
	got := b.bytes()
	if len(got) != maxUsageCaptureBytes {
		t.Errorf("non-streaming capture len=%d, want %d", len(got), maxUsageCaptureBytes)
	}
}

func TestCaptureBuffer_Streaming_KeepsPrefixAndTail(t *testing.T) {
	b := newCaptureBuffer(true)
	// Write enough to overflow the prefix and build a long tail stream.
	// Use identifiable markers at the start and end.
	head := []byte("HEAD-MARKER-")
	b.write(head)
	filler := bytes.Repeat([]byte{'x'}, 200*1024)
	b.write(filler)
	final := []byte("\ndata: {\"usage\":{\"prompt_tokens\":10}}\n\n")
	b.write(final)

	got := b.bytes()
	s := string(got)
	if !strings.Contains(s, "HEAD-MARKER") {
		t.Errorf("prefix dropped; got first bytes: %q", s[:min(len(s), 60)])
	}
	if !strings.Contains(s, `"usage"`) {
		t.Errorf("tail (final SSE chunk with usage) missing; got last bytes: %q",
			s[max(0, len(s)-80):])
	}
	// Bound: prefix cap + tail cap + a join newline.
	if len(got) > 16*1024+streamTailCaptureBytes+16 {
		t.Errorf("captured body too large: %d bytes", len(got))
	}
}

func TestCaptureBuffer_Streaming_ShortResponseKeepsEverything(t *testing.T) {
	b := newCaptureBuffer(true)
	body := []byte("data: {\"id\":\"x\"}\n\ndata: {\"usage\":{\"prompt_tokens\":1}}\n\ndata: [DONE]\n\n")
	b.write(body)
	got := b.bytes()
	if !bytes.Equal(got, body) && !strings.Contains(string(got), string(body)) {
		// Short responses fit in prefix; bytes() just returns prefix.
		// It's OK if the tail joiner added a \n, but the body must be present.
		t.Errorf("short streaming body not captured:\n  got:  %q\n  want: %q", got, body)
	}
}

func TestCaptureBuffer_Discard(t *testing.T) {
	b := newCaptureBuffer(true)
	b.write([]byte("something"))
	b.discard()
	if got := b.bytes(); got != nil {
		t.Errorf("discarded buffer should return nil, got %q", got)
	}
	// Writes after discard are no-ops.
	b.write([]byte("more"))
	if got := b.bytes(); got != nil {
		t.Errorf("writes after discard should stay dropped, got %q", got)
	}
}

func TestCaptureBuffer_EmptyReturnsNil(t *testing.T) {
	if got := newCaptureBuffer(false).bytes(); got != nil {
		t.Errorf("empty non-streaming should return nil, got %q", got)
	}
	if got := newCaptureBuffer(true).bytes(); got != nil {
		t.Errorf("empty streaming should return nil, got %q", got)
	}
}

func TestCaptureBuffer_Streaming_TailRingWrapsCorrectly(t *testing.T) {
	b := newCaptureBuffer(true)
	// Fill past prefix cap so tail starts capturing.
	b.write(bytes.Repeat([]byte{'P'}, 16*1024))

	// Write a sequence that wraps the ring at least twice.
	for i := 0; i < streamTailCaptureBytes*3; i++ {
		b.write([]byte{byte(i & 0xff)})
	}
	// Last byte written is streamTailCaptureBytes*3 - 1.
	got := b.bytes()
	// The tail's last byte must be the most recently written byte.
	lastWanted := byte((streamTailCaptureBytes*3 - 1) & 0xff)
	if got[len(got)-1] != lastWanted {
		t.Errorf("tail ring corrupted final byte: got %d want %d", got[len(got)-1], lastWanted)
	}
}
