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
	"go-llm-proxy/internal/pipeline"
)

// toolCallState tracks an in-progress tool call during streaming.
type toolCallState struct {
	itemID    string
	callID    string
	name      string
	args      strings.Builder
	outputIdx int
}

func (h *ResponsesHandler) handleStreaming(w http.ResponseWriter, resp *http.Response, req responsesRequest, model *config.ModelConfig, chatReq map[string]any, requestBytes int64, keyName, keyHash string, startTime time.Time, headersAlreadySent bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		httputil.WriteError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	upstreamCT := resp.Header.Get("Content-Type")
	slog.Debug("streaming handler entered",
		"model", req.Model, "upstream_status", resp.StatusCode, "upstream_content_type", upstreamCT)

	if !headersAlreadySent {
		httputil.SetSecurityHeaders(w)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
	}

	respID := api.RandomID("resp_")
	now := float64(time.Now().Unix())
	seq := 0
	outputIdx := 0
	upstreamModel := req.Model

	// Reasoning accumulation state.
	reasoningID := ""
	var reasoningBuf strings.Builder
	reasoningStarted := false

	// Think-tag filter: strips <think>...</think> from content and routes to reasoning.
	var thinkFilter thinkTagFilter

	// Message accumulation state.
	msgID := ""
	var textBuf strings.Builder
	msgStarted := false
	contentStarted := false

	// Tool call accumulation state (indexed by Chat Completions tool_call index).
	var toolCalls []*toolCallState

	// Final output items for the response.completed event.
	var outputItems []any
	var finishReason string
	var usageData *api.ChunkUsage
	createdEmitted := false

	// Search buffering: when pipeline search is enabled, buffer tool call events
	// AND content that appears before tool calls. This prevents "transition text"
	// like "Good data. Let me search..." from being shown before a search.
	searchEnabled := h.pipeline != nil && h.pipeline.ResolveWebSearchKey(model) != ""
	type bufferedEvent struct {
		eventType string
		data      map[string]any
	}
	var toolCallBuffer []bufferedEvent
	var contentBuffer strings.Builder // Buffer content when search enabled
	contentBuffering := false         // True when we're buffering content before potential search
	outputIdxBeforeTools := 0

	emit := func(event string, data map[string]any) {
		// The Responses API requires a "type" field in every SSE JSON payload
		// that matches the SSE event name.
		data["type"] = event
		jsonData, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, jsonData)
		flusher.Flush()
	}

	emitCreated := func() {
		emit("response.created", map[string]any{
			"response": map[string]any{
				"id":         respID,
				"object":     "response",
				"created_at": now,
				"model":      upstreamModel,
				"status":     "in_progress",
				"output":     []any{},
			},
			"sequence_number": seq,
		})
		seq++
		createdEmitted = true
	}

	startMsg := func() {
		msgID = api.RandomID("msg_")
		emit("response.output_item.added", map[string]any{
			"item": map[string]any{
				"id":      msgID,
				"type":    "message",
				"role":    "assistant",
				"status":  "in_progress",
				"content": []any{},
			},
			"output_index":    outputIdx,
			"sequence_number": seq,
		})
		seq++
		msgStarted = true
	}

	startContent := func() {
		emit("response.content_part.added", map[string]any{
			"part": map[string]any{
				"type":        "output_text",
				"text":        "",
				"annotations": []any{},
			},
			"content_index":   0,
			"output_index":    outputIdx,
			"item_id":         msgID,
			"sequence_number": seq,
		})
		seq++
		contentStarted = true
	}

	finishMsg := func() {
		if !msgStarted {
			return
		}
		text := textBuf.String()

		if contentStarted {
			emit("response.output_text.done", map[string]any{
				"text":            text,
				"content_index":   0,
				"output_index":    outputIdx,
				"item_id":         msgID,
				"sequence_number": seq,
			})
			seq++
			emit("response.content_part.done", map[string]any{
				"part": map[string]any{
					"type":        "output_text",
					"text":        text,
					"annotations": []any{},
				},
				"content_index":   0,
				"output_index":    outputIdx,
				"item_id":         msgID,
				"sequence_number": seq,
			})
			seq++
		}

		item := map[string]any{
			"id":     msgID,
			"type":   "message",
			"role":   "assistant",
			"status": "completed",
			"content": []any{map[string]any{
				"type":        "output_text",
				"text":        text,
				"annotations": []any{},
			}},
		}
		emit("response.output_item.done", map[string]any{
			"item":            item,
			"output_index":    outputIdx,
			"sequence_number": seq,
		})
		seq++

		outputItems = append(outputItems, item)
		outputIdx++
		msgStarted = false
		contentStarted = false
	}

	finishToolCalls := func() {
		for _, tc := range toolCalls {
			if tc == nil {
				continue
			}
			args := tc.args.String()
			emit("response.function_call_arguments.done", map[string]any{
				"arguments":       args,
				"item_id":         tc.itemID,
				"output_index":    tc.outputIdx,
				"sequence_number": seq,
			})
			seq++

			item := map[string]any{
				"id":        tc.itemID,
				"type":      "function_call",
				"call_id":   tc.callID,
				"name":      tc.name,
				"arguments": args,
				"status":    "completed",
			}
			emit("response.output_item.done", map[string]any{
				"item":            item,
				"output_index":    tc.outputIdx,
				"sequence_number": seq,
			})
			seq++
			outputItems = append(outputItems, item)
		}
	}

	finishReasoning := func() {
		if !reasoningStarted {
			return
		}
		text := reasoningBuf.String()
		item := map[string]any{
			"id":   reasoningID,
			"type": "reasoning",
			"summary": []any{map[string]any{
				"type": "summary_text",
				"text": text,
			}},
		}
		emit("response.output_item.done", map[string]any{
			"item":            item,
			"output_index":    outputIdx,
			"sequence_number": seq,
		})
		seq++
		outputItems = append(outputItems, item)
		outputIdx++
		reasoningStarted = false
	}

	// emitReasoningText emits a reasoning text segment (used by both the
	// reasoning JSON field path and think-tag filter).
	emitReasoningText := func(text string) {
		if !reasoningStarted {
			reasoningID = api.RandomID("rs_")
			emit("response.output_item.added", map[string]any{
				"item": map[string]any{
					"id":      reasoningID,
					"type":    "reasoning",
					"summary": []any{},
				},
				"output_index":    outputIdx,
				"sequence_number": seq,
			})
			seq++
			emit("response.reasoning_summary_part.added", map[string]any{
				"item_id":         reasoningID,
				"output_index":    outputIdx,
				"summary_index":   0,
				"sequence_number": seq,
			})
			seq++
			reasoningStarted = true
		}
		reasoningBuf.WriteString(text)
		emit("response.reasoning_summary_text.delta", map[string]any{
			"delta":           text,
			"item_id":         reasoningID,
			"output_index":    outputIdx,
			"summary_index":   0,
			"sequence_number": seq,
		})
		seq++
	}

	// emitContentText emits a content text segment.
	emitContentText := func(text string) {
		if reasoningStarted {
			finishReasoning()
		}
		if !msgStarted {
			startMsg()
		}
		if !contentStarted {
			startContent()
		}
		textBuf.WriteString(text)
		emit("response.output_text.delta", map[string]any{
			"delta":           text,
			"content_index":   0,
			"output_index":    outputIdx,
			"item_id":         msgID,
			"sequence_number": seq,
		})
		seq++
	}

	// Read and translate the upstream SSE stream.
	// Usage is extracted from the parsed chunks, so no response buffering is needed.
	var responseBytes int64
	var rawLines int
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)

	for scanner.Scan() {
		line := scanner.Text()
		responseBytes += int64(len(line)) + 1
		if responseBytes > api.MaxResponseBodySize {
			slog.Error("upstream streaming response exceeded size limit", "model", req.Model, "bytes", responseBytes)
			break
		}
		rawLines++

		// Log the first few lines from the backend at debug level for diagnostics.
		if rawLines <= 3 {
			slog.Debug("upstream SSE line", "line_num", rawLines, "content", line)
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
		if !createdEmitted {
			emitCreated()
		}
		if chunk.Usage != nil {
			usageData = chunk.Usage
		}
		if len(chunk.Choices) == 0 {
			continue
		}

		choice := chunk.Choices[0]
		delta := choice.Delta

		// Note: we do NOT start the message item on the role delta.
		// Reasoning models send role:"assistant" first, then reasoning tokens,
		// then content. Starting the message eagerly would collide with the
		// reasoning item at the same output_index. Instead, startMsg() is
		// called lazily when the first content delta arrives.

		// Reasoning delta — emit as a Responses API reasoning output item.
		// This gives Codex its native reasoning/thinking display.
		// Supports both "reasoning" and "reasoning_content" JSON fields.
		if r := delta.EffectiveReasoning(); r != nil && *r != "" {
			emitReasoningText(*r)
		}

		// Content delta — filter <think>...</think> tags and route to reasoning.
		// When search is enabled, buffer content until we know if it's followed by
		// a search tool call (to suppress "transition text" like "Let me search...").
		if delta.Content != nil && *delta.Content != "" {
			for _, seg := range thinkFilter.Process(*delta.Content) {
				if seg.IsReasoning {
					emitReasoningText(seg.Text)
				} else if searchEnabled && !contentBuffering {
					// Start buffering content when search is enabled
					contentBuffering = true
					contentBuffer.WriteString(seg.Text)
				} else if contentBuffering {
					contentBuffer.WriteString(seg.Text)
				} else {
					emitContentText(seg.Text)
				}
			}
		}

		// Tool call deltas.
		for _, tc := range delta.ToolCalls {
			if tc.ID != "" {
				// New tool call — finish reasoning and message first if open.
				if reasoningStarted {
					finishReasoning()
				}
				if msgStarted {
					finishMsg()
				}

				if len(toolCalls) == 0 {
					outputIdxBeforeTools = outputIdx
				}

				itemID := api.RandomID("fc_")
				name := ""
				if tc.Function != nil {
					name = tc.Function.Name
				}
				tcs := &toolCallState{
					itemID:    itemID,
					callID:    tc.ID,
					name:      name,
					outputIdx: outputIdx,
				}
				// Grow slice to accommodate index.
				for len(toolCalls) <= tc.Index {
					toolCalls = append(toolCalls, nil)
				}
				toolCalls[tc.Index] = tcs

				evData := map[string]any{
					"item": map[string]any{
						"id":        itemID,
						"type":      "function_call",
						"call_id":   tc.ID,
						"name":      name,
						"arguments": "",
						"status":    "in_progress",
					},
					"output_index":    outputIdx,
					"sequence_number": seq,
				}
				if searchEnabled {
					toolCallBuffer = append(toolCallBuffer, bufferedEvent{"response.output_item.added", evData})
				} else {
					emit("response.output_item.added", evData)
				}
				seq++
				outputIdx++
			}

			if tc.Function != nil && tc.Function.Arguments != "" {
				if tc.Index < len(toolCalls) && toolCalls[tc.Index] != nil {
					tcs := toolCalls[tc.Index]
					tcs.args.WriteString(tc.Function.Arguments)
					evData := map[string]any{
						"delta":           tc.Function.Arguments,
						"item_id":         tcs.itemID,
						"output_index":    tcs.outputIdx,
						"sequence_number": seq,
					}
					if searchEnabled {
						toolCallBuffer = append(toolCallBuffer, bufferedEvent{"response.function_call_arguments.delta", evData})
					} else {
						emit("response.function_call_arguments.delta", evData)
					}
					seq++
				}
			}
		}

		// Finish reason.
		if choice.FinishReason != nil {
			finishReason = *choice.FinishReason
		}
	}

	// Handle search loop for streaming responses.
	if searchEnabled && finishReason == "tool_calls" && len(toolCalls) > 0 {
		allSearch := true
		for _, tc := range toolCalls {
			if tc != nil && tc.name != "web_search" {
				allSearch = false
				break
			}
		}

		if allSearch {
			// Discard buffered content - it's transition text like "Let me search..."
			contentBuffer.Reset()
			contentBuffering = false

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

			// Execute search with keepalives.
			searchDone := make(chan struct{})
			var newChatReq map[string]any
			var searchErr error

			var searchResults []pipeline.SearchCallResult
			go func() {
				defer close(searchDone)
				newChatReq, searchResults, searchErr = h.pipeline.ExecuteSearchAndResend(
					ctx, chatReq, model, searchCalls, textBuf.String())
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
				// Emit web_search_call output items so Codex shows native search UI.
				for _, sr := range searchResults {
					if sr.Error != "" {
						continue
					}
					wsID := api.RandomID("ws_")
					wsItem := map[string]any{
						"id":     wsID,
						"type":   "web_search_call",
						"status": "completed",
					}
					if sr.Query != "" {
						wsItem["action"] = map[string]any{
							"type":  "search",
							"query": sr.Query,
						}
					}
					// Include sources - required for Codex to display search results properly.
					if len(sr.Hits) > 0 {
						sources := make([]map[string]any, len(sr.Hits))
						for i, hit := range sr.Hits {
							sources[i] = map[string]any{
								"type":  "url_citation",
								"url":   hit.URL,
								"title": hit.Title,
							}
						}
						wsItem["sources"] = sources
					}
					emit("response.output_item.done", map[string]any{
						"item":            wsItem,
						"output_index":    outputIdxBeforeTools,
						"sequence_number": seq,
					})
					seq++
					outputItems = append(outputItems, wsItem)
					outputIdxBeforeTools++
				}

				// Reset state for re-stream.
				outputIdx = outputIdxBeforeTools
				toolCalls = nil
				toolCallBuffer = nil
				msgStarted = false
				contentStarted = false
				textBuf.Reset()

				// Search loop: keep executing searches until model stops or max iterations.
				const maxSearchIterations = 10
				currentChatReq := newChatReq
				for searchIter := 0; searchIter < maxSearchIterations; searchIter++ {
					newFinish, newUsage, newTC, newSeq := h.streamResponsesFromBackend(
						ctx, currentChatReq, model, emit,
						&outputIdx, &seq, &msgID, &msgStarted, &contentStarted, &textBuf,
						startMsg, startContent, finishMsg,
						emitReasoningText, emitContentText, &thinkFilter)
					if newFinish != "" {
						finishReason = newFinish
					}
					if newUsage != nil {
						usageData = newUsage
					}
					toolCalls = newTC
					seq = newSeq

					// Check if continuation has only web_search tool calls.
					if finishReason != "tool_calls" || len(newTC) == 0 {
						break // No more tool calls, done.
					}

					allSearchContinuation := true
					for _, tc := range newTC {
						if tc != nil && tc.name != "web_search" {
							allSearchContinuation = false
							break
						}
					}
					if !allSearchContinuation {
						break // Mixed tools, pass to client.
					}

					// Build search calls from tool call state.
					var continuationCalls []api.ChatChoiceToolCall
					for _, tc := range newTC {
						if tc == nil {
							continue
						}
						continuationCalls = append(continuationCalls, api.ChatChoiceToolCall{
							ID:   tc.callID,
							Type: "function",
							Function: struct {
								Name      string `json:"name"`
								Arguments string `json:"arguments"`
							}{Name: tc.name, Arguments: tc.args.String()},
						})
					}

					// Execute searches.
					slog.Debug("streaming search continuation", "iteration", searchIter+1, "calls", len(continuationCalls))
					nextReq, contResults, contErr := h.pipeline.ExecuteSearchAndResend(
						ctx, currentChatReq, model, continuationCalls, textBuf.String())
					if contErr != nil {
						slog.Warn("streaming search continuation failed", "error", contErr)
						break
					}

					// Emit web_search_call items for the continuation searches.
					for _, sr := range contResults {
						if sr.Error != "" {
							continue
						}
						wsID := api.RandomID("ws_")
						wsItem := map[string]any{
							"id":     wsID,
							"type":   "web_search_call",
							"status": "completed",
						}
						if sr.Query != "" {
							wsItem["action"] = map[string]any{
								"type":  "search",
								"query": sr.Query,
							}
						}
						if len(sr.Hits) > 0 {
							sources := make([]map[string]any, len(sr.Hits))
							for i, hit := range sr.Hits {
								sources[i] = map[string]any{
									"type":  "url_citation",
									"url":   hit.URL,
									"title": hit.Title,
								}
							}
							wsItem["sources"] = sources
						}
						emit("response.output_item.done", map[string]any{
							"item":            wsItem,
							"output_index":    outputIdx,
							"sequence_number": seq,
						})
						seq++
						outputItems = append(outputItems, wsItem)
						outputIdx++
					}

					// Reset for next iteration.
					currentChatReq = nextReq
					toolCalls = nil
					msgStarted = false
					contentStarted = false
					textBuf.Reset()
				}

				// If we exited with pending all-search tool calls (hit max iterations),
				// clear them - they can't be executed and we didn't emit their events.
				if toolCalls != nil {
					allPendingSearch := true
					for _, tc := range toolCalls {
						if tc != nil && tc.name != "web_search" {
							allPendingSearch = false
							break
						}
					}
					if allPendingSearch {
						slog.Debug("search loop hit max iterations, discarding pending search calls",
							"pending", len(toolCalls))
						toolCalls = nil
					}
				}
			}
		} else {
			// Mixed or no-search: emit buffered content and replay buffered events.
			if contentBuffer.Len() > 0 {
				emitContentText(contentBuffer.String())
				contentBuffer.Reset()
			}
			contentBuffering = false
			for _, ev := range toolCallBuffer {
				emit(ev.eventType, ev.data)
			}
		}
	} else {
		// No search case: emit buffered content and replay any buffered events.
		if contentBuffer.Len() > 0 {
			emitContentText(contentBuffer.String())
			contentBuffer.Reset()
		}
		contentBuffering = false
		for _, ev := range toolCallBuffer {
			emit(ev.eventType, ev.data)
		}
	}

	// Flush any pending think-tag buffer before finalizing.
	for _, seg := range thinkFilter.Flush() {
		if seg.IsReasoning {
			emitReasoningText(seg.Text)
		} else {
			emitContentText(seg.Text)
		}
	}

	// Finalize pending items.
	if reasoningStarted {
		finishReasoning()
	}
	if msgStarted {
		finishMsg()
	}
	finishToolCalls()

	// Emit the terminal event.
	if !createdEmitted {
		slog.Error("streaming handler received no valid chunks from upstream",
			"model", req.Model, "response_bytes", responseBytes,
			"scanner_error", scanner.Err())
	}
	if createdEmitted {
		if finishReason == "" {
			finishReason = "stop"
		}

		status := "completed"
		eventName := "response.completed"
		var incompleteDetails any

		switch finishReason {
		case "length":
			status = "incomplete"
			eventName = "response.incomplete"
			incompleteDetails = map[string]any{"reason": "max_output_tokens"}
		case "content_filter":
			status = "failed"
			eventName = "response.failed"
		}

		var usageObj any
		if usageData != nil {
			usageObj = map[string]any{
				"input_tokens":          usageData.PromptTokens,
				"input_tokens_details":  nil,
				"output_tokens":         usageData.CompletionTokens,
				"output_tokens_details": nil,
				"total_tokens":          usageData.TotalTokens,
			}
		}

		emit(eventName, map[string]any{
			"response": map[string]any{
				"id":                 respID,
				"object":             "response",
				"created_at":         now,
				"model":              upstreamModel,
				"status":             status,
				"output":             outputItems,
				"output_text":        textBuf.String(),
				"usage":              usageObj,
				"incomplete_details": incompleteDetails,
			},
			"sequence_number": seq,
		})
	}

	h.logUsage(usageData, resp.StatusCode, req.Model, requestBytes, responseBytes, keyName, keyHash, startTime)
}

