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
	"regexp"
	"strings"
	"time"

	"go-llm-proxy/internal/api"
	"go-llm-proxy/internal/auth"
	"go-llm-proxy/internal/config"
	"go-llm-proxy/internal/httputil"
	"go-llm-proxy/internal/lb"
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
	requestedModel := req.Model
	resolvedModel := config.ResolveModelName(cfg, requestedModel)

	// Claude Code auto mode: classifier requests are answered by the decision
	// model whatever model name they ask for. On a miss the same request
	// continues below with the model swapped, so the reply stays a valid
	// classifier answer.
	if cfg.AutoMode != nil && !requireAnthropic {
		handled, fallbackModel := h.serveAutoMode(w, r, cfg, &req, body)
		if handled {
			return
		}
		if fallbackModel != "" {
			resolvedModel = fallbackModel
		}
	}

	req.Model = resolvedModel
	if req.Model != requestedModel {
		req.aliasFrom = requestedModel
		body = RewriteModelName(body, req.Model)
		slog.Info("resolved model alias", "model", req.Model, "alias_from", req.aliasFrom)
	}
	key := auth.KeyFromContext(r.Context())
	if !auth.KeyAllowsModel(key, req.Model) {
		httputil.WriteAnthropicError(w, http.StatusForbidden, "permission_error", "not authorized for requested model")
		return
	}

	model := config.FindModel(cfg, req.Model)
	if model == nil {
		RecordUnknownRequest(r, requestedModel, "messages")
		httputil.WriteAnthropicError(w, http.StatusNotFound, "not_found_error", "unknown model")
		return
	}

	if requireAnthropic && model.Type != config.BackendAnthropic {
		httputil.WriteAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "model is not an anthropic backend")
		return
	}

	// Sticky backend selection; the model view carries the chosen backend
	// through every downstream call for this request.
	model, releaseBackend := lb.ResolveModel(cfg, model, body)
	defer releaseBackend()

	keyName, keyHash := "", ""
	if key != nil {
		keyName = key.Name
		keyHash = usage.HashKey(key.Key)
	}
	startTime := time.Now()

	// model.Timeout bounds the wait for the backend's response headers and,
	// for non-streaming requests, the whole body. It stays armed through the
	// upstream Do call (so a backend that never answers is cut off) and is
	// disarmed only once a streaming response has started successfully: a
	// wall-clock cap on a live stream severs healthy long generations (see
	// requestTimeout and disarmIfStream).
	ctx, cancel, disarmTimeout := armUntilHeaders(r.Context(), model)
	defer cancel()

	// AWS Bedrock backends use the Converse API (translated from Anthropic
	// Messages, signed with SigV4 or a Bedrock API key) instead of Chat
	// Completions. Dispatched here because the Bedrock wire format differs
	// from both anthropic-native and openai-translated paths, and
	// messages_mode is not a meaningful knob for Bedrock — it's always
	// Converse. Implementation lives in messages_bedrock.go.
	if model.Type == config.BackendBedrock {
		if req.Stream {
			disarmTimeout() // Bedrock streams are not covered by the header-wait rule.
		}
		h.handleBedrock(ctx, w, body, req, model, keyName, keyHash, r.Header.Get("X-Request-ID"), startTime)
		return
	}

	// Native passthrough for anthropic backends or native mode.
	if !h.shouldTranslate(model) {
		h.handleNativePassthrough(ctx, disarmTimeout, w, r, body, req, model, keyName, keyHash, startTime)
		return
	}

	// Translate Anthropic Messages -> Chat Completions.
	slog.Info("proxying messages request (translated)", "model", req.Model, "key", keyName)

	chatReq, err := buildChatRequestFromAnthropic(req, model.Model)
	if err != nil {
		slog.Error("messages translation failed", "model", req.Model, "error", err)
		httputil.WriteAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "request translation failed")
		return
	}
	applyModelOptions(chatReq, req, model)

	// Run pipeline pre-send processors (vision, PDF, etc.).
	// For streaming requests, send SSE keepalives during processing to prevent
	// the client from timing out while the vision model describes images or a
	// large PDF is extracted.
	headersAlreadySent := false
	if h.pipeline != nil && h.pipeline.ShouldProcess(model) {
		if req.Stream && (pipeline.RequestContainsImageURLs(chatReq) || pipeline.RequestContainsPDFData(chatReq)) {
			chatReq, headersAlreadySent, err = runPipelineWithKeepalives(ctx, w, h.pipeline, chatReq, model)
		} else {
			chatReq, err = h.pipeline.ProcessRequest(ctx, chatReq, model)
		}
		if err != nil {
			if headersAlreadySent {
				// Headers already flushed at 200 — emit an SSE error event.
				writeSSEError(w, "api_error", "internal processing error")
			} else {
				slog.Error("pipeline processing failed", "model", req.Model, "error", err)
				httputil.WriteAnthropicError(w, http.StatusInternalServerError, "api_error", "internal processing error")
			}
			return
		}
	}

	// No backend accepts the internal pdf_data / document_url parts. Anything
	// the pipeline left behind (documents rewriting off) becomes an OpenAI file
	// part or a placeholder.
	pipeline.ConvertPDFDataToFileParts(chatReq)

	// Check if the client disconnected during pipeline processing (e.g. vision
	// calls can take 30+ seconds, and the client may have given up waiting).
	if ctx.Err() != nil {
		slog.Warn("client disconnected during pipeline processing",
			"model", req.Model, "key", keyName, "error", ctx.Err())
		return
	}

	chatBody, err := json.Marshal(chatReq)
	if err != nil {
		httputil.WriteAnthropicError(w, http.StatusInternalServerError, "api_error", "failed to build upstream request")
		return
	}
	if clampMaxTokens(chatReq, chatBody, model) {
		if chatBody, err = json.Marshal(chatReq); err != nil {
			httputil.WriteAnthropicError(w, http.StatusInternalServerError, "api_error", "failed to build upstream request")
			return
		}
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
	if err != nil || resp.StatusCode >= 400 {
		// Failures only: a success is recorded once, by logUsage.
		lb.RecordAttempt(model.Backend, statusOf(resp), err, r.Context().Err() != nil)
	}
	if err != nil {
		if ctx.Err() != nil {
			h.failBeforeStream(w, headersAlreadySent, http.StatusGatewayTimeout, "api_error", "upstream request timed out")
			return
		}
		slog.Error("upstream request failed", "error", err, "model", req.Model)
		h.failBeforeStream(w, headersAlreadySent, http.StatusBadGateway, "api_error", "upstream request failed")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, api.MaxResponseBodySize))
		slog.Error("upstream returned error for translated messages request",
			"model", req.Model, "status", resp.StatusCode, "body", string(errBody))

		// Never forward raw backend error bodies: they may contain internal
		// URLs, backend API keys, or infrastructure details. The message is
		// extracted, scrubbed and (for context overflows) rewritten instead.
		outStatus, outType, errMsg := buildClientError(resp.StatusCode, errBody, false)
		if outStatus == http.StatusBadRequest && imageRejectedRe.MatchString(errMsg) && pipeline.RequestContainsImageURLs(chatReq) {
			errMsg = fmt.Sprintf("The backend model (%s) does not appear to support image inputs. "+
				"Remove images from the conversation or use a vision-capable model. (%s)", req.Model, errMsg)
		}

		// Streaming with nothing sent yet: answer with the real HTTP status so
		// the client's retry and error handling work. Only when the stream was
		// already opened (pipeline keepalives) is an in-band SSE error needed.
		if headersAlreadySent {
			writeSSEError(w, outType, errMsg)
			return
		}
		copyRetryHeaders(w, resp.Header)
		httputil.WriteAnthropicError(w, outStatus, outType, errMsg)
		return
	}

	reqBytes := int64(len(chatBody))
	if req.Stream {
		disarmIfStream(resp, disarmTimeout) // a live stream may run as long as it needs
		h.handleStreaming(w, resp, req, model, chatReq, reqBytes, keyName, keyHash, startTime, headersAlreadySent)
	} else {
		h.handleNonStreaming(w, resp, req, model, chatReq, reqBytes, keyName, keyHash, startTime)
	}
}

