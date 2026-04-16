package handler

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"net/http/httptest"
	"strings"
	"testing"
)

// buildEventFrame constructs a single AWS event-stream frame for tests.
// (Duplicated from internal/awsstream/eventstream_test.go's encodeFrame —
// we don't expose that helper publicly, and copying ~25 lines is preferable
// to making a public production API just for tests.)
func buildEventFrame(t *testing.T, headers map[string]string, payload []byte) []byte {
	t.Helper()
	const headerTypeString = 7
	var hb bytes.Buffer
	for name, val := range headers {
		hb.WriteByte(byte(len(name)))
		hb.WriteString(name)
		hb.WriteByte(headerTypeString)
		var lb [2]byte
		binary.BigEndian.PutUint16(lb[:], uint16(len(val)))
		hb.Write(lb[:])
		hb.WriteString(val)
	}
	headerBytes := hb.Bytes()
	totalLen := 12 + len(headerBytes) + len(payload) + 4

	var out bytes.Buffer
	prelude := make([]byte, 8)
	binary.BigEndian.PutUint32(prelude[0:4], uint32(totalLen))
	binary.BigEndian.PutUint32(prelude[4:8], uint32(len(headerBytes)))
	out.Write(prelude)
	binary.Write(&out, binary.BigEndian, crc32.ChecksumIEEE(prelude))
	out.Write(headerBytes)
	out.Write(payload)
	binary.Write(&out, binary.BigEndian, crc32.ChecksumIEEE(out.Bytes()))
	return out.Bytes()
}

func buildBedrockStream(t *testing.T, frames []struct{ Event, Payload string }) []byte {
	t.Helper()
	var buf bytes.Buffer
	for _, f := range frames {
		buf.Write(buildEventFrame(t, map[string]string{
			":message-type": "event",
			":event-type":   f.Event,
			":content-type": "application/json",
		}, []byte(f.Payload)))
	}
	return buf.Bytes()
}

// parseSSE returns the sequence of (event-name, parsed-data-object) pairs
// emitted on the wire. Order matters; this is what the client sees.
func parseSSE(t *testing.T, body string) []bedrockSSEEvent {
	t.Helper()
	var out []bedrockSSEEvent
	for _, block := range strings.Split(strings.TrimSpace(body), "\n\n") {
		if block == "" {
			continue
		}
		var ev bedrockSSEEvent
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				ev.Name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				raw := strings.TrimPrefix(line, "data: ")
				ev.Raw = raw
				_ = json.Unmarshal([]byte(raw), &ev.Data)
			}
		}
		out = append(out, ev)
	}
	return out
}

type bedrockSSEEvent struct {
	Name string
	Raw  string
	Data map[string]any
}

// eventNames extracts just the names in order — used for assertions on
// the structural sequence of events.
func eventNames(events []bedrockSSEEvent) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.Name
	}
	return out
}

func TestStreamBedrockToAnthropicSSE_TextOnly(t *testing.T) {
	stream := buildBedrockStream(t, []struct{ Event, Payload string }{
		{"messageStart", `{"role":"assistant"}`},
		{"contentBlockDelta", `{"delta":{"text":"Hello"},"contentBlockIndex":0}`},
		{"contentBlockDelta", `{"delta":{"text":" world"},"contentBlockIndex":0}`},
		{"contentBlockStop", `{"contentBlockIndex":0}`},
		{"messageStop", `{"stopReason":"end_turn"}`},
		{"metadata", `{"usage":{"inputTokens":10,"outputTokens":2,"totalTokens":12}}`},
	})

	w := httptest.NewRecorder()
	bytes, usage := streamBedrockToAnthropicSSE(w, strings.NewReader(string(stream)), "claude-bedrock", "")
	if bytes == 0 {
		t.Fatalf("no bytes written")
	}
	if usage == nil || usage.Input != 10 || usage.Output != 2 {
		t.Errorf("usage: %+v", usage)
	}

	events := parseSSE(t, w.Body.String())
	want := []string{
		"message_start", "ping",
		"content_block_start", "content_block_delta", "content_block_delta", "content_block_stop",
		"message_delta", "message_stop",
	}
	got := eventNames(events)
	if !equalStrings(got, want) {
		t.Fatalf("event sequence:\n  got:  %v\n  want: %v", got, want)
	}

	// message_start carries the friendly model name.
	msgStart := events[0]
	model := msgStart.Data["message"].(map[string]any)["model"]
	if model != "claude-bedrock" {
		t.Errorf("model in message_start: %v", model)
	}

	// First content_block_start must be a text block.
	cbs := events[2]
	cb := cbs.Data["content_block"].(map[string]any)
	if cb["type"] != "text" {
		t.Errorf("first content block should be text, got %v", cb)
	}

	// Deltas must carry text_delta.
	for _, i := range []int{3, 4} {
		d := events[i].Data["delta"].(map[string]any)
		if d["type"] != "text_delta" {
			t.Errorf("event %d delta type: %v", i, d["type"])
		}
	}

	// message_delta carries usage.
	md := events[6]
	u := md.Data["usage"].(map[string]any)
	if u["input_tokens"].(float64) != 10 || u["output_tokens"].(float64) != 2 {
		t.Errorf("message_delta usage: %v", u)
	}
	if md.Data["delta"].(map[string]any)["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason: %v", md.Data["delta"])
	}
}

