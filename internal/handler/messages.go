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
	"path"
	"strings"
	"sync"
	"time"

	"go-llm-proxy/internal/api"
	"go-llm-proxy/internal/auth"
	"go-llm-proxy/internal/config"
	"go-llm-proxy/internal/httputil"
	"go-llm-proxy/internal/pipeline"
	"go-llm-proxy/internal/usage"
)

// MessagesHandler implements the Anthropic Messages API (POST /v1/messages).
// For backends with type: "anthropic", requests pass through natively.
// For OpenAI-compatible backends, requests are translated to Chat Completions
// format automatically, enabling Claude Code to work with any backend.
type MessagesHandler struct {
	config      *config.ConfigStore
	client      *http.Client
	usage       *usage.UsageLogger
	pipeline    *pipeline.Pipeline
	nativeCache sync.Map
}

func NewMessagesHandler(cs *config.ConfigStore, usage *usage.UsageLogger, pipeline *pipeline.Pipeline) *MessagesHandler {
	return &MessagesHandler{
		config:   cs,
		usage:    usage,
		pipeline: pipeline,
		client:   httputil.NewHTTPClient(),
	}
}

// shouldTranslate returns true if this backend should translate Anthropic
// Messages to Chat Completions.
func (h *MessagesHandler) shouldTranslate(model *config.ModelConfig) bool {
	if model.MessagesMode == config.MessagesModeTranslate {
		return true
	}
	if model.MessagesMode == config.MessagesModeNative {
		return false
	}
	// Auto mode: anthropic backends always passthrough, others always translate.
	// Unlike the Responses API, no OpenAI backend supports /v1/messages, so
	// probing is pointless — we can decide from the config type alone.
	return model.Type != config.BackendAnthropic
}

func (h *MessagesHandler) shouldForceNative(model *config.ModelConfig) bool {
	return model.MessagesMode == config.MessagesModeNative
}

