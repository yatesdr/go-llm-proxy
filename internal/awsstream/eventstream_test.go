package awsstream

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"strings"
	"testing"
)

// encodeFrame builds a single AWS event-stream frame from headers (string
// values only — the only type Bedrock uses) plus a payload. Used by tests to
// produce well-formed input.
func encodeFrame(headers map[string]string, payload []byte) []byte {
	// Headers section: name_len(1) | name | type(1) | value_len(2) | value
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
	preludeCRC := crc32.ChecksumIEEE(prelude)
	binary.Write(&out, binary.BigEndian, preludeCRC)
	out.Write(headerBytes)
	out.Write(payload)
	msgCRC := crc32.ChecksumIEEE(out.Bytes())
	binary.Write(&out, binary.BigEndian, msgCRC)
	return out.Bytes()
}

func TestReader_SingleFrame(t *testing.T) {
	frame := encodeFrame(map[string]string{
		":message-type": "event",
		":event-type":   "messageStart",
		":content-type": "application/json",
	}, []byte(`{"role":"assistant"}`))

	r := NewReader(bytes.NewReader(frame))
	msg, err := r.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if msg.MessageType() != "event" {
		t.Errorf("MessageType: %q", msg.MessageType())
	}
	if msg.EventType() != "messageStart" {
		t.Errorf("EventType: %q", msg.EventType())
	}
	if msg.HeaderString(":content-type") != "application/json" {
		t.Errorf("content-type: %q", msg.HeaderString(":content-type"))
	}
	if string(msg.Payload) != `{"role":"assistant"}` {
		t.Errorf("payload: %q", msg.Payload)
	}

	// EOF on next call.
	if _, err := r.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("expected EOF after final frame, got %v", err)
	}
}

func TestReader_MultipleFrames(t *testing.T) {
	var buf bytes.Buffer
	for i, et := range []string{"messageStart", "contentBlockDelta", "messageStop"} {
		buf.Write(encodeFrame(map[string]string{
			":message-type": "event",
			":event-type":   et,
		}, []byte(string(rune('a'+i)))))
	}

	r := NewReader(&buf)
	want := []string{"messageStart", "contentBlockDelta", "messageStop"}
	for i, w := range want {
		msg, err := r.Next()
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if msg.EventType() != w {
			t.Errorf("frame %d: event %q want %q", i, msg.EventType(), w)
		}
	}
	if _, err := r.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("expected EOF, got %v", err)
	}
}

func TestReader_PreludeCRCMismatch(t *testing.T) {
	frame := encodeFrame(map[string]string{":x": "y"}, []byte("p"))
	// Corrupt the prelude CRC.
	frame[8] ^= 0xFF
	_, err := NewReader(bytes.NewReader(frame)).Next()
	if err == nil || !strings.Contains(err.Error(), "prelude CRC") {
		t.Fatalf("expected prelude CRC error, got %v", err)
	}
}

func TestReader_MessageCRCMismatch(t *testing.T) {
	frame := encodeFrame(map[string]string{":x": "y"}, []byte("payload"))
	// Corrupt the trailing message CRC (last 4 bytes).
	frame[len(frame)-1] ^= 0xFF
	_, err := NewReader(bytes.NewReader(frame)).Next()
	if err == nil || !strings.Contains(err.Error(), "message CRC") {
		t.Fatalf("expected message CRC error, got %v", err)
	}
}

func TestReader_TruncatedMidFrame(t *testing.T) {
	frame := encodeFrame(map[string]string{":x": "y"}, []byte("payload"))
	// Truncate the frame after the prelude. ReadFull returns
	// io.ErrUnexpectedEOF when partial data is available.
	r := NewReader(bytes.NewReader(frame[:14]))
	_, err := r.Next()
	if err == nil {
		t.Fatalf("expected error on truncated frame")
	}
}

func TestReader_CleanEOFBeforeAnyFrame(t *testing.T) {
	r := NewReader(bytes.NewReader(nil))
	_, err := r.Next()
	if !errors.Is(err, io.EOF) {
		t.Fatalf("expected io.EOF on empty stream, got %v", err)
	}
}

func TestReader_HeaderValueTypes(t *testing.T) {
	// Hand-build a frame with one of each numeric value type to exercise
	// decodeHeaderValue beyond the string-only encoder above.
	var hb bytes.Buffer
	add := func(name string, valueType byte, val []byte) {
		hb.WriteByte(byte(len(name)))
		hb.WriteString(name)
		hb.WriteByte(valueType)
		hb.Write(val)
	}
	add("b", headerTypeByte, []byte{0x7F})
	add("s", headerTypeShort, []byte{0x01, 0x00})
	add("i", headerTypeInteger, []byte{0x00, 0x00, 0x00, 0x05})
	add("l", headerTypeLong, []byte{0, 0, 0, 0, 0, 0, 0, 0xFF})
	add("t", headerTypeTrue, nil)
	add("f", headerTypeFalse, nil)

	headerBytes := hb.Bytes()
	payload := []byte(`{}`)
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

	msg, err := NewReader(&out).Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if msg.Headers["b"].(int8) != 127 {
		t.Errorf("byte: %v", msg.Headers["b"])
	}
	if msg.Headers["s"].(int16) != 256 {
		t.Errorf("short: %v", msg.Headers["s"])
	}
	if msg.Headers["i"].(int32) != 5 {
		t.Errorf("integer: %v", msg.Headers["i"])
	}
	if msg.Headers["l"].(int64) != 255 {
		t.Errorf("long: %v", msg.Headers["l"])
	}
	if msg.Headers["t"].(bool) != true {
		t.Errorf("true: %v", msg.Headers["t"])
	}
	if msg.Headers["f"].(bool) != false {
		t.Errorf("false: %v", msg.Headers["f"])
	}
}

// TestReader_BedrockShapedSequence simulates a typical Bedrock ConverseStream
// sequence to confirm the decoder produces the events the streaming bridge
// will look for.
func TestReader_BedrockShapedSequence(t *testing.T) {
	frames := []struct {
		event   string
		payload string
	}{
		{"messageStart", `{"role":"assistant"}`},
		{"contentBlockDelta", `{"delta":{"text":"Hello"},"contentBlockIndex":0}`},
		{"contentBlockDelta", `{"delta":{"text":" world"},"contentBlockIndex":0}`},
		{"contentBlockStop", `{"contentBlockIndex":0}`},
		{"messageStop", `{"stopReason":"end_turn"}`},
		{"metadata", `{"usage":{"inputTokens":10,"outputTokens":2,"totalTokens":12},"metrics":{"latencyMs":120}}`},
	}
	var buf bytes.Buffer
	for _, f := range frames {
		buf.Write(encodeFrame(map[string]string{
			":message-type": "event",
			":event-type":   f.event,
			":content-type": "application/json",
		}, []byte(f.payload)))
	}

	r := NewReader(&buf)
	for i, want := range frames {
		msg, err := r.Next()
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if msg.EventType() != want.event {
			t.Errorf("frame %d: event %q want %q", i, msg.EventType(), want.event)
		}
		if string(msg.Payload) != want.payload {
			t.Errorf("frame %d payload: %q want %q", i, msg.Payload, want.payload)
		}
	}
}