func TestStreamBedrockToAnthropicSSE_ToolUse(t *testing.T) {
	stream := buildBedrockStream(t, []struct{ Event, Payload string }{
		{"messageStart", `{"role":"assistant"}`},
		{"contentBlockDelta", `{"delta":{"text":"Let me check"},"contentBlockIndex":0}`},
		{"contentBlockStop", `{"contentBlockIndex":0}`},
		{"contentBlockStart", `{"start":{"toolUse":{"toolUseId":"tu_1","name":"get_weather"}},"contentBlockIndex":1}`},
		{"contentBlockDelta", `{"delta":{"toolUse":{"input":"{\"city\":"}},"contentBlockIndex":1}`},
		{"contentBlockDelta", `{"delta":{"toolUse":{"input":"\"Paris\"}"}},"contentBlockIndex":1}`},
		{"contentBlockStop", `{"contentBlockIndex":1}`},
		{"messageStop", `{"stopReason":"tool_use"}`},
		{"metadata", `{"usage":{"inputTokens":15,"outputTokens":8}}`},
	})

	w := httptest.NewRecorder()
	streamBedrockToAnthropicSSE(w, strings.NewReader(string(stream)), "claude-bedrock", "")
	events := parseSSE(t, w.Body.String())

	// Find the tool_use block_start.
	var tuStart *bedrockSSEEvent
	for i, e := range events {
		if e.Name == "content_block_start" {
			cb := e.Data["content_block"].(map[string]any)
			if cb["type"] == "tool_use" {
				tuStart = &events[i]
				break
			}
		}
	}
	if tuStart == nil {
		t.Fatalf("no tool_use content_block_start found in: %v", eventNames(events))
	}
	cb := tuStart.Data["content_block"].(map[string]any)
	if cb["id"] != "tu_1" || cb["name"] != "get_weather" {
		t.Errorf("tool_use block: %v", cb)
	}

	// Confirm input_json_delta events follow.
	gotJSONDelta := false
	for _, e := range events {
		if e.Name == "content_block_delta" {
			d := e.Data["delta"].(map[string]any)
			if d["type"] == "input_json_delta" {
				gotJSONDelta = true
				break
			}
		}
	}
	if !gotJSONDelta {
		t.Errorf("no input_json_delta event emitted; events: %v", eventNames(events))
	}

	// Final message_delta carries tool_use stop_reason.
	last := events[len(events)-2] // message_delta is second-to-last
	if last.Name != "message_delta" {
		t.Fatalf("expected message_delta near end, got %v", last.Name)
	}
	if last.Data["delta"].(map[string]any)["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason: %v", last.Data["delta"])
	}
}

func TestStreamBedrockToAnthropicSSE_Reasoning(t *testing.T) {
	stream := buildBedrockStream(t, []struct{ Event, Payload string }{
		{"messageStart", `{"role":"assistant"}`},
		{"contentBlockDelta", `{"delta":{"reasoningContent":{"text":"Thinking..."}},"contentBlockIndex":0}`},
		{"contentBlockDelta", `{"delta":{"reasoningContent":{"signature":"sig"}},"contentBlockIndex":0}`},
		{"contentBlockStop", `{"contentBlockIndex":0}`},
		{"contentBlockDelta", `{"delta":{"text":"42"},"contentBlockIndex":1}`},
		{"contentBlockStop", `{"contentBlockIndex":1}`},
		{"messageStop", `{"stopReason":"end_turn"}`},
		{"metadata", `{"usage":{"inputTokens":5,"outputTokens":3}}`},
	})

	w := httptest.NewRecorder()
	streamBedrockToAnthropicSSE(w, strings.NewReader(string(stream)), "claude", "")
	events := parseSSE(t, w.Body.String())

	// Find the thinking block_start (the first content_block_start after the message_start/ping).
	thinkingFound := false
	for _, e := range events {
		if e.Name == "content_block_start" {
			cb := e.Data["content_block"].(map[string]any)
			if cb["type"] == "thinking" {
				thinkingFound = true
				break
			}
		}
	}
	if !thinkingFound {
		t.Errorf("no thinking content_block_start; events: %v", eventNames(events))
	}

	// Verify thinking_delta and signature_delta both emitted.
	hasThinkingDelta := false
	hasSigDelta := false
	for _, e := range events {
		if e.Name == "content_block_delta" {
			d := e.Data["delta"].(map[string]any)
			if d["type"] == "thinking_delta" {
				hasThinkingDelta = true
			}
			if d["type"] == "signature_delta" {
				hasSigDelta = true
			}
		}
	}
	if !hasThinkingDelta {
		t.Errorf("missing thinking_delta")
	}
	if !hasSigDelta {
		t.Errorf("missing signature_delta")
	}
}