func (h *MessagesHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httputil.WriteAnthropicError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}

	// Handle /anthropic/v1/messages prefix.
	cleanPath := path.Clean(r.URL.Path)
	requireAnthropic := false
	if strings.HasPrefix(cleanPath, "/anthropic/") {
		cleanPath = strings.TrimPrefix(cleanPath, "/anthropic")
		requireAnthropic = true
	}

	r.Body = http.MaxBytesReader(w, r.Body, api.MaxRequestBodySize)
	body, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		httputil.WriteAnthropicError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "request body too large")
		return
	}

	var req messagesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		httputil.WriteAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON request body")
		return
	}
	if req.Model == "" {
		httputil.WriteAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "missing model field")
		return
	}

	cfg := h.config.Get()
	key := auth.KeyFromContext(r.Context())
	if !auth.KeyAllowsModel(key, req.Model) {
		httputil.WriteAnthropicError(w, http.StatusForbidden, "permission_error", "not authorized for requested model")
		return
	}

	model := config.FindModel(cfg, req.Model)
	if model == nil {
		httputil.WriteAnthropicError(w, http.StatusNotFound, "not_found_error", "unknown model")
		return
	}

	if requireAnthropic && model.Type != config.BackendAnthropic {
		httputil.WriteAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "model is not an anthropic backend")
		return
	}

	keyName, keyHash := "", ""
	if key != nil {
		keyName = key.Name
		keyHash = usage.HashKey(key.Key)
	}
	startTime := time.Now()

	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(model.Timeout)*time.Second)
	defer cancel()

	// Native passthrough for anthropic backends or native mode.
	if !h.shouldTranslate(model) {
		h.handleNativePassthrough(ctx, w, r, body, req, model, keyName, keyHash, startTime)
		return
	}

	// Translate Anthropic Messages -> Chat Completions.
	slog.Info("proxying messages request (translated)", "model", req.Model, "key", keyName)

	chatReq, err := buildChatRequestFromAnthropic(req, model.Model)
	if err != nil {
		httputil.WriteAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "translation error: "+err.Error())
		return
	}

	// Run pipeline pre-send processors (vision, PDF, etc.).
	if h.pipeline != nil {
		chatReq, err = h.pipeline.ProcessRequest(ctx, chatReq, model)
		if err != nil {
			httputil.WriteAnthropicError(w, http.StatusInternalServerError, "api_error", "processing error: "+err.Error())
			return
		}
	}

	chatBody, err := json.Marshal(chatReq)
	if err != nil {
		httputil.WriteAnthropicError(w, http.StatusInternalServerError, "api_error", "failed to build upstream request")
		return
	}

	upstreamURL := strings.TrimRight(model.Backend, "/") + api.ChatCompletionsPath

	upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(chatBody))
	if err != nil {
		httputil.WriteAnthropicError(w, http.StatusInternalServerError, "api_error", "failed to create upstream request")
		return
	}

	upReq.Header.Set("Content-Type", "application/json")
	if req.Stream {
		upReq.Header.Set("Accept", "text/event-stream")
	}
	if v := r.Header.Get("X-Request-ID"); v != "" {
		upReq.Header.Set("X-Request-ID", v)
	}
	if model.APIKey != "" {
		upReq.Header.Set("Authorization", "Bearer "+model.APIKey)
	}

	resp, err := h.client.Do(upReq)
	if err != nil {
		if ctx.Err() != nil {
			httputil.WriteAnthropicError(w, http.StatusGatewayTimeout, "api_error", "upstream request timed out")
			return
		}
		slog.Error("upstream request failed", "error", err, "model", req.Model)
		httputil.WriteAnthropicError(w, http.StatusBadGateway, "api_error", "upstream request failed")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, api.MaxResponseBodySize))
		slog.Error("upstream returned error for translated messages request",
			"model", req.Model, "status", resp.StatusCode, "body", string(errBody))

		// Sanitized error: never forward raw backend error bodies to the client.
		// They may contain internal URLs, backend API keys, or infrastructure details.
		errMsg := fmt.Sprintf("backend returned HTTP %d", resp.StatusCode)
		if resp.StatusCode == http.StatusBadRequest && pipeline.RequestContainsImageURLs(chatReq) {
			errMsg = fmt.Sprintf("The backend model (%s) does not appear to support image inputs. "+
				"Remove images from the conversation or use a vision-capable model.", req.Model)
		}

		if req.Stream {
			httputil.SetSecurityHeaders(w)
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.WriteHeader(http.StatusOK)
			errData, _ := json.Marshal(map[string]any{
				"type": "error",
				"error": map[string]any{
					"type":    "api_error",
					"message": errMsg,
				},
			})
			fmt.Fprintf(w, "event: error\ndata: %s\n\n", errData)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			return
		}

		httputil.WriteAnthropicError(w, resp.StatusCode, "api_error", errMsg)
		return
	}

	reqBytes := int64(len(chatBody))
	if req.Stream {
		h.handleStreaming(w, resp, req, model, chatReq, reqBytes, keyName, keyHash, startTime)
	} else {
		h.handleNonStreaming(w, resp, req, model, chatReq, reqBytes, keyName, keyHash, startTime)
	}
}

// --- Native passthrough ---

