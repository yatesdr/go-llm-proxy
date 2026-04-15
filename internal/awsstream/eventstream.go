// Package awsstream decodes AWS application/vnd.amazon.eventstream messages.
//
// The format is a simple binary framing used by Bedrock ConverseStream (and
// most other AWS streaming APIs):
//
//	[ prelude (12 bytes) ][ headers ][ payload ][ message CRC (4 bytes) ]
//
//	prelude:
//	    total_length    uint32  // entire message including prelude and trailing CRC
//	    headers_length  uint32
//	    prelude_crc     uint32  // CRC32 of the first 8 bytes
//
//	header (repeated until headers_length consumed):
//	    name_length     uint8
//	    name            [name_length]byte (ASCII)
//	    value_type      uint8
//	    value           variable, see decodeHeaderValue
//
// We implement only the header value types Bedrock actually emits (string,
// byte-array). Other types return an error so we'll notice if AWS adds new
// behavior we should support.
//
// Reference: https://docs.aws.amazon.com/AmazonS3/latest/userguide/RESTSelectObjectAppendix.html
// (S3 SelectObjectContent docs are the most readable spec for this format —
// the framing itself is the same across all AWS services that use it.)
package awsstream

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
)

// Message is a single decoded event-stream frame.
type Message struct {
	// Headers maps header name to value. Values are either string or []byte
	// depending on the wire type; callers that care about the type should
	// type-assert.
	Headers map[string]any
	// Payload is the raw message body. For Bedrock events it is JSON.
	Payload []byte
}

// HeaderString returns the named header as a string, or "" if missing /
// not a string value. Convenient for the common case of reading
// :event-type, :message-type, :content-type.
func (m *Message) HeaderString(name string) string {
	v, ok := m.Headers[name]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

// MessageType returns the value of the :message-type header
// ("event", "exception", or "error" per the AWS spec).
func (m *Message) MessageType() string { return m.HeaderString(":message-type") }

// EventType returns the value of the :event-type header
// (e.g. "messageStart", "contentBlockDelta" for Bedrock).
func (m *Message) EventType() string { return m.HeaderString(":event-type") }

// Reader streams AWS event-stream messages from an underlying io.Reader.
// Reader is not safe for concurrent use.
type Reader struct {
	r io.Reader
}

// NewReader wraps r in an event-stream decoder.
func NewReader(r io.Reader) *Reader { return &Reader{r: r} }

// Next reads and returns the next message. It returns io.EOF cleanly when
// the underlying stream ends between messages, and io.ErrUnexpectedEOF if
// it ends mid-message.
func (er *Reader) Next() (*Message, error) {
	prelude := make([]byte, 12)
	if _, err := io.ReadFull(er.r, prelude); err != nil {
		// EOF on the very first byte of a new frame is the clean "stream
		// is finished" signal. Anything else mid-prelude is a truncation.
		if errors.Is(err, io.EOF) {
			return nil, io.EOF
		}
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, io.ErrUnexpectedEOF
		}
		return nil, fmt.Errorf("eventstream: reading prelude: %w", err)
	}

	totalLen := binary.BigEndian.Uint32(prelude[0:4])
	headersLen := binary.BigEndian.Uint32(prelude[4:8])
	preludeCRC := binary.BigEndian.Uint32(prelude[8:12])

	if got := crc32.ChecksumIEEE(prelude[:8]); got != preludeCRC {
		return nil, fmt.Errorf("eventstream: prelude CRC mismatch (got %08x want %08x)", got, preludeCRC)
	}

	if totalLen < 16 {
		return nil, fmt.Errorf("eventstream: total_length %d below minimum frame size", totalLen)
	}
	if headersLen > totalLen-16 {
		return nil, fmt.Errorf("eventstream: headers_length %d exceeds available %d", headersLen, totalLen-16)
	}
	// Sanity bound — Bedrock messages are well under a few MB; reject
	// pathological frames before allocating.
	const maxFrame = 16 * 1024 * 1024
	if totalLen > maxFrame {
		return nil, fmt.Errorf("eventstream: frame size %d exceeds limit %d", totalLen, maxFrame)
	}

	rest := make([]byte, totalLen-12) // headers + payload + 4-byte trailing CRC
	if _, err := io.ReadFull(er.r, rest); err != nil {
		return nil, fmt.Errorf("eventstream: reading frame body: %w", err)
	}

	// Verify the trailing message CRC, which covers prelude + headers + payload.
	msgCRC := binary.BigEndian.Uint32(rest[len(rest)-4:])
	body := rest[:len(rest)-4]
	h := crc32.NewIEEE()
	h.Write(prelude)
	h.Write(body)
	if got := h.Sum32(); got != msgCRC {
		return nil, fmt.Errorf("eventstream: message CRC mismatch (got %08x want %08x)", got, msgCRC)
	}

	headersBytes := body[:headersLen]
	payload := body[headersLen:]

	headers, err := decodeHeaders(headersBytes)
	if err != nil {
		return nil, err
	}

	return &Message{Headers: headers, Payload: payload}, nil
}