// sendChatRequest sends a Chat Completions request to the model's backend and returns the parsed response.
// streamResponsesFromBackend sends a streaming Chat Completions request and translates
// chunks into Responses API SSE events. Returns the finish_reason, usage, tool calls,
// and updated sequence number.
func (h *ResponsesHandler) streamResponsesFromBackend(
	ctx context.Context, chatReq map[string]any, model *config.ModelConfig,
	emit func(string, map[string]any),
	outputIdx *int, seq *int, msgID *string,
	msgStarted *bool, contentStarted *bool, textBuf *strings.Builder,
	startMsg func(), startContent func(), finishMsg func(),
	emitReasoningText func(string), emitContentText func(string), thinkFilter *thinkTagFilter,
) (finishReason string, usageData *api.ChunkUsage, toolCalls []*toolCallState, finalSeq int) {

	finalSeq = *seq

	chatReq["stream"] = true
	chatReq["stream_options"] = map[string]any{"include_usage": true}
	model.ApplySamplingDefaults(chatReq)
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

	// Content buffering: buffer content until we know if it's followed by search tool calls.
	// This prevents "transition text" like "Good data. Let me search..." from being emitted.
	searchEnabled := h.pipeline != nil && h.pipeline.ResolveWebSearchKey(model) != ""
	var contentBuffer strings.Builder
	type bufferedEvent struct {
		eventType string
		data      map[string]any
	}
	var toolCallBuffer []bufferedEvent

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

		// Handle reasoning_content field (same as main handler).
		if r := delta.EffectiveReasoning(); r != nil && *r != "" {
			emitReasoningText(*r)
		}

		// Handle content with think tag filtering.
		// When search is enabled, buffer content instead of emitting immediately.
		if delta.Content != nil && *delta.Content != "" {
			for _, seg := range thinkFilter.Process(*delta.Content) {
				if seg.IsReasoning {
					emitReasoningText(seg.Text)
				} else if searchEnabled {
					// Buffer content when search is enabled
					contentBuffer.WriteString(seg.Text)
				} else {
					emitContentText(seg.Text)
				}
			}
		}

		for _, tc := range delta.ToolCalls {
			if tc.ID != "" {
				if *msgStarted {
					finishMsg()
				}
				itemID := api.RandomID("fc_")
				name := ""
				if tc.Function != nil {
					name = tc.Function.Name
				}
				tcs := &toolCallState{
					itemID:    itemID,
					callID:    tc.ID,
					name:      name,
					outputIdx: *outputIdx,
				}
				for len(toolCalls) <= tc.Index {
					toolCalls = append(toolCalls, nil)
				}
				toolCalls[tc.Index] = tcs
				evData := map[string]any{
					"item": map[string]any{
						"id":        itemID,
						"type":      "function_call",
						"call_id":   tc.ID,
						"name":      name,
						"arguments": "",
						"status":    "in_progress",
					},
					"output_index":    *outputIdx,
					"sequence_number": *seq,
				}
				if searchEnabled {
					toolCallBuffer = append(toolCallBuffer, bufferedEvent{"response.output_item.added", evData})
				} else {
					emit("response.output_item.added", evData)
				}
				*seq++
				*outputIdx++
			}
			if tc.Function != nil && tc.Function.Arguments != "" {
				if tc.Index < len(toolCalls) && toolCalls[tc.Index] != nil {
					tcs := toolCalls[tc.Index]
					tcs.args.WriteString(tc.Function.Arguments)
					evData := map[string]any{
						"delta":           tc.Function.Arguments,
						"item_id":         tcs.itemID,
						"output_index":    tcs.outputIdx,
						"sequence_number": *seq,
					}
					if searchEnabled {
						toolCallBuffer = append(toolCallBuffer, bufferedEvent{"response.function_call_arguments.delta", evData})
					} else {
						emit("response.function_call_arguments.delta", evData)
					}
					*seq++
				}
			}
		}

		if choice.FinishReason != nil {
			finishReason = *choice.FinishReason
		}
	}

	// Handle buffered content based on whether we got search tool calls.
	if searchEnabled && len(toolCalls) > 0 {
		allSearch := true
		for _, tc := range toolCalls {
			if tc != nil && tc.name != "web_search" {
				allSearch = false
				break
			}
		}
		if allSearch {
			// Discard buffered content - it's transition text like "Let me search..."
			// Don't emit tool call events - the caller will handle them by executing
			// the searches and emitting web_search_call items instead.
			contentBuffer.Reset()
		} else {
			// Mixed tool calls - emit the buffered content and replay tool events.
			if contentBuffer.Len() > 0 {
				emitContentText(contentBuffer.String())
			}
			for _, ev := range toolCallBuffer {
				emit(ev.eventType, ev.data)
			}
		}
	} else if contentBuffer.Len() > 0 {
		// No tool calls - emit buffered content
		emitContentText(contentBuffer.String())
	}

	finalSeq = *seq
	return
}
