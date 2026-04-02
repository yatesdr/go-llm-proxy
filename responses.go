package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
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
	config *ConfigStore
	client *http.Client
	usage  *UsageLogger

	// nativeCache tracks which backend+path combinations support native
	// Responses API endpoints. Key: "backendURL\x00path", Value: bool.
	// Populated on first request per combination; never expires (restart to reset).
	nativeCache sync.Map
}

func NewResponsesHandler(cs *ConfigStore, usage *UsageLogger) *ResponsesHandler {
	return &ResponsesHandler{
		config: cs,
		usage:  usage,
		client: &http.Client{
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func nativeCacheKey(backend, path string) string { return backend + "\x00" + path }

// shouldTranslate returns true if this backend+path should skip the native
// probe and always use Chat Completions translation.
func (h *ResponsesHandler) shouldTranslate(model *ModelConfig, path string) bool {
	if model.ResponsesMode == ResponsesModeTranslate {
		return true
	}
	if model.ResponsesMode == ResponsesModeNative {
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
func (h *ResponsesHandler) shouldForceNative(model *ModelConfig) bool {
	return model.ResponsesMode == ResponsesModeNative
}

// tryNativePassthrough attempts to forward the request directly to the
// backend's native Responses API endpoint. Returns true if the backend
// handled the request (any status except 404). Returns false if the
// backend returned 404, meaning it does not support the endpoint —
// the caller should fall back to translation.
func (h *ResponsesHandler) tryNativePassthrough(ctx context.Context, w http.ResponseWriter, r *http.Request, body []byte, modelName string, model *ModelConfig, path, keyName, keyHash string, startTime time.Time) bool {
	if model.Model != modelName {
		body = rewriteModelName(body, model.Model)
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

	// Forward response headers.
	for k := range allowedResponseHeaders {
		if v := resp.Header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	setSecurityHeaders(w)
	w.WriteHeader(resp.StatusCode)

	// Stream response body.
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

	// Log usage (token counts unavailable for passthrough without buffering).
	if h.usage != nil {
		rec := UsageRecord{
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

// --- Request/response types ---

type responsesRequest struct {
	Model             string            `json:"model"`
	Input             json.RawMessage   `json:"input"`
	Instructions      string            `json:"instructions,omitempty"`
	Tools             []json.RawMessage `json:"tools,omitempty"`
	ToolChoice        json.RawMessage   `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool             `json:"parallel_tool_calls,omitempty"`
	Temperature       *float64          `json:"temperature,omitempty"`
	TopP              *float64          `json:"top_p,omitempty"`
	MaxOutputTokens   *int              `json:"max_output_tokens,omitempty"`
	Stream            bool              `json:"stream"`
	Reasoning         *reasoningConfig  `json:"reasoning,omitempty"`
	Text              json.RawMessage   `json:"text,omitempty"`
	User              string            `json:"user,omitempty"`
}

type reasoningConfig struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

// inputItem represents an item in the Responses API input array.
type inputItem struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	ID        string          `json:"id"`
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"` // function_call
	Input     string          `json:"input"`     // custom_tool_call
	Output    string          `json:"output"`    // *_output items
	Action    json.RawMessage `json:"action"`    // local_shell_call
	Status    string          `json:"status"`
}

// Chat Completions streaming chunk types.
type chatChunk struct {
	ID      string        `json:"id"`
	Model   string        `json:"model"`
	Choices []chunkChoice `json:"choices"`
	Usage   *chunkUsage   `json:"usage,omitempty"`
}

type chunkChoice struct {
	Index        int        `json:"index"`
	Delta        chunkDelta `json:"delta"`
	FinishReason *string    `json:"finish_reason"`
}

type chunkDelta struct {
	Role      string          `json:"role,omitempty"`
	Content   *string         `json:"content,omitempty"`
	Reasoning *string         `json:"reasoning,omitempty"`
	ToolCalls []chunkToolCall `json:"tool_calls,omitempty"`
}

type chunkToolCall struct {
	Index    int          `json:"index"`
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function *chunkToolFn `json:"function,omitempty"`
}

type chunkToolFn struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type chunkUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Chat Completions non-streaming response types.
type chatResponse struct {
	ID      string       `json:"id"`
	Model   string       `json:"model"`
	Created int64        `json:"created"`
	Choices []chatChoice `json:"choices"`
	Usage   *chunkUsage  `json:"usage,omitempty"`
}

type chatChoice struct {
	Index        int             `json:"index"`
	Message      chatChoiceMsg   `json:"message"`
	FinishReason string          `json:"finish_reason"`
}

type chatChoiceMsg struct {
	Role      string             `json:"role"`
	Content   *string            `json:"content"`
	ToolCalls []chatChoiceToolCall `json:"tool_calls,omitempty"`
}

type chatChoiceToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// --- ID generation ---

func randomID(prefix string) string {
	b := make([]byte, 12)
	rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

// --- Input translation (Responses API → Chat Completions) ---

// translateInput converts Responses API input items into Chat Completions messages.
func translateInput(input json.RawMessage, instructions string) ([]map[string]any, error) {
	var messages []map[string]any

	if instructions != "" {
		messages = append(messages, map[string]any{
			"role":    "system",
			"content": instructions,
		})
	}

	// String input: wrap as a single user message.
	var inputStr string
	if json.Unmarshal(input, &inputStr) == nil {
		messages = append(messages, map[string]any{
			"role":    "user",
			"content": inputStr,
		})
		return messages, nil
	}

	// Array input.
	var items []json.RawMessage
	if err := json.Unmarshal(input, &items); err != nil {
		return nil, fmt.Errorf("input must be a string or array")
	}

	for _, raw := range items {
		var item inputItem
		if json.Unmarshal(raw, &item) != nil {
			continue
		}

		switch {
		case item.Type == "function_call" || item.Type == "local_shell_call" || item.Type == "custom_tool_call":
			args := item.Arguments
			if args == "" && item.Input != "" {
				args = item.Input
			}
			if args == "" && len(item.Action) > 0 && string(item.Action) != "null" {
				args = string(item.Action)
			}
			if args == "" {
				args = "{}"
			}
			name := item.Name
			if name == "" && item.Type == "local_shell_call" {
				name = "shell"
			}

			tc := map[string]any{
				"id":   item.CallID,
				"type": "function",
				"function": map[string]any{
					"name":      name,
					"arguments": args,
				},
			}

			// Merge into the last message if it's an assistant message.
			merged := false
			if n := len(messages); n > 0 {
				last := messages[n-1]
				if last["role"] == "assistant" {
					existing, _ := last["tool_calls"].([]any)
					last["tool_calls"] = append(existing, tc)
					merged = true
				}
			}
			if !merged {
				messages = append(messages, map[string]any{
					"role":       "assistant",
					"content":    nil,
					"tool_calls": []any{tc},
				})
			}

		case item.Type == "function_call_output" || item.Type == "local_shell_call_output" || item.Type == "custom_tool_call_output":
			messages = append(messages, map[string]any{
				"role":         "tool",
				"tool_call_id": item.CallID,
				"content":      item.Output,
			})

		case item.Type == "reasoning" || item.Type == "compaction" ||
			item.Type == "tool_search_call" || item.Type == "tool_search_output" ||
			item.Type == "web_search_call" || item.Type == "image_generation_call":
			continue // no Chat Completions equivalent

		case item.Role != "":
			role := item.Role
			if role == "developer" {
				role = "system"
			}
			content := translateContentForChat(item.Content, item.Role)
			messages = append(messages, map[string]any{
				"role":    role,
				"content": content,
			})

		default:
			continue
		}
	}

	if len(messages) == 0 {
		return nil, fmt.Errorf("no valid input items")
	}
	return messages, nil
}

// translateContentForChat converts Responses API content to Chat Completions format.
func translateContentForChat(content json.RawMessage, role string) any {
	if len(content) == 0 || string(content) == "null" {
		return ""
	}

	// String content: pass through.
	var s string
	if json.Unmarshal(content, &s) == nil {
		return s
	}

	// Array of content parts.
	var parts []map[string]json.RawMessage
	if json.Unmarshal(content, &parts) != nil {
		return string(content)
	}

	// For assistant messages, extract text from output_text parts.
	if role == "assistant" {
		var texts []string
		for _, p := range parts {
			var partType string
			json.Unmarshal(p["type"], &partType)
			if partType == "output_text" {
				var text string
				json.Unmarshal(p["text"], &text)
				texts = append(texts, text)
			}
		}
		return strings.Join(texts, "")
	}

	// For user messages, translate input part types.
	var translated []map[string]any
	for _, p := range parts {
		var partType string
		json.Unmarshal(p["type"], &partType)

		switch partType {
		case "input_text":
			var text string
			json.Unmarshal(p["text"], &text)
			translated = append(translated, map[string]any{
				"type": "text",
				"text": text,
			})
		case "input_image":
			var url string
			json.Unmarshal(p["image_url"], &url)
			part := map[string]any{
				"type":      "image_url",
				"image_url": map[string]any{"url": url},
			}
			if detail, ok := p["detail"]; ok {
				var d string
				json.Unmarshal(detail, &d)
				part["image_url"].(map[string]any)["detail"] = d
			}
			translated = append(translated, part)
		default:
			var part map[string]any
			raw, _ := json.Marshal(p)
			json.Unmarshal(raw, &part)
			translated = append(translated, part)
		}
	}
	if len(translated) > 0 {
		return translated
	}
	return ""
}

// --- Tool translation ---

// translateTools converts Responses API tool definitions to Chat Completions format.
func translateTools(tools []json.RawMessage) []map[string]any {
	var result []map[string]any
	for _, raw := range tools {
		var tool struct {
			Type        string          `json:"type"`
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
			Strict      *bool           `json:"strict"`
		}
		if json.Unmarshal(raw, &tool) != nil || tool.Type != "function" {
			continue
		}
		fn := map[string]any{"name": tool.Name}
		if tool.Description != "" {
			fn["description"] = tool.Description
		}
		if len(tool.Parameters) > 0 {
			fn["parameters"] = json.RawMessage(tool.Parameters)
		}
		if tool.Strict != nil {
			fn["strict"] = *tool.Strict
		}
		result = append(result, map[string]any{
			"type":     "function",
			"function": fn,
		})
	}
	return result
}

// --- Text format translation ---

// translateTextFormat converts Responses API text.format to Chat Completions response_format.
func translateTextFormat(text json.RawMessage) json.RawMessage {
	if len(text) == 0 {
		return nil
	}
	var tf struct {
		Format struct {
			Type   string          `json:"type"`
			Name   string          `json:"name,omitempty"`
			Schema json.RawMessage `json:"schema,omitempty"`
			Strict *bool           `json:"strict,omitempty"`
		} `json:"format"`
	}
	if json.Unmarshal(text, &tf) != nil {
		return nil
	}
	switch tf.Format.Type {
	case "json_schema":
		schema := map[string]any{"name": tf.Format.Name}
		if len(tf.Format.Schema) > 0 {
			schema["schema"] = json.RawMessage(tf.Format.Schema)
		}
		if tf.Format.Strict != nil {
			schema["strict"] = *tf.Format.Strict
		}
		result, _ := json.Marshal(map[string]any{
			"type":        "json_schema",
			"json_schema": schema,
		})
		return result
	case "json_object":
		result, _ := json.Marshal(map[string]any{"type": "json_object"})
		return result
	}
	return nil // "text" format needs no response_format
}

// --- Chat Completions request builder ---

func buildChatRequest(req responsesRequest, backendModel string, messages []map[string]any) map[string]any {
	chatReq := map[string]any{
		"model":    backendModel,
		"messages": messages,
		"stream":   req.Stream,
	}
	if req.Stream {
		chatReq["stream_options"] = map[string]any{"include_usage": true}
	}
	if len(req.Tools) > 0 {
		if tools := translateTools(req.Tools); len(tools) > 0 {
			chatReq["tools"] = tools
		}
	}
	if len(req.ToolChoice) > 0 && string(req.ToolChoice) != "null" {
		chatReq["tool_choice"] = json.RawMessage(req.ToolChoice)
	}
	if req.ParallelToolCalls != nil {
		chatReq["parallel_tool_calls"] = *req.ParallelToolCalls
	}
	if req.Temperature != nil {
		chatReq["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		chatReq["top_p"] = *req.TopP
	}
	if req.MaxOutputTokens != nil {
		chatReq["max_completion_tokens"] = *req.MaxOutputTokens
	}
	if req.Reasoning != nil && req.Reasoning.Effort != "" {
		chatReq["reasoning_effort"] = req.Reasoning.Effort
	}
	if len(req.Text) > 0 {
		if rf := translateTextFormat(req.Text); rf != nil {
			chatReq["response_format"] = json.RawMessage(rf)
		}
	}
	if req.User != "" {
		chatReq["user"] = req.User
	}
	return chatReq
}

// --- Handler ---

func (h *ResponsesHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)
	body, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}

	var req responsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON request body")
		return
	}
	if req.Model == "" {
		writeError(w, http.StatusBadRequest, "missing model field in request")
		return
	}

	cfg := h.config.Get()
	key := keyFromContext(r.Context())
	if !keyAllowsModel(key, req.Model) {
		writeError(w, http.StatusForbidden, "not authorized for requested model")
		return
	}

	model := findModel(cfg, req.Model)
	if model == nil {
		writeError(w, http.StatusNotFound, "unknown model")
		return
	}
	if model.Type == BackendAnthropic {
		writeError(w, http.StatusBadRequest, "responses API is not supported for anthropic backends")
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

	// Try native passthrough unless forced to translate.
	if !h.shouldTranslate(model, "/responses") {
		if h.tryNativePassthrough(ctx, w, r, body, req.Model, model, "/responses", keyName, keyHash, startTime) {
			return
		}
		// For native mode, don't fall back to translation — the backend must handle it.
		if h.shouldForceNative(model) {
			writeError(w, http.StatusBadGateway, "backend failed native responses passthrough and responses_mode is set to native")
			return
		}
	}

	// Translate Responses API → Chat Completions.
	slog.Info("proxying responses request (translated)", "model", req.Model, "key", keyName)

	messages, err := translateInput(req.Input, req.Instructions)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid input: "+err.Error())
		return
	}

	chatReq := buildChatRequest(req, model.Model, messages)
	chatBody, err := json.Marshal(chatReq)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to build upstream request")
		return
	}

	upstreamURL := strings.TrimRight(model.Backend, "/") + "/chat/completions"

	upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(chatBody))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create upstream request")
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
			writeError(w, http.StatusGatewayTimeout, "upstream request timed out")
			return
		}
		slog.Error("upstream request failed", "error", err, "model", req.Model)
		writeError(w, http.StatusBadGateway, "upstream request failed")
		return
	}
	defer resp.Body.Close()

	// Forward upstream error responses.
	if resp.StatusCode >= 400 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodySize))
		slog.Error("upstream returned error for translated request",
			"model", req.Model, "status", resp.StatusCode, "body", string(errBody))

		if req.Stream {
			// Client expects SSE. Wrap the error as a response.failed event
			// so Codex sees a proper terminal event instead of a raw JSON body
			// on a connection it expected to be SSE.
			setSecurityHeaders(w)
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.WriteHeader(http.StatusOK)
			failedData, _ := json.Marshal(map[string]any{
				"type": "response.failed",
				"response": map[string]any{
					"id":         randomID("resp_"),
					"object":     "response",
					"created_at": float64(time.Now().Unix()),
					"model":      req.Model,
					"status":     "failed",
					"error": map[string]any{
						"type":    "upstream_error",
						"message": fmt.Sprintf("backend returned %d: %s", resp.StatusCode, string(errBody)),
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

		setSecurityHeaders(w)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		w.Write(errBody)
		return
	}

	reqBytes := int64(len(chatBody))
	if req.Stream {
		h.handleStreaming(w, resp, req, reqBytes, keyName, keyHash, startTime)
	} else {
		h.handleNonStreaming(w, resp, req, reqBytes, keyName, keyHash, startTime)
	}
}

// --- Non-streaming handler ---

func (h *ResponsesHandler) handleNonStreaming(w http.ResponseWriter, resp *http.Response, req responsesRequest, requestBytes int64, keyName, keyHash string, startTime time.Time) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodySize))
	if err != nil {
		writeError(w, http.StatusBadGateway, "failed to read upstream response")
		return
	}

	var chatResp chatResponse
	if err := json.Unmarshal(body, &chatResp); err != nil {
		writeError(w, http.StatusBadGateway, "invalid upstream response")
		return
	}

	respID := randomID("resp_")
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
				"id":     randomID("msg_"),
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
				"id":        randomID("fc_"),
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

	setSecurityHeaders(w)
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

func (h *ResponsesHandler) handleStreaming(w http.ResponseWriter, resp *http.Response, req responsesRequest, requestBytes int64, keyName, keyHash string, startTime time.Time) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	upstreamCT := resp.Header.Get("Content-Type")
	slog.Debug("streaming handler entered",
		"model", req.Model, "upstream_status", resp.StatusCode, "upstream_content_type", upstreamCT)

	setSecurityHeaders(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	respID := randomID("resp_")
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
	var usage *chunkUsage
	createdEmitted := false

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
		msgID = randomID("msg_")
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
		if responseBytes > maxResponseBodySize {
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

		var chunk chatChunk
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
			usage = chunk.Usage
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

				itemID := randomID("fc_")
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

				emit("response.output_item.added", map[string]any{
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
				})
				seq++
				outputIdx++
			}

			if tc.Function != nil && tc.Function.Arguments != "" {
				if tc.Index < len(toolCalls) && toolCalls[tc.Index] != nil {
					tcs := toolCalls[tc.Index]
					tcs.args.WriteString(tc.Function.Arguments)
					emit("response.function_call_arguments.delta", map[string]any{
						"delta":           tc.Function.Arguments,
						"item_id":         tcs.itemID,
						"output_index":    tcs.outputIdx,
						"sequence_number": seq,
					})
					seq++
				}
			}
		}

		// Finish reason — finalize all pending items.
		if choice.FinishReason != nil {
			finishReason = *choice.FinishReason
			if msgStarted {
				finishMsg()
			}
			finishToolCalls()
		}
	}

	// Emit the terminal event.
	if !createdEmitted {
		slog.Error("streaming handler received no valid chunks from upstream",
			"model", req.Model, "response_bytes", responseBytes,
			"scanner_error", scanner.Err())
	}
	if createdEmitted {
		if finishReason == "" {
			if msgStarted {
				finishMsg()
			}
			finishToolCalls()
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
		if usage != nil {
			usageObj = map[string]any{
				"input_tokens":  usage.PromptTokens,
				"output_tokens": usage.CompletionTokens,
				"total_tokens":  usage.TotalTokens,
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

	h.logUsage(usage, resp.StatusCode, req.Model, requestBytes, responseBytes, keyName, keyHash, startTime)
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
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)
	body, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}

	var req responsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON request body")
		return
	}
	if req.Model == "" {
		writeError(w, http.StatusBadRequest, "missing model field")
		return
	}

	cfg := h.config.Get()
	key := keyFromContext(r.Context())
	if !keyAllowsModel(key, req.Model) {
		writeError(w, http.StatusForbidden, "not authorized for requested model")
		return
	}
	model := findModel(cfg, req.Model)
	if model == nil {
		writeError(w, http.StatusNotFound, "unknown model")
		return
	}
	if model.Type == BackendAnthropic {
		writeError(w, http.StatusBadRequest, "compaction is not supported for anthropic backends")
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

	// Try native passthrough unless forced to translate.
	if !h.shouldTranslate(model, "/responses/compact") {
		if h.tryNativePassthrough(ctx, w, r, body, req.Model, model, "/responses/compact", keyName, keyHash, startTime) {
			return
		}
		if h.shouldForceNative(model) {
			writeError(w, http.StatusBadGateway, "backend failed native compact passthrough and responses_mode is set to native")
			return
		}
	}

	// Fall back to model-based summarization.
	slog.Info("proxying compact request (summarizing)", "model", req.Model, "key", keyName)

	messages, err := translateInput(req.Input, req.Instructions)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid input: "+err.Error())
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
		writeError(w, http.StatusInternalServerError, "failed to build upstream request")
		return
	}

	upstreamURL := strings.TrimRight(model.Backend, "/") + "/chat/completions"

	upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(chatBody))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create upstream request")
		return
	}
	upReq.Header.Set("Content-Type", "application/json")
	if model.APIKey != "" {
		upReq.Header.Set("Authorization", "Bearer "+model.APIKey)
	}

	resp, err := h.client.Do(upReq)
	if err != nil {
		if ctx.Err() != nil {
			writeError(w, http.StatusGatewayTimeout, "upstream request timed out")
			return
		}
		slog.Error("compact upstream failed", "error", err, "model", req.Model)
		writeError(w, http.StatusBadGateway, "upstream request failed")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodySize))
		setSecurityHeaders(w)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		w.Write(errBody)
		return
	}

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodySize))
	if err != nil {
		writeError(w, http.StatusBadGateway, "failed to read upstream response")
		return
	}

	var chatResp chatResponse
	if err := json.Unmarshal(respBody, &chatResp); err != nil {
		writeError(w, http.StatusBadGateway, "invalid upstream response")
		return
	}

	summaryText := ""
	if len(chatResp.Choices) > 0 && chatResp.Choices[0].Message.Content != nil {
		summaryText = *chatResp.Choices[0].Message.Content
	}

	// Build compacted output: preserved user messages + summary as assistant message.
	output := extractUserMessages(req.Input)
	output = append(output, map[string]any{
		"id":     randomID("msg_"),
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
		"id":         randomID("resp_"),
		"object":     "response.compaction",
		"created_at": float64(time.Now().Unix()),
		"output":     output,
		"usage":      usageObj,
	}

	setSecurityHeaders(w)
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

// --- Usage logging ---

func (h *ResponsesHandler) logUsage(usage *chunkUsage, statusCode int, model string, requestBytes, responseBytes int64, keyName, keyHash string, startTime time.Time) {
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
