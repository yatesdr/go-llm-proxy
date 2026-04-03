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
	"sync"
	"time"

	"go-llm-proxy/internal/api"
	"go-llm-proxy/internal/auth"
	"go-llm-proxy/internal/config"
	"go-llm-proxy/internal/httputil"
	"go-llm-proxy/internal/pipeline"
	"go-llm-proxy/internal/usage"
)

// ResponsesHandler implements the OpenAI Responses API (POST /v1/responses).
// By default, requests are proxied through to the backend transparently. If
// the backend does not support the Responses API (returns 404), the handler
// automatically falls back to translating requests into Chat Completions
// format. Detection is cached per backend URL and endpoint path, so a backend
// that supports /responses but not /responses/compact is handled correctly.
//
// Backends can control this via responses_mode in config: "auto" (default),
// "native" (always passthrough), or "translate" (always translate).
type ResponsesHandler struct {
	config   *config.ConfigStore
	client   *http.Client
	usage    *usage.UsageLogger
	pipeline *pipeline.Pipeline

	// nativeCache tracks which backend+path combinations support native
	// Responses API endpoints. Key: "backendURL\x00path", Value: bool.
	// Populated on first request per combination; never expires (restart to reset).
	nativeCache sync.Map
}

func NewResponsesHandler(cs *config.ConfigStore, usage *usage.UsageLogger, pipeline *pipeline.Pipeline) *ResponsesHandler {
	return &ResponsesHandler{
		config:   cs,
		usage:    usage,
		pipeline: pipeline,
		client:   httputil.NewHTTPClient(),
	}
}

func nativeCacheKey(backend, path string) string { return backend + "\x00" + path }

// shouldTranslate returns true if this backend+path should skip the native
// probe and always use Chat Completions translation.
func (h *ResponsesHandler) shouldTranslate(model *config.ModelConfig, path string) bool {
	if model.ResponsesMode == config.ResponsesModeTranslate {
		return true
	}
	if model.ResponsesMode == config.ResponsesModeNative {
		return false
	}
	// Auto mode: check the cache from previous probes.
	if cached, ok := h.nativeCache.Load(nativeCacheKey(model.Backend, path)); ok {
		return !cached.(bool)
	}
	return false // Unknown — will probe.
}

// shouldForceNative returns true if the model is configured to always
// passthrough without probing.
func (h *ResponsesHandler) shouldForceNative(model *config.ModelConfig) bool {
	return model.ResponsesMode == config.ResponsesModeNative
}

