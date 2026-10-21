package handler

import (
	"bytes"
	"context"
	"encoding/json"
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
	"go-llm-proxy/internal/lb"
	"go-llm-proxy/internal/pipeline"
	"go-llm-proxy/internal/usage"
)

// ResponsesHandler implements the OpenAI Responses API (POST /v1/responses).
// By default, the handler probes route support without a request body, proxies
// native endpoints transparently, and otherwise translates requests into Chat
// Completions format. Detection is cached per backend, model, and endpoint path,
// so a backend that supports /responses but not /responses/compact is handled
// correctly without letting one model's error affect another.
//
// Backends can control this via responses_mode in config: "auto" (default),
// "native" (always passthrough), or "translate" (always translate).
type ResponsesHandler struct {
	config   *config.ConfigStore
	client   *http.Client
	usage    *usage.UsageLogger
	pipeline *pipeline.Pipeline

	// nativeCache tracks native Responses API support. Positive and negative
	// results expire so backend upgrades and corrected model configuration do
	// not require a proxy restart.
	nativeCache sync.Map
}

type nativeSupportEntry struct {
	supported bool
	expiresAt time.Time
}

const (
	nativeSupportTTL     = 30 * time.Minute
	nativeUnsupportedTTL = 2 * time.Minute
)

func NewResponsesHandler(cs *config.ConfigStore, usage *usage.UsageLogger, pipeline *pipeline.Pipeline) *ResponsesHandler {
	return &ResponsesHandler{
		config:   cs,
		usage:    usage,
		pipeline: pipeline,
		client:   httputil.NewHTTPClient(),
	}
}

func nativeCacheKey(model *config.ModelConfig, path string) string {
	return model.Backend + "\x00" + model.Model + "\x00" + path
}

func (h *ResponsesHandler) loadNativeSupport(model *config.ModelConfig, path string) (bool, bool) {
	key := nativeCacheKey(model, path)
	value, ok := h.nativeCache.Load(key)
	if !ok {
		return false, false
	}
	entry, ok := value.(nativeSupportEntry)
	if !ok || time.Now().After(entry.expiresAt) {
		h.nativeCache.Delete(key)
		return false, false
	}
	return entry.supported, true
}

func (h *ResponsesHandler) storeNativeSupport(model *config.ModelConfig, path string, supported bool) {
	ttl := nativeSupportTTL
	if !supported {
		ttl = nativeUnsupportedTTL
	}
	h.nativeCache.Store(nativeCacheKey(model, path), nativeSupportEntry{
		supported: supported,
		expiresAt: time.Now().Add(ttl),
	})
}

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
	if supported, ok := h.loadNativeSupport(model, path); ok {
		return !supported
	}
	return false // Unknown — will probe.
}

// shouldForceNative returns true if the model is configured to always
// passthrough without probing.
func (h *ResponsesHandler) shouldForceNative(model *config.ModelConfig) bool {
	return model.ResponsesMode == config.ResponsesModeNative
}

// probeNativeRoute checks endpoint availability without using a real client
// request as the capability probe. The bools are (supported, conclusive).
func (h *ResponsesHandler) probeNativeRoute(ctx context.Context, model *config.ModelConfig, path string) (bool, bool) {
	upstreamURL := strings.TrimRight(model.Backend, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodOptions, upstreamURL, nil)
	if err != nil {
		return false, false
	}
	if model.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+model.APIKey)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return false, false
	}
	defer resp.Body.Close()

	// Only a response that names POST in Allow proves the route exists:
	// CORS-enabled servers answer OPTIONS with 2xx on every path, so a bare
	// 2xx proves nothing.
	if (resp.StatusCode >= 200 && resp.StatusCode < 300) || resp.StatusCode == http.StatusMethodNotAllowed {
		for _, method := range strings.Split(resp.Header.Get("Allow"), ",") {
			if strings.EqualFold(strings.TrimSpace(method), http.MethodPost) {
				return true, true
			}
		}
		return false, false
	}
	if resp.StatusCode == http.StatusNotFound {
		return false, true
	}
	return false, false
}

