package handler

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"go-llm-proxy/internal/api"
	"go-llm-proxy/internal/config"
	"go-llm-proxy/internal/httputil"
)

// msgToolCallState tracks a tool call during Anthropic Messages streaming.
type msgToolCallState struct {
	callID     string
	name       string
	blockIndex int
	args       strings.Builder
}

func (h *MessagesHandler) handleStreaming(w http.ResponseWriter, resp *http.Response, req messagesRequest, model *config.ModelConfig, chatReq map[string]any, requestBytes int64, keyName, keyHash string, startTime time.Time) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		httputil.WriteAnthropicError(w, http.StatusInternalServerError, "api_error", "streaming not supported")
		return
	}

	httputil.SetSecurityHeaders(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	upstreamModel := req.Model
	msgID := api.RandomID("msg_")
	blockIndex := 0
	msgStartEmitted := false
	textBlockOpen := false
	reasoningBlockOpen := false
	var toolCalls []*msgToolCallState
	var usageData *api.ChunkUsage
	var finishReason string
	var responseBytes int64

	emit := func(eventType string, data map[string]any) {
		data["type"] = eventType
		jsonData, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, jsonData)
		flusher.Flush()
	}

	emitMessageStart := func() {
		emit("message_start", map[string]any{
			"message": map[string]any{
				"id":            msgID,
				"type":          "message",
				"role":          "assistant",
				"content":       []any{},
				"model":         upstreamModel,
				"stop_reason":   nil,
				"stop_sequence": nil,
				"usage":         map[string]any{"input_tokens": 0, "output_tokens": 1},
			},
		})
		slog.Debug("emitted message_start", "model", upstreamModel)
		msgStartEmitted = true
	}

	openTextBlock := func() {
		emit("content_block_start", map[string]any{
			"index":         blockIndex,
			"content_block": map[string]any{"type": "text", "text": ""},
		})
		slog.Debug("opened text block", "index", blockIndex)
		textBlockOpen = true
	}

	closeTextBlock := func() {
		if !textBlockOpen {
			return
		}
		emit("content_block_stop", map[string]any{"index": blockIndex})
		slog.Debug("closed text block", "index", blockIndex)
		blockIndex++
		textBlockOpen = false
	}

	openReasoningBlock := func() {
		emit("content_block_start", map[string]any{
			"index":         blockIndex,
			"content_block": map[string]any{"type": "thinking", "thinking": "", "signature": ""},
		})
		slog.Debug("opened thinking block", "index", blockIndex)
		reasoningBlockOpen = true
	}

	closeReasoningBlock := func() {
		if !reasoningBlockOpen {
			return
		}
		// Emit placeholder signature before closing.
		emit("content_block_delta", map[string]any{
			"index": blockIndex,
			"delta": map[string]any{"type": "signature_delta", "signature": "proxy-generated"},
		})
		emit("content_block_stop", map[string]any{"index": blockIndex})
		slog.Debug("closed thinking block", "index", blockIndex)
		blockIndex++
		reasoningBlockOpen = false
	}

	// Determine if search buffering is needed.
	searchEnabled := h.pipeline != nil && h.pipeline.ResolveWebSearchKey(model) != ""

	// bufferedEvent stores tool call events that may need to be replayed or discarded.
	type bufferedEvent struct {
		eventType string
		data      map[string]any
	}
	var toolCallBuffer []bufferedEvent
	toolCallBlockIndexStart := 0 // blockIndex when first tool call was seen

	bufferOrEmit := func(eventType string, data map[string]any) {
		if searchEnabled {
			toolCallBuffer = append(toolCallBuffer, bufferedEvent{eventType, data})
		} else {
			emit(eventType, data)
		}
	}

	closeAllBlocks := func() {
		closeReasoningBlock()
		closeTextBlock()
		for _, tc := range toolCalls {
			if tc != nil {
				bufferOrEmit("content_block_stop", map[string]any{"index": tc.blockIndex})
			}
		}
	}

	var accumulatedContent strings.Builder

	// Read and translate the upstream SSE stream.
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)

	for scanner.Scan() {
		line := scanner.Text()
		responseBytes += int64(len(line)) + 1

		if responseBytes > api.MaxResponseBodySize {
			slog.Error("upstream streaming response exceeded size limit", "model", req.Model)
			break
		}

		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := line[6:]
		if data == "[DONE]" {
			break
		}

		var chunk api.ChatChunk
		if json.Unmarshal([]byte(data), &chunk) != nil {
			slog.Debug("skipped unparseable upstream SSE chunk", "data", data)
			continue
		}

		if chunk.Model != "" {
			upstreamModel = chunk.Model
		}
		if !msgStartEmitted {
			emitMessageStart()
			// Emit ping for keepalive.
			emit("ping", map[string]any{})
		}
		if chunk.Usage != nil {
			usageData = chunk.Usage
		}
		if len(chunk.Choices) == 0 {
			continue
		}

		choice := chunk.Choices[0]
		delta := choice.Delta

		// Reasoning tokens -> thinking block.
		if delta.Reasoning != nil && *delta.Reasoning != "" {
			if !reasoningBlockOpen {
				openReasoningBlock()
			}
			emit("content_block_delta", map[string]any{
				"index": blockIndex,
				"delta": map[string]any{"type": "thinking_delta", "thinking": *delta.Reasoning},
			})
		}

		// Content delta -> text block.
		if delta.Content != nil && *delta.Content != "" {
			if reasoningBlockOpen {
				closeReasoningBlock()
			}
			if !textBlockOpen {
				openTextBlock()
			}
			accumulatedContent.WriteString(*delta.Content)
			emit("content_block_delta", map[string]any{
				"index": blockIndex,
				"delta": map[string]any{"type": "text_delta", "text": *delta.Content},
			})
		}

		// Tool call deltas.
		for _, tc := range delta.ToolCalls {
			if tc.ID != "" {
				// New tool call — close open blocks first.
				closeReasoningBlock()
				closeTextBlock()

				if len(toolCalls) == 0 {
					toolCallBlockIndexStart = blockIndex
				}

				name := ""
				if tc.Function != nil {
					name = tc.Function.Name
				}
				tcs := &msgToolCallState{
					callID:     tc.ID,
					name:       name,
					blockIndex: blockIndex,
				}
				// Grow slice to accommodate index.
				for len(toolCalls) <= tc.Index {
					toolCalls = append(toolCalls, nil)
				}
				toolCalls[tc.Index] = tcs

				bufferOrEmit("content_block_start", map[string]any{
					"index": blockIndex,
					"content_block": map[string]any{
						"type":  "tool_use",
						"id":    tc.ID,
						"name":  name,
						"input": map[string]any{},
					},
				})
				slog.Debug("opened tool_use block", "index", blockIndex, "name", name)
				blockIndex++
			}

			if tc.Function != nil && tc.Function.Arguments != "" {
				if tc.Index < len(toolCalls) && toolCalls[tc.Index] != nil {
					tcs := toolCalls[tc.Index]
					tcs.args.WriteString(tc.Function.Arguments)
					bufferOrEmit("content_block_delta", map[string]any{
						"index": tcs.blockIndex,
						"delta": map[string]any{
							"type":         "input_json_delta",
							"partial_json": tc.Function.Arguments,
						},
					})
				}
			}
		}

		// Finish reason.
		if choice.FinishReason != nil {
			finishReason = *choice.FinishReason
		}
	}

	// Check for streaming search loop: if search is enabled, finish_reason is tool_calls,
	// and all tool calls are web_search, execute the search and re-stream from backend.
	if searchEnabled && finishReason == "tool_calls" && len(toolCalls) > 0 {
		allSearch := true
		for _, tc := range toolCalls {
			if tc != nil && tc.name != "web_search" {
				allSearch = false
				break
			}
		}

		if allSearch {
			// Build chatChoiceToolCalls from accumulated state.
			var searchCalls []api.ChatChoiceToolCall
			for _, tc := range toolCalls {
				if tc == nil {
					continue
				}
				searchCalls = append(searchCalls, api.ChatChoiceToolCall{
					ID:   tc.callID,
					Type: "function",
					Function: struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					}{Name: tc.name, Arguments: tc.args.String()},
				})
			}

			ctx := resp.Request.Context()

			// Emit keepalive comments during search execution.
			searchDone := make(chan struct{})
			var newChatReq map[string]any
			var searchErr error

			go func() {
				defer close(searchDone)
				newChatReq, searchErr = h.pipeline.ExecuteSearchAndResend(
					ctx, chatReq, model, searchCalls, accumulatedContent.String())
			}()

			ticker := time.NewTicker(2 * time.Second)
			defer ticker.Stop()
		searchWait:
			for {
				select {
				case <-searchDone:
					break searchWait
				case <-ticker.C:
					fmt.Fprintf(w, ": searching\n\n")
					flusher.Flush()
				case <-ctx.Done():
					break searchWait
				}
			}

			if searchErr != nil {
				slog.Warn("streaming search execution failed", "error", searchErr)
			} else if newChatReq != nil {
				// Reset tool call state, re-stream from backend.
				blockIndex = toolCallBlockIndexStart
				toolCalls = nil
				toolCallBuffer = nil

				newFinish, newUsage, newTC := h.streamFromBackend(ctx, w, flusher, newChatReq, model,
					blockIndex, &textBlockOpen, openTextBlock, closeTextBlock, emit)
				if newFinish != "" {
					finishReason = newFinish
				}
				if newUsage != nil {
					usageData = newUsage
				}
				toolCalls = newTC
				// Update blockIndex past any new blocks opened.
				for _, tc := range toolCalls {
					if tc != nil && tc.blockIndex >= blockIndex {
						blockIndex = tc.blockIndex + 1
					}
				}
			}

			// Fall through to normal terminal event emission.
		} else {
			// Mixed or no-search: replay buffered events and close.
			for _, ev := range toolCallBuffer {
				emit(ev.eventType, ev.data)
			}
			closeAllBlocks()
			toolCalls = nil // Already closed — prevent terminal double-close.
		}
	} else {
		// Not a search case: replay any buffered events and close.
		for _, ev := range toolCallBuffer {
			emit(ev.eventType, ev.data)
		}
		closeAllBlocks()
		toolCalls = nil // Already closed — prevent terminal double-close.
	}

	// Emit terminal events.
	if msgStartEmitted {
		if finishReason == "" {
			finishReason = "stop"
		}

		// Close any open text block (idempotent — checks textBlockOpen).
		closeTextBlock()

		// Close tool call blocks from the re-stream path only.
		// Blocks from the original stream were already closed by closeAllBlocks/replay.
		for _, tc := range toolCalls {
			if tc != nil {
				emit("content_block_stop", map[string]any{"index": tc.blockIndex})
			}
		}

		stopReason := mapFinishToStopReason(finishReason)

		var outputTokens int
		if usageData != nil {
			outputTokens = usageData.CompletionTokens
		}

		emit("message_delta", map[string]any{
			"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
			"usage": map[string]any{"output_tokens": outputTokens},
		})

		emit("message_stop", map[string]any{})

		slog.Debug("stream complete", "stop_reason", stopReason, "blocks", blockIndex)
	} else {
		slog.Error("streaming handler received no valid chunks from upstream",
			"model", req.Model, "response_bytes", responseBytes)
	}

	h.logUsage(usageData, resp.StatusCode, req.Model, requestBytes, responseBytes, keyName, keyHash, startTime)
}