// tryNativePassthrough attempts to forward the request directly to the
// backend's native Responses API endpoint. Returns true if the backend
// handled the request (any status except 404). Returns false if the
// backend returned 404, meaning it does not support the endpoint —
// the caller should fall back to translation.
func (h *ResponsesHandler) tryNativePassthrough(ctx context.Context, w http.ResponseWriter, r *http.Request, body []byte, modelName string, model *config.ModelConfig, path, keyName, keyHash string, startTime time.Time) bool {
	if model.Model != modelName {
		body = RewriteModelName(body, model.Model)
	}

	upstreamURL := strings.TrimRight(model.Backend, "/") + path

	upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(body))
	if err != nil {
		return false
	}

	upReq.Header.Set("Content-Type", "application/json")
	if v := r.Header.Get("Accept"); v != "" {
		upReq.Header.Set("Accept", v)
	}
	if v := r.Header.Get("X-Request-ID"); v != "" {
		upReq.Header.Set("X-Request-ID", v)
	}
	if model.APIKey != "" {
		upReq.Header.Set("Authorization", "Bearer "+model.APIKey)
	}

	resp, err := h.client.Do(upReq)
	if err != nil {
		// Network error — don't cache (may be transient), let caller try translation.
		slog.Warn("native responses probe failed", "backend", model.Backend, "error", err)
		return false
	}

	// 404 = backend does not support this endpoint.
	cacheKey := nativeCacheKey(model.Backend, path)
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		h.nativeCache.Store(cacheKey, false)
		slog.Info("backend does not support native Responses API endpoint, falling back to translation",
			"backend", model.Backend, "path", path, "model", modelName)
		return false
	}

	// Backend handled it — cache as native and stream the response through.
	if _, loaded := h.nativeCache.LoadOrStore(cacheKey, true); !loaded {
		slog.Info("backend supports native Responses API endpoint",
			"backend", model.Backend, "path", path)
	}
	defer resp.Body.Close()

	slog.Info("proxying native responses request", "model", modelName, "path", path, "key", keyName)

	// For error responses, sanitize before sending to the client.
	if resp.StatusCode >= 400 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, api.MaxResponseBodySize))
		slog.Error("upstream returned error (native passthrough)",
			"model", modelName, "status", resp.StatusCode, "body", string(errBody))
		httputil.WriteError(w, resp.StatusCode, fmt.Sprintf("backend returned HTTP %d", resp.StatusCode))
		if h.usage != nil {
			rec := usage.UsageRecord{
				Timestamp: startTime, KeyHash: keyHash, KeyName: keyName,
				Model: modelName, Endpoint: "/v1" + path, StatusCode: resp.StatusCode,
				RequestBytes: int64(len(body)), ResponseBytes: int64(len(errBody)),
				DurationMS: time.Since(startTime).Milliseconds(),
			}
			go h.usage.Log(rec)
		}
		return true
	}

	// Forward response headers.
	for k := range AllowedResponseHeaders {
		if v := resp.Header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	httputil.SetSecurityHeaders(w)
	w.WriteHeader(resp.StatusCode)

	// Stream response body.
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

	// Log usage (token counts unavailable for passthrough without buffering).
	if h.usage != nil {
		rec := usage.UsageRecord{
			Timestamp:     startTime,
			KeyHash:       keyHash,
			KeyName:       keyName,
			Model:         modelName,
			Endpoint:      "/v1" + path,
			StatusCode:    resp.StatusCode,
			RequestBytes:  int64(len(body)),
			ResponseBytes: totalBytes,
			DurationMS:    time.Since(startTime).Milliseconds(),
		}
		go h.usage.Log(rec)
	}

	return true
}

// --- Handler ---