func decodeHeaders(b []byte) (map[string]any, error) {
	out := map[string]any{}
	i := 0
	for i < len(b) {
		if i+1 > len(b) {
			return nil, errors.New("eventstream: header truncated at name length")
		}
		nameLen := int(b[i])
		i++
		if i+nameLen > len(b) {
			return nil, errors.New("eventstream: header truncated in name")
		}
		name := string(b[i : i+nameLen])
		i += nameLen
		if i+1 > len(b) {
			return nil, errors.New("eventstream: header truncated at value type")
		}
		valueType := b[i]
		i++
		val, consumed, err := decodeHeaderValue(valueType, b[i:])
		if err != nil {
			return nil, fmt.Errorf("eventstream: header %q: %w", name, err)
		}
		i += consumed
		out[name] = val
	}
	return out, nil
}

// AWS event-stream header value type codes.
const (
	headerTypeTrue      = 0
	headerTypeFalse     = 1
	headerTypeByte      = 2
	headerTypeShort     = 3
	headerTypeInteger   = 4
	headerTypeLong      = 5
	headerTypeByteArray = 6
	headerTypeString    = 7
	headerTypeTimestamp = 8
	headerTypeUUID      = 9
)

func decodeHeaderValue(valueType byte, b []byte) (any, int, error) {
	switch valueType {
	case headerTypeTrue:
		return true, 0, nil
	case headerTypeFalse:
		return false, 0, nil
	case headerTypeString, headerTypeByteArray:
		if len(b) < 2 {
			return nil, 0, errors.New("truncated string/bytes length")
		}
		n := int(binary.BigEndian.Uint16(b[:2]))
		if len(b) < 2+n {
			return nil, 0, errors.New("truncated string/bytes value")
		}
		if valueType == headerTypeString {
			return string(b[2 : 2+n]), 2 + n, nil
		}
		v := make([]byte, n)
		copy(v, b[2:2+n])
		return v, 2 + n, nil
	case headerTypeByte:
		if len(b) < 1 {
			return nil, 0, errors.New("truncated byte value")
		}
		return int8(b[0]), 1, nil
	case headerTypeShort:
		if len(b) < 2 {
			return nil, 0, errors.New("truncated short value")
		}
		return int16(binary.BigEndian.Uint16(b[:2])), 2, nil
	case headerTypeInteger:
		if len(b) < 4 {
			return nil, 0, errors.New("truncated integer value")
		}
		return int32(binary.BigEndian.Uint32(b[:4])), 4, nil
	case headerTypeLong, headerTypeTimestamp:
		if len(b) < 8 {
			return nil, 0, errors.New("truncated long/timestamp value")
		}
		return int64(binary.BigEndian.Uint64(b[:8])), 8, nil
	case headerTypeUUID:
		if len(b) < 16 {
			return nil, 0, errors.New("truncated uuid value")
		}
		v := make([]byte, 16)
		copy(v, b[:16])
		return v, 16, nil
	}
	return nil, 0, fmt.Errorf("unknown header value type %d", valueType)
}
