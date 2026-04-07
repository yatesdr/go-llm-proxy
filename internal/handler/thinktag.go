package handler

import "strings"

// thinkTagFilter is a streaming parser that detects <think>...</think> tags
// in content chunks and separates reasoning from content. Some models (e.g.
// MiniMax-M2.7) embed reasoning as literal XML tags in the content field
// rather than using a dedicated reasoning JSON field.
type thinkTagFilter struct {
	inside  bool   // currently inside a <think> block
	pending string // buffered text that might be part of a tag boundary
}

// textSegment represents a piece of text that is either reasoning or content.
type textSegment struct {
	Text        string
	IsReasoning bool
}

const thinkOpen = "<think>"
const thinkClose = "</think>"

// Process takes a content chunk and returns segments classified as reasoning or content.
// Handles tags that may be split across chunk boundaries.
func (f *thinkTagFilter) Process(chunk string) []textSegment {
	input := f.pending + chunk
	f.pending = ""

	var segments []textSegment

	for len(input) > 0 {
		if f.inside {
			idx := strings.Index(input, thinkClose)
			if idx >= 0 {
				if idx > 0 {
					segments = append(segments, textSegment{input[:idx], true})
				}
				input = input[idx+len(thinkClose):]
				f.inside = false
			} else {
				// Check for partial </think> at the end.
				n := longestTagSuffix(input, thinkClose)
				if n > 0 {
					if len(input)-n > 0 {
						segments = append(segments, textSegment{input[:len(input)-n], true})
					}
					f.pending = input[len(input)-n:]
				} else {
					segments = append(segments, textSegment{input, true})
				}
				input = ""
			}
		} else {
			idx := strings.Index(input, thinkOpen)
			if idx >= 0 {
				if idx > 0 {
					segments = append(segments, textSegment{input[:idx], false})
				}
				input = input[idx+len(thinkOpen):]
				f.inside = true
			} else {
				// Check for partial <think> at the end.
				n := longestTagSuffix(input, thinkOpen)
				if n > 0 {
					if len(input)-n > 0 {
						segments = append(segments, textSegment{input[:len(input)-n], false})
					}
					f.pending = input[len(input)-n:]
				} else {
					segments = append(segments, textSegment{input, false})
				}
				input = ""
			}
		}
	}

	return segments
}

// Flush returns any buffered text that was held back waiting for a complete tag.
// Call this when the stream ends.
func (f *thinkTagFilter) Flush() []textSegment {
	if f.pending == "" {
		return nil
	}
	seg := textSegment{Text: f.pending, IsReasoning: f.inside}
	f.pending = ""
	return []textSegment{seg}
}

// longestTagSuffix returns the length of the longest suffix of s that is a
// prefix of tag. This detects partial tags split across chunk boundaries.
func longestTagSuffix(s, tag string) int {
	maxLen := len(tag) - 1
	if maxLen > len(s) {
		maxLen = len(s)
	}
	for l := maxLen; l > 0; l-- {
		if strings.HasSuffix(s, tag[:l]) {
			return l
		}
	}
	return 0
}

// stripThinkTags removes <think>...</think> blocks from a complete string,
// returning (reasoning, content). Used for non-streaming responses.
func stripThinkTags(s string) (reasoning, content string) {
	var reasoningBuf, contentBuf strings.Builder
	for len(s) > 0 {
		openIdx := strings.Index(s, thinkOpen)
		if openIdx < 0 {
			contentBuf.WriteString(s)
			break
		}
		contentBuf.WriteString(s[:openIdx])
		s = s[openIdx+len(thinkOpen):]
		closeIdx := strings.Index(s, thinkClose)
		if closeIdx < 0 {
			// Unclosed <think> — treat rest as reasoning.
			reasoningBuf.WriteString(s)
			break
		}
		reasoningBuf.WriteString(s[:closeIdx])
		s = s[closeIdx+len(thinkClose):]
	}
	return reasoningBuf.String(), strings.TrimSpace(contentBuf.String())
}
