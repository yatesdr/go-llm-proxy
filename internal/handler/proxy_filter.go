package handler

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"go-llm-proxy/internal/api"
)

// streamChatWithThinkFilter streams a Chat Completions SSE response to the client,
// filtering <think>...</think> tags from content. This is a simple, focused function
// that handles one concern: removing reasoning content from the stream.
//
// It reads chunks from resp.Body, filters think tags from delta.content,
// and writes the (potentially modified) chunks to w.
//
// Returns the accumulated usage data if present in the stream.
func streamChatWithThinkFilter(w http.ResponseWriter, resp *http.Response) *api.ChunkUsage {
	flusher, canFlush := w.(http.Flusher)
	flush := func() {
		if canFlush {
			flusher.Flush()
		}
	}

	// Set streaming headers.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	var usageData *api.ChunkUsage
	var thinkFilter thinkTagFilter

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)

	for scanner.Scan() {
		line := scanner.Text()

		// Pass through non-data lines (comments, empty lines).
		if !strings.HasPrefix(line, "data: ") {
			fmt.Fprintf(w, "%s\n", line)
			flush()
			continue
		}

		data := line[6:]

		// Handle stream end.
		if data == "[DONE]" {
			// Flush any pending content from the think filter.
			for _, seg := range thinkFilter.Flush() {
				if !seg.IsReasoning && seg.Text != "" {
					emitContentChunk(w, seg.Text, flush)
				}
			}
			fmt.Fprintf(w, "data: [DONE]\n\n")
			flush()
			break
		}

		// Parse the chunk.
		var chunk api.ChatChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			// Can't parse - pass through unchanged.
			fmt.Fprintf(w, "%s\n", line)
			flush()
			continue
		}

		// Capture usage data.
		if chunk.Usage != nil {
			usageData = chunk.Usage
		}

		// Filter think tags from content.
		modified := filterChunkContent(&chunk, &thinkFilter)

		// Clear reasoning fields if present.
		if len(chunk.Choices) > 0 {
			delta := &chunk.Choices[0].Delta
			if delta.Reasoning != nil || delta.ReasoningContent != nil {
				delta.Reasoning = nil
				delta.ReasoningContent = nil
				modified = true
			}
		}

		// Emit the chunk.
		if modified {
			// Re-serialize the modified chunk.
			newData, err := json.Marshal(chunk)
			if err != nil {
				// Serialization failed - skip this chunk.
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", newData)
		} else {
			// No modifications - pass through original line.
			fmt.Fprintf(w, "%s\n", line)
		}
		flush()
	}

	return usageData
}

// filterChunkContent filters <think>...</think> tags from the chunk's content.
// Returns true if the content was modified.
func filterChunkContent(chunk *api.ChatChunk, filter *thinkTagFilter) bool {
	if len(chunk.Choices) == 0 {
		return false
	}

	delta := &chunk.Choices[0].Delta
	if delta.Content == nil || *delta.Content == "" {
		return false
	}

	// Process content through the think filter.
	var filtered strings.Builder
	for _, seg := range filter.Process(*delta.Content) {
		if !seg.IsReasoning {
			filtered.WriteString(seg.Text)
		}
	}

	filteredStr := filtered.String()
	if filteredStr == *delta.Content {
		return false // No change.
	}

	// Update the content with filtered version.
	delta.Content = &filteredStr
	return true
}

// emitContentChunk emits a simple content-only chunk.
func emitContentChunk(w http.ResponseWriter, text string, flush func()) {
	chunk := api.ChatChunk{
		Choices: []api.ChunkChoice{{
			Delta: api.ChunkDelta{Content: &text},
		}},
	}
	data, _ := json.Marshal(chunk)
	fmt.Fprintf(w, "data: %s\n\n", data)
	flush()
}

// filterNonStreamingContent filters <think>...</think> tags from a non-streaming
// Chat Completions response. Modifies chatResp in place.
func filterNonStreamingContent(chatResp *api.ChatResponse) {
	if len(chatResp.Choices) == 0 {
		return
	}
	content := chatResp.Choices[0].Message.Content
	if content == nil || *content == "" {
		return
	}
	_, filtered := stripThinkTags(*content)
	chatResp.Choices[0].Message.Content = &filtered
}

// reStreamWithThinkFilter reads an SSE stream from r, filters think tags,
// and writes to w. Used for re-streaming after search when headers are already sent.
func reStreamWithThinkFilter(w http.ResponseWriter, r io.Reader, flusher http.Flusher, canFlush bool) {
	flush := func() {
		if canFlush {
			flusher.Flush()
		}
	}

	var thinkFilter thinkTagFilter

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)

	for scanner.Scan() {
		line := scanner.Text()

		// Pass through non-data lines.
		if !strings.HasPrefix(line, "data: ") {
			fmt.Fprintf(w, "%s\n", line)
			flush()
			continue
		}

		data := line[6:]

		// Handle stream end.
		if data == "[DONE]" {
			// Flush any pending content.
			for _, seg := range thinkFilter.Flush() {
				if !seg.IsReasoning && seg.Text != "" {
					emitContentChunk(w, seg.Text, flush)
				}
			}
			fmt.Fprintf(w, "data: [DONE]\n\n")
			flush()
			break
		}

		// Parse and filter.
		var chunk api.ChatChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			fmt.Fprintf(w, "%s\n", line)
			flush()
			continue
		}

		modified := filterChunkContent(&chunk, &thinkFilter)

		// Clear reasoning fields.
		if len(chunk.Choices) > 0 {
			delta := &chunk.Choices[0].Delta
			if delta.Reasoning != nil || delta.ReasoningContent != nil {
				delta.Reasoning = nil
				delta.ReasoningContent = nil
				modified = true
			}
		}

		if modified {
			newData, err := json.Marshal(chunk)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", newData)
		} else {
			fmt.Fprintf(w, "%s\n", line)
		}
		flush()
	}
}