func (h *ResponsesHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httputil.WriteError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, api.MaxRequestBodySize)
	body, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		httputil.WriteError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}

	var req responsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid JSON request body")
		return
	}
	if req.Model == "" {
		httputil.WriteError(w, http.StatusBadRequest, "missing model field in request")
		return
	}

	cfg := h.config.Get()
	key := auth.KeyFromContext(r.Context())
	if !auth.KeyAllowsModel(key, req.Model) {
		httputil.WriteError(w, http.StatusForbidden, "not authorized for requested model")
		return
	}

	model := config.FindModel(cfg, req.Model)
	if model == nil {
		httputil.WriteError(w, http.StatusNotFound, "unknown model")
		return
	}
	if model.Type == config.BackendAnthropic {
		httputil.WriteError(w, http.StatusBadRequest, "responses API is not supported for anthropic backends")
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

	// Try native passthrough unless forced to translate.
	if !h.shouldTranslate(model, "/responses") {
		if h.tryNativePassthrough(ctx, w, r, body, req.Model, model, "/responses", keyName, keyHash, startTime) {
			return
		}
		// For native mode, don't fall back to translation — the backend must handle it.
		if h.shouldForceNative(model) {
			httputil.WriteError(w, http.StatusBadGateway, "backend failed native responses passthrough and responses_mode is set to native")
			return
		}
	}

	// Translate Responses API -> Chat Completions.
	slog.Info("proxying responses request (translated)", "model", req.Model, "key", keyName)

	messages, err := translateInput(req.Input, req.Instructions)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid input: "+err.Error())
		return
	}

	chatReq := buildChatRequest(req, model.Model, messages)

	// Run pipeline pre-send processors (vision, PDF, etc.).
	if h.pipeline != nil {
		chatReq, err = h.pipeline.ProcessRequest(ctx, chatReq, model)
		if err != nil {
			httputil.WriteError(w, http.StatusInternalServerError, "processing error: "+err.Error())
			return
		}
	}

	chatBody, err := json.Marshal(chatReq)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to build upstream request")
		return
	}

	upstreamURL := strings.TrimRight(model.Backend, "/") + api.ChatCompletionsPath

	upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(chatBody))
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to create upstream request")
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

	slog.Info("proxying responses request", "model", req.Model, "key", keyName)

	resp, err := h.client.Do(upReq)
	if err != nil {
		if ctx.Err() != nil {
			httputil.WriteError(w, http.StatusGatewayTimeout, "upstream request timed out")
			return
		}
		slog.Error("upstream request failed", "error", err, "model", req.Model)
		httputil.WriteError(w, http.StatusBadGateway, "upstream request failed")
		return
	}
	defer resp.Body.Close()

	// Forward upstream error responses.
	if resp.StatusCode >= 400 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, api.MaxResponseBodySize))
		slog.Error("upstream returned error for translated request",
			"model", req.Model, "status", resp.StatusCode, "body", string(errBody))

		if req.Stream {
			// Client expects SSE. Wrap the error as a response.failed event
			// so Codex sees a proper terminal event instead of a raw JSON body
			// on a connection it expected to be SSE.
			httputil.SetSecurityHeaders(w)
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.WriteHeader(http.StatusOK)
			failedData, _ := json.Marshal(map[string]any{
				"type": "response.failed",
				"response": map[string]any{
					"id":         api.RandomID("resp_"),
					"object":     "response",
					"created_at": float64(time.Now().Unix()),
					"model":      req.Model,
					"status":     "failed",
					"error": map[string]any{
						"type":    "upstream_error",
						"message": fmt.Sprintf("backend returned HTTP %d", resp.StatusCode),
					},
					"output": []any{},
				},
				"sequence_number": 0,
			})
			fmt.Fprintf(w, "event: response.failed\ndata: %s\n\n", failedData)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			return
		}

		// Sanitized error: never forward raw backend error bodies to the client.
		httputil.WriteError(w, resp.StatusCode, fmt.Sprintf("backend returned HTTP %d", resp.StatusCode))
		return
	}

	reqBytes := int64(len(chatBody))
	if req.Stream {
		h.handleStreaming(w, resp, req, model, chatReq, reqBytes, keyName, keyHash, startTime)
	} else {
		h.handleNonStreaming(w, resp, req, model, chatReq, reqBytes, keyName, keyHash, startTime)
	}
}

// --- Non-streaming handler ---

func (h *ResponsesHandler) handleNonStreaming(w http.ResponseWriter, resp *http.Response, req responsesRequest, model *config.ModelConfig, chatReq map[string]any, requestBytes int64, keyName, keyHash string, startTime time.Time) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, api.MaxResponseBodySize))
	if err != nil {
		httputil.WriteError(w, http.StatusBadGateway, "failed to read upstream response")
		return
	}

	var chatResp api.ChatResponse
	if err := json.Unmarshal(body, &chatResp); err != nil {
		httputil.WriteError(w, http.StatusBadGateway, "invalid upstream response")
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
			httputil.WriteError(w, http.StatusBadGateway, "search processing error: "+err.Error())
			return
		}
		chatResp = *finalResp
	}

	respID := api.RandomID("resp_")
	createdAt := float64(chatResp.Created)
	if createdAt == 0 {
		createdAt = float64(time.Now().Unix())
	}

	var output []any
	var outputText string
	status := "completed"
	var incompleteDetails any

	if len(chatResp.Choices) > 0 {
		choice := chatResp.Choices[0]
		msg := choice.Message

		if msg.Content != nil && *msg.Content != "" {
			outputText = *msg.Content
			output = append(output, map[string]any{
				"id":     api.RandomID("msg_"),
				"type":   "message",
				"role":   "assistant",
				"status": "completed",
				"content": []any{map[string]any{
					"type":        "output_text",
					"text":        *msg.Content,
					"annotations": []any{},
				}},
			})
		}

		for _, tc := range msg.ToolCalls {
			output = append(output, map[string]any{
				"id":        api.RandomID("fc_"),
				"type":      "function_call",
				"call_id":   tc.ID,
				"name":      tc.Function.Name,
				"arguments": tc.Function.Arguments,
				"status":    "completed",
			})
		}

		switch choice.FinishReason {
		case "length":
			status = "incomplete"
			incompleteDetails = map[string]any{"reason": "max_output_tokens"}
		case "content_filter":
			status = "failed"
		}
	}

	var usageObj any
	if chatResp.Usage != nil {
		usageObj = map[string]any{
			"input_tokens":  chatResp.Usage.PromptTokens,
			"output_tokens": chatResp.Usage.CompletionTokens,
			"total_tokens":  chatResp.Usage.TotalTokens,
		}
	}

	response := map[string]any{
		"id":                 respID,
		"object":             "response",
		"created_at":         createdAt,
		"model":              chatResp.Model,
		"status":             status,
		"error":              nil,
		"incomplete_details": incompleteDetails,
		"output":             output,
		"output_text":        outputText,
		"usage":              usageObj,
	}

	httputil.SetSecurityHeaders(w)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)

	h.logUsage(chatResp.Usage, resp.StatusCode, req.Model, requestBytes, int64(len(body)), keyName, keyHash, startTime)
}

