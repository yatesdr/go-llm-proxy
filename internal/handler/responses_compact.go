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
	"time"

	"go-llm-proxy/internal/api"
	"go-llm-proxy/internal/auth"
	"go-llm-proxy/internal/config"
	"go-llm-proxy/internal/httputil"
	"go-llm-proxy/internal/usage"
)

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
		slog.Error("compact input translation failed", "model", req.Model, "error", err)
		httputil.WriteError(w, http.StatusBadRequest, "request translation failed")
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
		slog.Error("compact upstream returned error",
			"model", req.Model, "status", resp.StatusCode, "body", string(errBody))
		// Sanitized error: never forward raw backend error bodies to the client.
		// They may contain internal URLs, backend API keys, or infrastructure details.
		httputil.WriteError(w, resp.StatusCode, fmt.Sprintf("backend returned HTTP %d", resp.StatusCode))
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

	logUsageChat(h.usage, usageLogInput{
		startTime: startTime, statusCode: resp.StatusCode,
		keyName: keyName, keyHash: keyHash,
		model: req.Model, endpoint: "/v1/responses/compact",
		requestBytes: int64(len(chatBody)), responseBytes: int64(len(respBody)),
	}, chatResp.Usage)
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
