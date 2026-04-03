package main

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
)

// MessagesHandler implements the Anthropic Messages API (POST /v1/messages).
// For backends with type: "anthropic", requests pass through natively.
// For OpenAI-compatible backends, requests are translated to Chat Completions
// format automatically, enabling Claude Code to work with any backend.
type MessagesHandler struct {
	config      *ConfigStore
	client      *http.Client
	usage       *UsageLogger
	nativeCache sync.Map
}

func NewMessagesHandler(cs *ConfigStore, usage *UsageLogger) *MessagesHandler {
	return &MessagesHandler{
		config: cs,
		usage:  usage,
		client: &http.Client{
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// shouldTranslate returns true if this backend should translate Anthropic
// Messages to Chat Completions.
func (h *MessagesHandler) shouldTranslate(model *ModelConfig) bool {
	if model.MessagesMode == MessagesModeTranslate {
		return true
	}
	if model.MessagesMode == MessagesModeNative {
		return false
	}
	// Auto mode: anthropic backends always passthrough, others always translate.
	// Unlike the Responses API, no OpenAI backend supports /v1/messages, so
	// probing is pointless — we can decide from the config type alone.
	return model.Type != BackendAnthropic
}

func (h *MessagesHandler) shouldForceNative(model *ModelConfig) bool {
	return model.MessagesMode == MessagesModeNative
}

func (h *MessagesHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAnthropicError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}

	// Handle /anthropic/v1/messages prefix.
	cleanPath := path.Clean(r.URL.Path)
	requireAnthropic := false
	if strings.HasPrefix(cleanPath, "/anthropic/") {
		cleanPath = strings.TrimPrefix(cleanPath, "/anthropic")
		requireAnthropic = true
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)
	body, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		writeAnthropicError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "request body too large")
		return
	}

	var req messagesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON request body")
		return
	}
	if req.Model == "" {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "missing model field")
		return
	}

	cfg := h.config.Get()
	key := keyFromContext(r.Context())
	if !keyAllowsModel(key, req.Model) {
		writeAnthropicError(w, http.StatusForbidden, "permission_error", "not authorized for requested model")
		return
	}

	model := findModel(cfg, req.Model)
	if model == nil {
		writeAnthropicError(w, http.StatusNotFound, "not_found_error", "unknown model")
		return
	}

	if requireAnthropic && model.Type != BackendAnthropic {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "model is not an anthropic backend")
		return
	}

	keyName, keyHash := "", ""
	if key != nil {
		keyName = key.Name
		keyHash = HashKey(key.Key)
	}
	startTime := time.Now()

	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(model.Timeout)*time.Second)
	defer cancel()

	// Native passthrough for anthropic backends or native mode.
	if !h.shouldTranslate(model) {
		h.handleNativePassthrough(ctx, w, r, body, req, model, keyName, keyHash, startTime)
		return
	}

	// Translate Anthropic Messages → Chat Completions.
	slog.Info("proxying messages request (translated)", "model", req.Model, "key", keyName)

	chatReq, err := buildChatRequestFromAnthropic(req, model.Model)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "translation error: "+err.Error())
		return
	}

	chatBody, err := json.Marshal(chatReq)
	if err != nil {
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", "failed to build upstream request")
		return
	}

	upstreamURL := strings.TrimRight(model.Backend, "/") + "/chat/completions"

	upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(chatBody))
	if err != nil {
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", "failed to create upstream request")
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
			writeAnthropicError(w, http.StatusGatewayTimeout, "api_error", "upstream request timed out")
			return
		}
		slog.Error("upstream request failed", "error", err, "model", req.Model)
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "upstream request failed")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodySize))
		slog.Error("upstream returned error for translated messages request",
			"model", req.Model, "status", resp.StatusCode, "body", string(errBody))

		// Provide a friendly error when the backend rejects a request containing images.
		errMsg := fmt.Sprintf("backend returned %d: %s", resp.StatusCode, string(errBody))
		if resp.StatusCode == http.StatusBadRequest && requestContainsImages(chatReq) {
			errMsg = fmt.Sprintf("The backend model (%s) does not appear to support image inputs. "+
				"Remove images from the conversation or use a vision-capable model. "+
				"Original error: %s", req.Model, string(errBody))
		}

		if req.Stream {
			setSecurityHeaders(w)
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

		writeAnthropicError(w, resp.StatusCode, "api_error", errMsg)
		return
	}

	reqBytes := int64(len(chatBody))
	if req.Stream {
		h.handleStreaming(w, resp, req, reqBytes, keyName, keyHash, startTime)
	} else {
		h.handleNonStreaming(w, resp, req, reqBytes, keyName, keyHash, startTime)
	}
}

// --- Native passthrough ---