// --- Streaming handler ---

// toolCallState tracks an in-progress tool call during streaming.
type toolCallState struct {
	itemID    string
	callID    string
	name      string
	args      strings.Builder
	outputIdx int
}

func (h *ResponsesHandler) handleStreaming(w http.ResponseWriter, resp *http.Response, req responsesRequest, model *config.ModelConfig, chatReq map[string]any, requestBytes int64, keyName, keyHash string, startTime time.Time) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		httputil.WriteError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	upstreamCT := resp.Header.Get("Content-Type")
	slog.Debug("streaming handler entered",
		"model", req.Model, "upstream_status", resp.StatusCode, "upstream_content_type", upstreamCT)

	httputil.SetSecurityHeaders(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	respID := api.RandomID("resp_")
	now := float64(time.Now().Unix())
	seq := 0
	outputIdx := 0
	upstreamModel := req.Model

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

	// Search buffering: when pipeline search is enabled, buffer tool call events.
	searchEnabled := h.pipeline != nil && h.pipeline.ResolveWebSearchKey(model) != ""
	type bufferedEvent struct {
		eventType string
		data      map[string]any
	}
	var toolCallBuffer []bufferedEvent
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

		// Reasoning delta — send SSE comments to keep the connection alive
		// during the model's thinking phase. Full reasoning events confuse
		// some Codex versions, so we use protocol-level keepalives instead.
		if delta.Reasoning != nil && *delta.Reasoning != "" {
			// SSE comment: keeps the TCP connection alive without producing
			// a client-visible event. Codex (and all SSE clients) ignore these.
			fmt.Fprintf(w, ": reasoning\n\n")
			flusher.Flush()
		}

		// Content delta.
		if delta.Content != nil && *delta.Content != "" {
			if !msgStarted {
				startMsg()
			}
			if !contentStarted {
				startContent()
			}
			textBuf.WriteString(*delta.Content)
			emit("response.output_text.delta", map[string]any{
				"delta":           *delta.Content,
				"content_index":   0,
				"output_index":    outputIdx,
				"item_id":         msgID,
				"sequence_number": seq,
			})
			seq++
		}

		// Tool call deltas.
		for _, tc := range delta.ToolCalls {
			if tc.ID != "" {
				// New tool call — finish the message first if open.
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

			go func() {
				defer close(searchDone)
				newChatReq, searchErr = h.pipeline.ExecuteSearchAndResend(
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
				// Reset state for re-stream.
				outputIdx = outputIdxBeforeTools
				toolCalls = nil
				toolCallBuffer = nil
				msgStarted = false
				contentStarted = false
				textBuf.Reset()

				newFinish, newUsage, newTC, newSeq := h.streamResponsesFromBackend(
					ctx, newChatReq, model, emit,
					&outputIdx, &seq, &msgID, &msgStarted, &contentStarted, &textBuf,
					startMsg, startContent, finishMsg)
				if newFinish != "" {
					finishReason = newFinish
				}
				if newUsage != nil {
					usageData = newUsage
				}
				toolCalls = newTC
				seq = newSeq
			}
		} else {
			// Mixed or no-search: replay buffered events.
			for _, ev := range toolCallBuffer {
				emit(ev.eventType, ev.data)
			}
		}
	} else {
		// No search case: replay any buffered events.
		for _, ev := range toolCallBuffer {
			emit(ev.eventType, ev.data)
		}
	}

	// Finalize pending items.
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
				"input_tokens":  usageData.PromptTokens,
				"output_tokens": usageData.CompletionTokens,
				"total_tokens":  usageData.TotalTokens,
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

// --- Compact handler ---

const compactSystemPrompt = "Summarize the preceding conversation between a user and an AI coding assistant. " +
	"Cover: (1) the task the user asked for, (2) key decisions and code changes made, " +
	"(3) current state — what is done and what remains. Be concise but preserve every " +
	"detail needed to continue the work without re-reading the full history."

// HandleCompact implements POST /v1/responses/compact.
// It uses the backend model to summarize the conversation, returning preserved
// user messages plus a summary. This matches the format Codex's own inline
// compaction produces for non-OpenAI providers.
func (h *ResponsesHandler) HandleCompact(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httputil.WriteError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, api.MaxRequestBodySize)
	body, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		httputil.WriteError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}

	var req responsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid JSON request body")
		return
	}
	if req.Model == "" {
		httputil.WriteError(w, http.StatusBadRequest, "missing model field")
		return
	}

	cfg := h.config.Get()
	key := auth.KeyFromContext(r.Context())
	if !auth.KeyAllowsModel(key, req.Model) {
		httputil.WriteError(w, http.StatusForbidden, "not authorized for requested model")
		return
	}
	model := config.FindModel(cfg, req.Model)
	if model == nil {
		httputil.WriteError(w, http.StatusNotFound, "unknown model")
		return
	}
	if model.Type == config.BackendAnthropic {
		httputil.WriteError(w, http.StatusBadRequest, "compaction is not supported for anthropic backends")
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

	// Try native passthrough unless forced to translate.
	if !h.shouldTranslate(model, "/responses/compact") {
		if h.tryNativePassthrough(ctx, w, r, body, req.Model, model, "/responses/compact", keyName, keyHash, startTime) {
			return
		}
		if h.shouldForceNative(model) {
			httputil.WriteError(w, http.StatusBadGateway, "backend failed native compact passthrough and responses_mode is set to native")
			return
		}
	}

	// Fall back to model-based summarization.
	slog.Info("proxying compact request (summarizing)", "model", req.Model, "key", keyName)

	messages, err := translateInput(req.Input, req.Instructions)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid input: "+err.Error())
		return
	}

	// Build a summarization request: system prompt + full conversation + summarize instruction.
	summaryMessages := []map[string]any{
		{"role": "system", "content": compactSystemPrompt},
	}
	summaryMessages = append(summaryMessages, messages...)
	summaryMessages = append(summaryMessages, map[string]any{
		"role":    "user",
		"content": "Now produce the summary.",
	})

	chatReq := map[string]any{
		"model":    model.Model,
		"messages": summaryMessages,
		"stream":   false,
	}

	chatBody, err := json.Marshal(chatReq)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to build upstream request")
		return
	}

	upstreamURL := strings.TrimRight(model.Backend, "/") + api.ChatCompletionsPath

	upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(chatBody))
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to create upstream request")
		return
	}
	upReq.Header.Set("Content-Type", "application/json")
	if model.APIKey != "" {
		upReq.Header.Set("Authorization", "Bearer "+model.APIKey)
	}

	resp, err := h.client.Do(upReq)
	if err != nil {
		if ctx.Err() != nil {
			httputil.WriteError(w, http.StatusGatewayTimeout, "upstream request timed out")
			return
		}
		slog.Error("compact upstream failed", "error", err, "model", req.Model)
		httputil.WriteError(w, http.StatusBadGateway, "upstream request failed")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, api.MaxResponseBodySize))
		httputil.SetSecurityHeaders(w)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		w.Write(errBody)
		return
	}

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, api.MaxResponseBodySize))
	if err != nil {
		httputil.WriteError(w, http.StatusBadGateway, "failed to read upstream response")
		return
	}

	var chatResp api.ChatResponse
	if err := json.Unmarshal(respBody, &chatResp); err != nil {
		httputil.WriteError(w, http.StatusBadGateway, "invalid upstream response")
		return
	}

	summaryText := ""
	if len(chatResp.Choices) > 0 && chatResp.Choices[0].Message.Content != nil {
		summaryText = *chatResp.Choices[0].Message.Content
	}

	// Build compacted output: preserved user messages + summary as assistant message.
	output := extractUserMessages(req.Input)
	output = append(output, map[string]any{
		"id":     api.RandomID("msg_"),
		"type":   "message",
		"role":   "assistant",
		"status": "completed",
		"content": []any{map[string]any{
			"type":        "output_text",
			"text":        summaryText,
			"annotations": []any{},
		}},
	})

	var usageObj any
	if chatResp.Usage != nil {
		usageObj = map[string]any{
			"input_tokens":  chatResp.Usage.PromptTokens,
			"output_tokens": chatResp.Usage.CompletionTokens,
			"total_tokens":  chatResp.Usage.TotalTokens,
		}
	}

	response := map[string]any{
		"id":         api.RandomID("resp_"),
		"object":     "response.compaction",
		"created_at": float64(time.Now().Unix()),
		"output":     output,
		"usage":      usageObj,
	}

	httputil.SetSecurityHeaders(w)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)

	h.logUsage(chatResp.Usage, resp.StatusCode, req.Model, int64(len(chatBody)), int64(len(respBody)), keyName, keyHash, startTime)
}