func (h *MessagesHandler) handleNativePassthrough(ctx context.Context, w http.ResponseWriter, r *http.Request, body []byte, req messagesRequest, model *config.ModelConfig, keyName, keyHash string, startTime time.Time) {
	if model.Model != req.Model {
		body = RewriteModelName(body, model.Model)
	}

	// Build upstream URL: Anthropic backends keep /v1 in path.
	upstreamURL := strings.TrimRight(model.Backend, "/") + "/v1/messages"

	upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(body))
	if err != nil {
		httputil.WriteAnthropicError(w, http.StatusInternalServerError, "api_error", "failed to create upstream request")
		return
	}

	upReq.Header.Set("Content-Type", "application/json")
	if v := r.Header.Get("Accept"); v != "" {
		upReq.Header.Set("Accept", v)
	}
	if v := r.Header.Get("X-Request-ID"); v != "" {
		upReq.Header.Set("X-Request-ID", v)
	}
	// Anthropic auth and protocol headers.
	if model.APIKey != "" {
		upReq.Header.Set("X-Api-Key", model.APIKey)
	}
	for _, h := range []string{"Anthropic-Version", "Anthropic-Beta"} {
		if v := r.Header.Get(h); v != "" {
			upReq.Header.Set(h, v)
		}
	}

	slog.Info("proxying messages request (native)", "model", req.Model, "key", keyName)

	resp, err := h.client.Do(upReq)
	if err != nil {
		if ctx.Err() != nil {
			httputil.WriteAnthropicError(w, http.StatusGatewayTimeout, "api_error", "upstream request timed out")
			return
		}
		slog.Error("upstream request failed", "error", err, "model", req.Model)
		httputil.WriteAnthropicError(w, http.StatusBadGateway, "api_error", "upstream request failed")
		return
	}
	defer resp.Body.Close()

	// For error responses, sanitize before sending to the client.
	if resp.StatusCode >= 400 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, api.MaxResponseBodySize))
		slog.Error("upstream returned error (native passthrough)",
			"model", req.Model, "status", resp.StatusCode, "body", string(errBody))
		httputil.WriteAnthropicError(w, resp.StatusCode, "api_error",
			fmt.Sprintf("backend returned HTTP %d", resp.StatusCode))
		if h.usage != nil {
			rec := usage.UsageRecord{
				Timestamp: startTime, KeyHash: keyHash, KeyName: keyName,
				Model: req.Model, Endpoint: "/v1/messages", StatusCode: resp.StatusCode,
				RequestBytes: int64(len(body)), ResponseBytes: int64(len(errBody)),
				DurationMS: time.Since(startTime).Milliseconds(),
			}
			go h.usage.Log(rec)
		}
		return
	}

	// Forward response headers.
	for k := range AllowedResponseHeaders {
		if v := resp.Header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	httputil.SetSecurityHeaders(w)
	w.WriteHeader(resp.StatusCode)

	flusher, canFlush := w.(http.Flusher)
	buf := make([]byte, 4096)
	var totalBytes int64
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			totalBytes += int64(n)
			if totalBytes > api.MaxResponseBodySize {
				break
			}
			w.Write(buf[:n])
			if canFlush {
				flusher.Flush()
			}
		}
		if readErr != nil {
			break
		}
	}

	if h.usage != nil {
		rec := usage.UsageRecord{
			Timestamp: startTime, KeyHash: keyHash, KeyName: keyName,
			Model: req.Model, Endpoint: "/v1/messages", StatusCode: resp.StatusCode,
			RequestBytes: int64(len(body)), ResponseBytes: totalBytes,
			DurationMS: time.Since(startTime).Milliseconds(),
		}
		go h.usage.Log(rec)
	}
}

// --- Non-streaming handler ---