// defaultInputImageDetail sets "detail":"auto" on input_image parts that omit
// it. OpenAI treats detail as optional, but vLLM's Responses endpoint rejects
// the whole request without it. The body is returned unchanged when there is
// nothing to fill in or it does not parse.
func defaultInputImageDetail(body []byte) []byte {
	if !bytes.Contains(body, []byte(`"input_image"`)) {
		return body
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var req map[string]any
	if dec.Decode(&req) != nil {
		return body
	}
	changed := false
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			if t["type"] == "input_image" {
				if _, ok := t["detail"]; !ok {
					t["detail"] = "auto"
					changed = true
				}
			}
			for _, child := range t {
				walk(child)
			}
		case []any:
			for _, child := range t {
				walk(child)
			}
		}
	}
	walk(req["input"])
	if !changed {
		return body
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if enc.Encode(req) != nil {
		return body
	}
	return bytes.TrimRight(out.Bytes(), "\n")
}

// tryNativePassthrough attempts to forward the request directly to the
// backend's native Responses API endpoint. It returns false only when auto
// mode has conclusive evidence that translation should be used.
func (h *ResponsesHandler) tryNativePassthrough(ctx context.Context, disarm func(), w http.ResponseWriter, r *http.Request, body []byte, modelName, aliasFrom string, model *config.ModelConfig, path, keyName, keyHash string, startTime time.Time) bool {
	forceNative := h.shouldForceNative(model)
	routeSupported := forceNative
	if !forceNative {
		if supported, ok := h.loadNativeSupport(model, path); ok {
			routeSupported = supported
		} else if supported, conclusive := h.probeNativeRoute(ctx, model, path); conclusive {
			h.storeNativeSupport(model, path, supported)
			if !supported {
				slog.Info("backend does not expose native Responses API endpoint, falling back to translation",
					"backend", model.Backend, "path", path, "model", modelName)
				return false
			}
			routeSupported = true
		}
	}

	if model.Model != modelName {
		body = RewriteModelName(body, model.Model)
	}
	body = defaultInputImageDetail(body)

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
		if r.Context().Err() == nil {
			lb.RecordOutcome(model.Backend, false)
		}
		return false
	}

	// In auto mode, an unprobed 404/405 can mean the route is absent. A 404
	// carrying an OpenAI-style "model not found" error means the route exists
	// and only the model is wrong: report that instead of translating, and
	// leave the route cache alone.
	if !forceNative && !routeSupported && (resp.StatusCode == http.StatusNotFound ||
		resp.StatusCode == http.StatusMethodNotAllowed) {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound && looksLikeModelMissing(errBody) {
			slog.Warn("native responses backend does not know the model",
				"backend", model.Backend, "model", modelName, "body", cleanUpstreamMessage(string(errBody)))
			ue := classifyUpstreamError(resp.StatusCode, errBody, resp.Header)
			ue.Message = "model not found on backend"
			writeUpstreamError(w, ue)
			return true
		}
		h.storeNativeSupport(model, path, false)
		slog.Info("backend does not support native Responses API endpoint, falling back to translation",
			"backend", model.Backend, "path", path, "model", modelName, "status", resp.StatusCode)
		return false
	}

	// A native 5xx in auto mode falls back to translation for this request:
	// an unpatched vLLM fails on Responses features (additional_tools,
	// namespaces) that Chat Completions translation handles. The native route
	// is then skipped for a short while instead of being retried on every
	// request.
	if !forceNative && resp.StatusCode >= http.StatusInternalServerError {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		lb.RecordOutcome(model.Backend, false)
		h.storeNativeSupport(model, path, false)
		slog.Warn("native responses request failed, falling back to translation",
			"backend", model.Backend, "path", path, "model", modelName, "status", resp.StatusCode)
		return false
	}

	// A non-5xx response establishes route support even when an OPTIONS probe
	// was inconclusive.
	if !forceNative && resp.StatusCode < http.StatusInternalServerError {
		_, previouslyKnown := h.loadNativeSupport(model, path)
		h.storeNativeSupport(model, path, true)
		if !previouslyKnown {
			slog.Info("backend supports native Responses API endpoint",
				"backend", model.Backend, "path", path)
		}
	}
	defer resp.Body.Close()

	slog.Info("proxying native responses request", "model", modelName, "path", path, "key", keyName)

	// For error responses, send a cleaned error with the right status/code.
	if resp.StatusCode >= 400 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, api.MaxResponseBodySize))
		slog.Error("upstream returned error (native passthrough)",
			"model", modelName, "status", resp.StatusCode, "body", string(errBody))
		writeUpstreamError(w, classifyUpstreamError(resp.StatusCode, errBody, resp.Header))
		logUsage(h.usage, usageLogInput{
			startTime: startTime, statusCode: resp.StatusCode, keyName: keyName, keyHash: keyHash,
			model: modelName, aliasFrom: aliasFrom, backend: model.Backend, endpoint: "/v1" + path,
			requestBytes: int64(len(body)), responseBytes: int64(len(errBody)),
		})
		return true
	}

	// Streaming bodies must outlive the per-model timeout: it guarded the
	// connect and the wait for headers, which have now succeeded.
	disarmIfStream(resp, disarm)

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
	logUsage(h.usage, usageLogInput{
		startTime: startTime, statusCode: resp.StatusCode, keyName: keyName, keyHash: keyHash,
		model: modelName, aliasFrom: aliasFrom, backend: model.Backend, endpoint: "/v1" + path,
		requestBytes: int64(len(body)), responseBytes: totalBytes,
	})

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
	requestedModel := req.Model
	req.Model = config.ResolveModelName(cfg, requestedModel)
	if req.Model != requestedModel {
		req.aliasFrom = requestedModel
		body = RewriteModelName(body, req.Model)
		slog.Info("resolved model alias", "model", req.Model, "alias_from", req.aliasFrom)
	}
	key := auth.KeyFromContext(r.Context())
	if !auth.KeyAllowsModel(key, req.Model) {
		httputil.WriteError(w, http.StatusForbidden, "not authorized for requested model")
		return
	}

	model := config.FindModel(cfg, req.Model)
	if model == nil {
		RecordUnknownRequest(r, requestedModel, "responses")
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

	// model.Timeout guards connect, the wait for response headers and
	// non-streaming bodies. It stays armed until the upstream has answered
	// with headers; a streaming exchange then disarms it, because a
	// wall-clock cap on a live stream severs healthy long generations (see
	// requestTimeout).
	ctx, cancel, disarmTimeout := armUntilHeaders(r.Context(), model)
	defer cancel()

	// Try native passthrough unless forced to translate.
	if !h.shouldTranslate(model, "/responses") {
		if h.tryNativePassthrough(ctx, disarmTimeout, w, r, body, req.Model, req.aliasFrom, model, "/responses", keyName, keyHash, startTime) {
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

	// Server-side conversation state cannot be honored over Chat Completions.
	if req.PreviousResponseID != "" {
		writeUpstreamError(w, upstreamError{Status: http.StatusBadRequest, Type: "invalid_request_error",
			Code:    "previous_response_id_not_supported",
			Message: "previous_response_id is not supported for this model; resend the full conversation in input"})
		return
	}
	if req.Store != nil && *req.Store {
		writeUpstreamError(w, upstreamError{Status: http.StatusBadRequest, Type: "invalid_request_error",
			Code:    "store_not_supported",
			Message: "store=true is not supported for this model; set store to false"})
		return
	}

	chatReq, err := prepareChatRequest(&req, model)
	if err != nil {
		slog.Error("responses input translation failed", "model", req.Model, "error", err)
		httputil.WriteError(w, http.StatusBadRequest, "request translation failed")
		return
	}

	// Run pipeline pre-send processors (vision, PDF, etc.).
	// For streaming requests, send SSE keepalives during processing to prevent
	// the client from timing out while the vision model describes images.
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
				writeFailedEvent(w, req.Model, "server_error", "server_error", "internal processing error")
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

	// PDFs the pipeline did not consume go upstream as OpenAI `file` parts.
	downgradePDFData(chatReq)
	// Per-model sampling defaults; values the client set are kept.
	model.ApplySamplingDefaults(chatReq)

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
		ue := upstreamError{Status: http.StatusBadGateway, Type: "server_error",
			Code: "upstream_unreachable", Message: "upstream request failed"}
		if ctx.Err() != nil {
			ue = upstreamError{Status: http.StatusGatewayTimeout, Type: "server_error",
				Code: "upstream_timeout", Message: "upstream request timed out"}
		}
		if r.Context().Err() == nil {
			lb.RecordOutcome(model.Backend, false)
		}
		slog.Error("upstream request failed", "error", err, "model", req.Model)
		respondUpstreamFailure(w, req.Model, headersAlreadySent, ue)
		return
	}
	defer resp.Body.Close()

	// Upstream error before any response byte: surface the real status with a
	// cleaned message and code (context_length_exceeded, rate_limit_exceeded,
	// ...), as a response.failed event only if a stream is already open.
	if resp.StatusCode >= 400 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, api.MaxResponseBodySize))
		slog.Error("upstream returned error for translated request",
			"model", req.Model, "status", resp.StatusCode, "body", string(errBody))
		respondUpstreamFailure(w, req.Model, headersAlreadySent, classifyUpstreamError(resp.StatusCode, errBody, resp.Header))
		logUsage(h.usage, usageLogInput{
			startTime: startTime, statusCode: resp.StatusCode, keyName: keyName, keyHash: keyHash,
			model: req.Model, aliasFrom: req.aliasFrom, backend: model.Backend, endpoint: "/v1/responses",
			requestBytes: int64(len(chatBody)), responseBytes: int64(len(errBody)),
		})
		return
	}

	disarmIfStream(resp, disarmTimeout)

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
	var respErr any

	if len(chatResp.Choices) > 0 {
		choice := chatResp.Choices[0]
		msg := choice.Message

		reasoningText := ""
		if r := msg.EffectiveReasoning(); r != nil {
			reasoningText = *r
		}

		var contentText string
		if msg.Content != nil && *msg.Content != "" {
			// Strip <think>...</think> tags from content; route to reasoning.
			thinkText, rest := stripThinkTags(*msg.Content)
			contentText = rest
			if thinkText != "" && reasoningText == "" {
				reasoningText = thinkText
			}
		}
		if req.jsonMode && len(msg.ToolCalls) == 0 {
			contentText = cleanStructuredOutput(contentText)
		}

		if reasoningText != "" {
			output = append(output, reasoningOutputItem(api.RandomID("rs_"), reasoningText))
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

		for _, tc := range msg.ToolCalls {
			output = append(output, toolCallOutputItem(req.tools, api.RandomID("fc_"), tc.ID, tc.Function.Name, tc.Function.Arguments, "completed"))
		}

		switch choice.FinishReason {
		case "length":
			status = "incomplete"
			incompleteDetails = map[string]any{"reason": "max_output_tokens"}
		case "content_filter":
			status = "failed"
			respErr = map[string]any{"code": "content_filter", "message": "the response was blocked by the backend's content filter"}
		}
	}

	usageObj := responsesUsage(chatResp.Usage)

	response := map[string]any{
		"id":                 respID,
		"object":             "response",
		"created_at":         createdAt,
		"model":              chatResp.Model,
		"status":             status,
		"error":              respErr,
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
		model: req.Model, aliasFrom: req.aliasFrom, endpoint: "/v1/responses",
		backend:      model.Backend,
		requestBytes: requestBytes, responseBytes: int64(len(body)),
	}, chatResp.Usage)
}

func (h *ResponsesHandler) sendChatRequest(ctx context.Context, chatReq map[string]any, model *config.ModelConfig) (*api.ChatResponse, error) {
	return sendChatCompletionsRequest(ctx, h.client, chatReq, model)
}

// reasoningHeading is the summary's first line for a backend's reasoning
// (which has no summary of its own): Codex shows it in its status line while
// the model thinks.
const reasoningHeading = "**Thinking**"

// reasoningSummary is the heading and then the reasoning: Codex lists it in
// the expanded transcript (Ctrl+T), not in the main view.
func reasoningSummary(text string) string {
	if body := strings.TrimSpace(text); body != "" {
		return reasoningHeading + "\n\n" + body
	}
	return reasoningHeading
}

// reasoningOutputItem builds a completed reasoning item: the summary as above
// and the text as reasoning_text content (what clients replay verbatim in
// later requests).
func reasoningOutputItem(id, text string) map[string]any {
	return map[string]any{
		"id":      id,
		"type":    "reasoning",
		"summary": []any{map[string]any{"type": "summary_text", "text": reasoningSummary(text)}},
		"content": []any{map[string]any{"type": "reasoning_text", "text": text}},
	}
}