// extractUserMessages returns all user-role messages from the input array,
// normalized to Responses API typed-item format.
func extractUserMessages(input json.RawMessage) []any {
	var items []json.RawMessage
	if json.Unmarshal(input, &items) != nil {
		return nil
	}
	var out []any
	for _, raw := range items {
		var item struct {
			Role string `json:"role"`
		}
		if json.Unmarshal(raw, &item) != nil || item.Role != "user" {
			continue
		}
		// Pass through the original item, ensuring it has a type field.
		var full map[string]any
		json.Unmarshal(raw, &full)
		if full["type"] == nil {
			full["type"] = "message"
		}
		out = append(out, full)
	}
	return out
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
) (finishReason string, usageData *api.ChunkUsage, toolCalls []*toolCallState, finalSeq int) {

	finalSeq = *seq

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
			if !*msgStarted {
				startMsg()
			}
			if !*contentStarted {
				startContent()
			}
			textBuf.WriteString(*delta.Content)
			emit("response.output_text.delta", map[string]any{
				"delta":           *delta.Content,
				"content_index":   0,
				"output_index":    *outputIdx,
				"item_id":         *msgID,
				"sequence_number": *seq,
			})
			*seq++
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
				emit("response.output_item.added", map[string]any{
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
				})
				*seq++
				*outputIdx++
			}
			if tc.Function != nil && tc.Function.Arguments != "" {
				if tc.Index < len(toolCalls) && toolCalls[tc.Index] != nil {
					tcs := toolCalls[tc.Index]
					tcs.args.WriteString(tc.Function.Arguments)
					emit("response.function_call_arguments.delta", map[string]any{
						"delta":           tc.Function.Arguments,
						"item_id":         tcs.itemID,
						"output_index":    tcs.outputIdx,
						"sequence_number": *seq,
					})
					*seq++
				}
			}
		}

		if choice.FinishReason != nil {
			finishReason = *choice.FinishReason
		}
	}

	finalSeq = *seq
	return
}

func (h *ResponsesHandler) sendChatRequest(ctx context.Context, chatReq map[string]any, model *config.ModelConfig) (*api.ChatResponse, error) {
	return sendChatCompletionsRequest(ctx, h.client, chatReq, model)
}

// --- Usage logging ---

func (h *ResponsesHandler) logUsage(usageData *api.ChunkUsage, statusCode int, model string, requestBytes, responseBytes int64, keyName, keyHash string, startTime time.Time) {
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
		Endpoint:      "/v1/responses",
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
