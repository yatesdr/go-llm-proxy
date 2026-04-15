package handler

// captureBuffer is a bounded buffer for sampling an upstream response so we
// can extract token-usage metadata without retaining the full body in memory.
//
// Two strategies:
//
//   - Non-streaming: keep the first N bytes. Usage appears near the top of
//     the JSON response.
//
//   - Streaming: keep the first small prefix (model/id metadata sometimes
//     appears in the first chunk) AND a rolling tail window (final SSE
//     chunk carries usage when stream_options.include_usage=true). A full
//     ring would work too; the split prefix+tail keeps the implementation
//     allocation-free after construction.
//
// If the caller decides capture is pointless (e.g. the response already
// exceeded the overall size cap), discard() clears the buffers and
// subsequent bytes() returns nil.

type captureBuffer struct {
	streaming bool

	// Non-streaming / streaming prefix window.
	prefix    []byte
	prefixCap int

	// Streaming tail window. A ring of the last streamTailCaptureBytes
	// bytes written, reconstructed on bytes().
	tail    []byte
	tailPos int  // next write position
	tailLen int  // valid bytes in tail
	tailSaw bool // true once anything has been written

	discarded bool
}

// newCaptureBuffer allocates a buffer sized for the response style.
func newCaptureBuffer(streaming bool) *captureBuffer {
	if streaming {
		return &captureBuffer{
			streaming: true,
			prefixCap: 16 * 1024, // 16 KB is enough to sniff initial headers/model
			prefix:    make([]byte, 0, 16*1024),
			tail:      make([]byte, streamTailCaptureBytes),
		}
	}
	return &captureBuffer{
		prefixCap: maxUsageCaptureBytes,
		prefix:    make([]byte, 0, 4096),
	}
}

// write copies bytes into the prefix buffer until full, then (if streaming)
// into the ring tail. Non-streaming responses silently drop anything past
// the prefix cap.
func (b *captureBuffer) write(p []byte) {
	if b.discarded {
		return
	}
	// Fill prefix first.
	if len(b.prefix) < b.prefixCap {
		room := b.prefixCap - len(b.prefix)
		if room >= len(p) {
			b.prefix = append(b.prefix, p...)
			return
		}
		b.prefix = append(b.prefix, p[:room]...)
		p = p[room:]
	}
	if !b.streaming {
		return
	}
	// Stream remaining into the tail ring.
	b.tailSaw = true
	for _, c := range p {
		b.tail[b.tailPos] = c
		b.tailPos++
		if b.tailPos >= len(b.tail) {
			b.tailPos = 0
		}
		if b.tailLen < len(b.tail) {
			b.tailLen++
		}
	}
}

// bytes returns a contiguous snapshot usable by ExtractTokenUsage. For
// streaming, the returned slice is prefix + a newline + tail (unrolled from
// the ring), which is still valid SSE for the usage parser. Returns nil if
// nothing has been captured or capture was discarded.
func (b *captureBuffer) bytes() []byte {
	if b.discarded {
		return nil
	}
	if !b.streaming {
		if len(b.prefix) == 0 {
			return nil
		}
		return b.prefix
	}
	if len(b.prefix) == 0 && !b.tailSaw {
		return nil
	}
	out := make([]byte, 0, len(b.prefix)+b.tailLen+1)
	out = append(out, b.prefix...)
	if b.tailSaw {
		if len(out) > 0 && out[len(out)-1] != '\n' {
			out = append(out, '\n')
		}
		// Unroll the ring starting from (tailPos - tailLen) mod len.
		start := (b.tailPos + len(b.tail) - b.tailLen) % len(b.tail)
		if start+b.tailLen <= len(b.tail) {
			out = append(out, b.tail[start:start+b.tailLen]...)
		} else {
			out = append(out, b.tail[start:]...)
			out = append(out, b.tail[:b.tailLen-(len(b.tail)-start)]...)
		}
	}
	return out
}

// discard marks the buffer empty, releasing its backing slices.
func (b *captureBuffer) discard() {
	b.discarded = true
	b.prefix = nil
	b.tail = nil
}