// streamFromBackend sends a streaming Chat Completions request and translates the SSE
// chunks into Anthropic Messages content blocks (text and tool_use), emitting them via
// the provided emit function. This is used for the search re-stream path so the
// streaming parser logic exists in exactly one place.
//
// Returns the finish_reason, final usage, and any tool calls accumulated during the stream.
func (h *MessagesHandler) streamFromBackend(
	ctx context.Context, w http.ResponseWriter, flusher http.Flusher,
	chatReq map[string]any, model *config.ModelConfig,
	startBlockIndex int, textBlockOpen *bool,
	openTextBlock func(), closeTextBlock func(),
	emit func(string, map[string]any),
) (finishReason string, usageData *api.ChunkUsage, toolCalls []*msgToolCallState) {

	chatReq["stream"] = true
	chatReq["stream_options"] = map[string]any{"include_usage": true}
	newBody, err := json.Marshal(chatReq)
	if err != nil {
		slog.Error("streaming search: failed to marshal re-send request", "error", err)
		return
	}

	upstreamURL := strings.TrimRight(model.Backend, "/") + api.ChatCompletionsPath
	upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(newBody))
	if err != nil {
		slog.Error("streaming search: failed to build re-send request", "error", err)
		return
	}
	upReq.Header.Set("Content-Type", "application/json")
	upReq.Header.Set("Accept", "text/event-stream")
	if model.APIKey != "" {
		upReq.Header.Set("Authorization", "Bearer "+model.APIKey)
	}

	resp, err := h.client.Do(upReq)
	if err != nil {
		slog.Error("streaming search: re-send request failed", "error", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		slog.Error("streaming search: backend returned error on re-send",
			"status", resp.StatusCode, "body", string(errBody))
		return
	}

	blockIndex := startBlockIndex
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := line[6:]
		if data == "[DONE]" {
			break
		}
		var chunk api.ChatChunk
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		if chunk.Usage != nil {
			usageData = chunk.Usage
		}
		if len(chunk.Choices) == 0 {
			continue
		}

		choice := chunk.Choices[0]
		delta := choice.Delta

		if delta.Content != nil && *delta.Content != "" {
			if !*textBlockOpen {
				openTextBlock()
			}
			emit("content_block_delta", map[string]any{
				"index": blockIndex,
				"delta": map[string]any{"type": "text_delta", "text": *delta.Content},
			})
		}

		for _, tc := range delta.ToolCalls {
			if tc.ID != "" {
				closeTextBlock()
				name := ""
				if tc.Function != nil {
					name = tc.Function.Name
				}
				tcs := &msgToolCallState{
					callID:     tc.ID,
					name:       name,
					blockIndex: blockIndex,
				}
				for len(toolCalls) <= tc.Index {
					toolCalls = append(toolCalls, nil)
				}
				toolCalls[tc.Index] = tcs
				emit("content_block_start", map[string]any{
					"index": blockIndex,
					"content_block": map[string]any{
						"type":  "tool_use",
						"id":    tc.ID,
						"name":  name,
						"input": map[string]any{},
					},
				})
				blockIndex++
			}
			if tc.Function != nil && tc.Function.Arguments != "" {
				if tc.Index < len(toolCalls) && toolCalls[tc.Index] != nil {
					tcs := toolCalls[tc.Index]
					tcs.args.WriteString(tc.Function.Arguments)
					emit("content_block_delta", map[string]any{
						"index": tcs.blockIndex,
						"delta": map[string]any{
							"type":         "input_json_delta",
							"partial_json": tc.Function.Arguments,
						},
					})
				}
			}
		}

		if choice.FinishReason != nil {
			finishReason = *choice.FinishReason
		}
	}
	return
}