func TestStreamBedrockToAnthropicSSE_DefensiveBlockClose(t *testing.T) {
	// Bedrock omits contentBlockStop for an open block; bridge must close it
	// so the client doesn't see a dangling content_block_start.
	stream := buildBedrockStream(t, []struct{ Event, Payload string }{
		{"messageStart", `{"role":"assistant"}`},
		{"contentBlockDelta", `{"delta":{"text":"hi"},"contentBlockIndex":0}`},
		{"messageStop", `{"stopReason":"end_turn"}`},
		{"metadata", `{"usage":{"inputTokens":1,"outputTokens":1}}`},
	})

	w := httptest.NewRecorder()
	streamBedrockToAnthropicSSE(w, strings.NewReader(string(stream)), "claude", "")
	events := parseSSE(t, w.Body.String())

	// Count starts vs stops.
	starts, stops := 0, 0
	for _, e := range events {
		if e.Name == "content_block_start" {
			starts++
		}
		if e.Name == "content_block_stop" {
			stops++
		}
	}
	if starts != stops {
		t.Errorf("unbalanced content_block_start (%d) / stop (%d); events: %v", starts, stops, eventNames(events))
	}
}

func TestStreamBedrockToAnthropicSSE_NoEventsErrorPath(t *testing.T) {
	w := httptest.NewRecorder()
	streamBedrockToAnthropicSSE(w, strings.NewReader(""), "claude", "")
	body := w.Body.String()
	if !strings.Contains(body, "event: error") {
		t.Errorf("expected SSE error event for empty stream, got: %q", body)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestStreamBedrockToAnthropicSSE_ExceptionTypeNotLeakedToClient(t *testing.T) {
	// Bedrock sends an event-stream exception frame carrying :exception-type.
	// The stream bridge must map to our fixed vocabulary and must NOT echo
	// the upstream type name to the client.
	var buf bytes.Buffer
	buf.Write(buildEventFrame(t, map[string]string{
		":message-type":   "exception",
		":exception-type": "throttlingException",
	}, []byte(`{"message":"Rate exceeded for arn:aws:bedrock:us-east-2:123456789012:model/claude"}`)))

	w := httptest.NewRecorder()
	streamBedrockToAnthropicSSE(w, bytes.NewReader(buf.Bytes()), "claude", "req-123")
	body := w.Body.String()

	forbidden := []string{"throttlingException", "arn:aws", "123456789012"}
	for _, f := range forbidden {
		if strings.Contains(body, f) {
			t.Errorf("SSE payload leaks %q: %s", f, body)
		}
	}
	if !strings.Contains(body, "overloaded_error") {
		t.Errorf("expected overloaded_error category in SSE payload, got: %s", body)
	}
}

func TestStreamBedrockToAnthropicSSE_CacheMetrics(t *testing.T) {
	stream := buildBedrockStream(t, []struct{ Event, Payload string }{
		{"messageStart", `{"role":"assistant"}`},
		{"contentBlockDelta", `{"delta":{"text":"hi"},"contentBlockIndex":0}`},
		{"messageStop", `{"stopReason":"end_turn"}`},
		{"metadata", `{"usage":{"inputTokens":10,"outputTokens":2,"cacheReadInputTokens":7,"cacheWriteInputTokens":3}}`},
	})
	w := httptest.NewRecorder()
	streamBedrockToAnthropicSSE(w, strings.NewReader(string(stream)), "claude", "")
	events := parseSSE(t, w.Body.String())

	var md bedrockSSEEvent
	for _, e := range events {
		if e.Name == "message_delta" {
			md = e
		}
	}
	u, _ := md.Data["usage"].(map[string]any)
	if u == nil {
		t.Fatalf("no usage in message_delta: %v", md)
	}
	if u["cache_read_input_tokens"].(float64) != 7 {
		t.Errorf("cache_read_input_tokens: %v", u["cache_read_input_tokens"])
	}
	if u["cache_creation_input_tokens"].(float64) != 3 {
		t.Errorf("cache_creation_input_tokens: %v", u["cache_creation_input_tokens"])
	}
}
