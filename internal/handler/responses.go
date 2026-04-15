package handler

import (
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

	// 404, 405, 500 = backend does not support this endpoint.
	// Some backends (e.g. SGLang) return 500 instead of 404 for unrecognized routes.
	cacheKey := nativeCacheKey(model.Backend, path)
	if resp.StatusCode == http.StatusNotFound ||
		resp.StatusCode == http.StatusMethodNotAllowed ||
		resp.StatusCode == http.StatusInternalServerError {
		resp.Body.Close()
		h.nativeCache.Store(cacheKey, false)
		slog.Info("backend does not support native Responses API endpoint, falling back to translation",
			"backend", model.Backend, "path", path, "model", modelName, "status", resp.StatusCode)
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
	if model.Type == config.BackendBedrock {
		httputil.WriteError(w, http.StatusBadRequest,
			"responses API is not supported for bedrock backends; use /v1/chat/completions or /v1/messages")
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
		slog.Error("responses input translation failed", "model", req.Model, "error", err)
		httputil.WriteError(w, http.StatusBadRequest, "request translation failed")
		return
	}

	chatReq := buildChatRequest(req, model.Model, messages)

	// Run pipeline pre-send processors (vision, PDF, etc.).
	// For streaming requests, send SSE keepalives during processing to prevent
	// the client from timing out while the vision model describes images.
	headersAlreadySent := false
	if h.pipeline != nil && h.pipeline.ShouldProcess(model) {
		if req.Stream && pipeline.RequestContainsImageURLs(chatReq) {
			chatReq, headersAlreadySent, err = runPipelineWithKeepalives(ctx, w, h.pipeline, chatReq, model)
		} else {
			chatReq, err = h.pipeline.ProcessRequest(ctx, chatReq, model)
		}
		if err != nil {
			if headersAlreadySent {
				// Headers already flushed at 200 — emit an SSE error event.
				failedData, _ := json.Marshal(map[string]any{
					"type": "response.failed",
					"response": map[string]any{
						"id":     api.RandomID("resp_"),
						"object": "response",
						"model":  req.Model,
						"status": "failed",
						"error": map[string]any{
							"type":    "server_error",
							"message": "internal processing error",
						},
						"output": []any{},
					},
					"sequence_number": 0,
				})
				fmt.Fprintf(w, "event: response.failed\ndata: %s\n\n", failedData)
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			} else {
				slog.Error("pipeline processing failed", "model", req.Model, "error", err)
				httputil.WriteError(w, http.StatusInternalServerError, "internal processing error")
			}
			return
		}
	}

	// Check if the client disconnected during pipeline processing.
	if ctx.Err() != nil {
		slog.Warn("client disconnected during pipeline processing",
			"model", req.Model, "key", keyName, "error", ctx.Err())
		return
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
		h.handleStreaming(w, resp, req, model, chatReq, reqBytes, keyName, keyHash, startTime, headersAlreadySent)
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
			slog.Error("search processing failed", "model", req.Model, "error", err)
			httputil.WriteError(w, http.StatusBadGateway, "search processing failed")
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

		// Emit reasoning from JSON field (reasoning or reasoning_content).
		if r := msg.EffectiveReasoning(); r != nil && *r != "" {
			output = append(output, map[string]any{
				"id":   api.RandomID("rs_"),
				"type": "reasoning",
				"summary": []any{map[string]any{
					"type": "summary_text",
					"text": *r,
				}},
			})
		}

		if msg.Content != nil && *msg.Content != "" {
			// Strip <think>...</think> tags from content; route to reasoning.
			thinkText, contentText := stripThinkTags(*msg.Content)

			if thinkText != "" && msg.EffectiveReasoning() == nil {
				output = append(output, map[string]any{
					"id":   api.RandomID("rs_"),
					"type": "reasoning",
					"summary": []any{map[string]any{
						"type": "summary_text",
						"text": thinkText,
					}},
				})
			}

			if contentText != "" {
				outputText = contentText
				output = append(output, map[string]any{
					"id":     api.RandomID("msg_"),
					"type":   "message",
					"role":   "assistant",
					"status": "completed",
					"content": []any{map[string]any{
						"type":        "output_text",
						"text":        contentText,
						"annotations": []any{},
					}},
				})
			}
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
			"input_tokens":          chatResp.Usage.PromptTokens,
			"input_tokens_details":  nil,
			"output_tokens":         chatResp.Usage.CompletionTokens,
			"output_tokens_details": nil,
			"total_tokens":          chatResp.Usage.TotalTokens,
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

	logUsageChat(h.usage, usageLogInput{
		startTime: startTime, statusCode: resp.StatusCode,
		keyName: keyName, keyHash: keyHash,
		model: req.Model, endpoint: "/v1/responses",
		requestBytes: requestBytes, responseBytes: int64(len(body)),
	}, chatResp.Usage)
}

func (h *ResponsesHandler) sendChatRequest(ctx context.Context, chatReq map[string]any, model *config.ModelConfig) (*api.ChatResponse, error) {
	return sendChatCompletionsRequest(ctx, h.client, chatReq, model)
}