func (h *MessagesHandler) handleNonStreaming(w http.ResponseWriter, resp *http.Response, req messagesRequest, model *config.ModelConfig, chatReq map[string]any, requestBytes int64, keyName, keyHash string, startTime time.Time) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, api.MaxResponseBodySize))
	if err != nil {
		httputil.WriteAnthropicError(w, http.StatusBadGateway, "api_error", "failed to read upstream response")
		return
	}

	var chatResp api.ChatResponse
	if err := json.Unmarshal(body, &chatResp); err != nil {
		httputil.WriteAnthropicError(w, http.StatusBadGateway, "api_error", "invalid upstream response")
		return
	}

	// Search tool loop: if the response calls web_search, execute and re-send.
	if h.pipeline != nil && len(chatResp.Choices) > 0 && pipeline.HasSearchToolCall(chatResp.Choices[0].Message.ToolCalls) {
		ctx := resp.Request.Context()
		finalResp, err := h.pipeline.HandleNonStreamingSearchLoop(ctx, chatReq, model, &chatResp,
			func(req map[string]any) (*api.ChatResponse, error) {
				return h.sendChatRequest(ctx, req, model)
			}, 5)
		if err != nil {
			httputil.WriteAnthropicError(w, http.StatusBadGateway, "api_error", "search processing error: "+err.Error())
			return
		}
		chatResp = *finalResp
	}

	respID := api.RandomID("msg_")
	var content []any
	stopReason := "end_turn"

	if len(chatResp.Choices) > 0 {
		choice := chatResp.Choices[0]
		msg := choice.Message

		// Reasoning -> thinking block (before text content).
		if msg.Reasoning != nil && *msg.Reasoning != "" {
			content = append(content, map[string]any{
				"type":      "thinking",
				"thinking":  *msg.Reasoning,
				"signature": api.RandomID(""),
			})
		}

		if msg.Content != nil && *msg.Content != "" {
			content = append(content, map[string]any{
				"type": "text",
				"text": *msg.Content,
			})
		}

		for _, tc := range msg.ToolCalls {
			// Parse arguments string back to object for Anthropic format.
			var input json.RawMessage
			if json.Unmarshal([]byte(tc.Function.Arguments), &input) != nil {
				input = json.RawMessage("{}")
			}
			content = append(content, map[string]any{
				"type":  "tool_use",
				"id":    tc.ID,
				"name":  tc.Function.Name,
				"input": input,
			})
		}

		stopReason = mapFinishToStopReason(choice.FinishReason)
	}

	if content == nil {
		content = []any{}
	}

	var usageObj map[string]any
	if chatResp.Usage != nil {
		usageObj = map[string]any{
			"input_tokens":                chatResp.Usage.PromptTokens,
			"output_tokens":              chatResp.Usage.CompletionTokens,
			"cache_creation_input_tokens": 0,
			"cache_read_input_tokens":     0,
		}
	}

	response := map[string]any{
		"id":            respID,
		"type":          "message",
		"role":          "assistant",
		"content":       content,
		"model":         chatResp.Model,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage":         usageObj,
	}

	httputil.SetSecurityHeaders(w)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)

	h.logUsage(chatResp.Usage, resp.StatusCode, req.Model, requestBytes, int64(len(body)), keyName, keyHash, startTime)
}

// --- Streaming handler ---

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

func (h *MessagesHandler) sendChatRequest(ctx context.Context, chatReq map[string]any, model *config.ModelConfig) (*api.ChatResponse, error) {
	return sendChatCompletionsRequest(ctx, h.client, chatReq, model)
}

// --- Usage logging ---

func (h *MessagesHandler) logUsage(usageData *api.ChunkUsage, statusCode int, model string, requestBytes, responseBytes int64, keyName, keyHash string, startTime time.Time) {
	if h.usage == nil {
		return
	}
	var tokens usage.TokenUsage
	if usageData != nil {
		tokens = usage.TokenUsage{
			InputTokens:  usageData.PromptTokens,
			OutputTokens: usageData.CompletionTokens,
			TotalTokens:  usageData.TotalTokens,
		}
	}
	rec := usage.UsageRecord{
		Timestamp:     startTime,
		KeyHash:       keyHash,
		KeyName:       keyName,
		Model:         model,
		Endpoint:      "/v1/messages",
		StatusCode:    statusCode,
		RequestBytes:  requestBytes,
		ResponseBytes: responseBytes,
		InputTokens:   tokens.InputTokens,
		OutputTokens:  tokens.OutputTokens,
		TotalTokens:   tokens.TotalTokens,
		DurationMS:    time.Since(startTime).Milliseconds(),
	}
	go h.usage.Log(rec)
}
