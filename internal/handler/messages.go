package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path"
	"strings"
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
	config   *config.ConfigStore
	client   *http.Client
	usage    *usage.UsageLogger
	pipeline *pipeline.Pipeline
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

// --- Shared helpers ---

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