func (h *MessagesHandler) handleNativePassthrough(ctx context.Context, w http.ResponseWriter, r *http.Request, body []byte, req messagesRequest, model *ModelConfig, keyName, keyHash string, startTime time.Time) {
	if model.Model != req.Model {
		body = rewriteModelName(body, model.Model)
	}

	// Build upstream URL: Anthropic backends keep /v1 in path.
	upstreamURL := strings.TrimRight(model.Backend, "/") + "/v1/messages"

	upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(body))
	if err != nil {
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", "failed to create upstream request")
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
			writeAnthropicError(w, http.StatusGatewayTimeout, "api_error", "upstream request timed out")
			return
		}
		slog.Error("upstream request failed", "error", err, "model", req.Model)
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "upstream request failed")
		return
	}
	defer resp.Body.Close()

	// Forward response headers.
	for k := range allowedResponseHeaders {
		if v := resp.Header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	setSecurityHeaders(w)
	w.WriteHeader(resp.StatusCode)

	flusher, canFlush := w.(http.Flusher)
	buf := make([]byte, 4096)
	var totalBytes int64
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			totalBytes += int64(n)
			if totalBytes > maxResponseBodySize {
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
		rec := UsageRecord{
			Timestamp: startTime, KeyHash: keyHash, KeyName: keyName,
			Model: req.Model, Endpoint: "/v1/messages", StatusCode: resp.StatusCode,
			RequestBytes: int64(len(body)), ResponseBytes: totalBytes,
			DurationMS: time.Since(startTime).Milliseconds(),
		}
		go h.usage.Log(rec)
	}
}

// --- Non-streaming handler ---

func (h *MessagesHandler) handleNonStreaming(w http.ResponseWriter, resp *http.Response, req messagesRequest, requestBytes int64, keyName, keyHash string, startTime time.Time) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodySize))
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "failed to read upstream response")
		return
	}

	var chatResp chatResponse
	if err := json.Unmarshal(body, &chatResp); err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "invalid upstream response")
		return
	}

	respID := randomID("msg_")
	var content []any
	stopReason := "end_turn"

	if len(chatResp.Choices) > 0 {
		choice := chatResp.Choices[0]
		msg := choice.Message

		// Reasoning → thinking block (before text content).
		if msg.Reasoning != nil && *msg.Reasoning != "" {
			content = append(content, map[string]any{
				"type":      "thinking",
				"thinking":  *msg.Reasoning,
				"signature": randomID(""),
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

	setSecurityHeaders(w)
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

func (h *MessagesHandler) handleStreaming(w http.ResponseWriter, resp *http.Response, req messagesRequest, requestBytes int64, keyName, keyHash string, startTime time.Time) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", "streaming not supported")
		return
	}

	setSecurityHeaders(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	upstreamModel := req.Model
	msgID := randomID("msg_")
	blockIndex := 0
	msgStartEmitted := false
	textBlockOpen := false
	reasoningBlockOpen := false
	var toolCalls []*msgToolCallState
	var usage *chunkUsage
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

	closeAllBlocks := func() {
		closeReasoningBlock()
		closeTextBlock()
		for _, tc := range toolCalls {
			if tc != nil {
				emit("content_block_stop", map[string]any{"index": tc.blockIndex})
			}
		}
	}

	// Read and translate the upstream SSE stream.
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)

	for scanner.Scan() {
		line := scanner.Text()
		responseBytes += int64(len(line)) + 1

		if responseBytes > maxResponseBodySize {
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

		var chunk chatChunk
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
			usage = chunk.Usage
		}
		if len(chunk.Choices) == 0 {
			continue
		}

		choice := chunk.Choices[0]
		delta := choice.Delta

		// Reasoning tokens → thinking block.
		if delta.Reasoning != nil && *delta.Reasoning != "" {
			if !reasoningBlockOpen {
				openReasoningBlock()
			}
			emit("content_block_delta", map[string]any{
				"index": blockIndex,
				"delta": map[string]any{"type": "thinking_delta", "thinking": *delta.Reasoning},
			})
		}

		// Content delta → text block.
		if delta.Content != nil && *delta.Content != "" {
			if reasoningBlockOpen {
				closeReasoningBlock()
			}
			if !textBlockOpen {
				openTextBlock()
			}
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

				emit("content_block_start", map[string]any{
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

		// Finish reason.
		if choice.FinishReason != nil {
			finishReason = *choice.FinishReason
			closeAllBlocks()
		}
	}

	// Emit terminal events.
	if msgStartEmitted {
		if finishReason == "" {
			closeAllBlocks()
			finishReason = "stop"
		}

		stopReason := mapFinishToStopReason(finishReason)

		var outputTokens int
		if usage != nil {
			outputTokens = usage.CompletionTokens
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

	h.logUsage(usage, resp.StatusCode, req.Model, requestBytes, responseBytes, keyName, keyHash, startTime)
}

// --- Usage logging ---

func (h *MessagesHandler) logUsage(usage *chunkUsage, statusCode int, model string, requestBytes, responseBytes int64, keyName, keyHash string, startTime time.Time) {
	if h.usage == nil {
		return
	}
	var tokens TokenUsage
	if usage != nil {
		tokens = TokenUsage{
			InputTokens:  usage.PromptTokens,
			OutputTokens: usage.CompletionTokens,
			TotalTokens:  usage.TotalTokens,
		}
	}
	rec := UsageRecord{
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