// imageRejectedRe recognises backend errors complaining about image input.
var imageRejectedRe = regexp.MustCompile(`(?i)image|multimodal|multi-modal|vision`)

// failBeforeStream reports a failure that happened before any upstream
// response: as an HTTP error normally, or as an SSE error event when the
// stream was already opened for pipeline keepalives.
func (h *MessagesHandler) failBeforeStream(w http.ResponseWriter, streamOpen bool, status int, errType, msg string) {
	if streamOpen {
		writeSSEError(w, errType, msg)
		return
	}
	httputil.WriteAnthropicError(w, status, errType, msg)
}

// --- Native passthrough ---

func (h *MessagesHandler) handleNativePassthrough(ctx context.Context, disarmTimeout func(), w http.ResponseWriter, r *http.Request, body []byte, req messagesRequest, model *config.ModelConfig, keyName, keyHash string, startTime time.Time) {
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
	if err != nil || resp.StatusCode >= 400 {
		// Failures only: a success is recorded once, by logUsage.
		lb.RecordAttempt(model.Backend, statusOf(resp), err, r.Context().Err() != nil)
	}
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
		writeUpstreamFailure(w, resp.Header, resp.StatusCode, errBody, true)
		logUsage(h.usage, usageLogInput{
			startTime: startTime, statusCode: resp.StatusCode,
			keyName: keyName, keyHash: keyHash,
			model: req.Model, aliasFrom: req.aliasFrom, endpoint: "/v1/messages",
			backend:      model.Backend,
			requestBytes: int64(len(body)), responseBytes: int64(len(errBody)),
		})
		return
	}

	// Forward response headers (including rate-limit state).
	for k := range AllowedResponseHeaders {
		if v := resp.Header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	copyRetryHeaders(w, resp.Header)
	httputil.SetSecurityHeaders(w)
	w.WriteHeader(resp.StatusCode)
	disarmIfStream(resp, disarmTimeout) // a live stream is not wall-clock capped

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

	logUsage(h.usage, usageLogInput{
		startTime: startTime, statusCode: resp.StatusCode,
		keyName: keyName, keyHash: keyHash,
		model: req.Model, aliasFrom: req.aliasFrom, endpoint: "/v1/messages",
		backend:      model.Backend,
		requestBytes: int64(len(body)), responseBytes: totalBytes,
	})
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
			slog.Error("search processing failed", "model", req.Model, "error", err)
			httputil.WriteAnthropicError(w, http.StatusBadGateway, "api_error", "search processing failed")
			return
		}
		chatResp = *finalResp
	}

	respID := api.RandomID("msg_")
	var content []any
	stopReason := "end_turn"
	var stopSequence any

	if len(chatResp.Choices) > 0 {
		choice := chatResp.Choices[0]
		msg := choice.Message

		// Reasoning -> thinking block (before text content).
		// Supports both "reasoning" and "reasoning_content" JSON fields.
		if r := msg.EffectiveReasoning(); r != nil && *r != "" {
			content = append(content, map[string]any{
				"type":      "thinking",
				"thinking":  *r,
				"signature": api.RandomID(""),
			})
		}

		if msg.Content != nil && *msg.Content != "" {
			// Strip <think>...</think> tags from content; route to thinking block.
			thinkText, contentText := stripThinkTags(*msg.Content)

			if thinkText != "" && msg.EffectiveReasoning() == nil {
				content = append(content, map[string]any{
					"type":      "thinking",
					"thinking":  thinkText,
					"signature": api.RandomID(""),
				})
			}

			if contentText != "" {
				content = append(content, map[string]any{
					"type": "text",
					"text": contentText,
				})
			}
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

		stopReason, stopSequence = resolveStopReason(choice.FinishReason, choice.StopReason,
			req.StopSequences, len(msg.ToolCalls) > 0)
	}

	if content == nil {
		content = []any{}
	}

	usageObj := anthropicUsage(chatResp.Usage)

	response := map[string]any{
		"id":            respID,
		"type":          "message",
		"role":          "assistant",
		"content":       content,
		"model":         chatResp.Model,
		"stop_reason":   stopReason,
		"stop_sequence": stopSequence,
		"usage":         usageObj,
	}

	httputil.SetSecurityHeaders(w)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)

	logUsageChat(h.usage, usageLogInput{
		startTime: startTime, statusCode: resp.StatusCode,
		keyName: keyName, keyHash: keyHash,
		model: req.Model, aliasFrom: req.aliasFrom, endpoint: "/v1/messages",
		backend:      model.Backend,
		requestBytes: requestBytes, responseBytes: int64(len(body)),
	}, chatResp.Usage)
}

// --- Shared helpers ---

func (h *MessagesHandler) sendChatRequest(ctx context.Context, chatReq map[string]any, model *config.ModelConfig) (*api.ChatResponse, error) {
	return sendChatCompletionsRequest(ctx, h.client, chatReq, model)
}
