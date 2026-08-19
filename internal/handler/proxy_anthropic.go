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
	"go-llm-proxy/internal/config"
	"go-llm-proxy/internal/httputil"
	"go-llm-proxy/internal/lb"
)

const defaultAnthropicVersion = "2023-06-01"

// handleAnthropicChat dispatches an OpenAI Chat Completions request to an
// Anthropic Messages backend. It runs after the normal Chat pipeline and
// sampling-default processing, preserving those features across protocols.
func (p *ProxyHandler) handleAnthropicChat(
	w http.ResponseWriter,
	r *http.Request,
	body []byte,
	modelName, aliasFrom string,
	model *config.ModelConfig,
	cfg *config.Config,
	parsedChatReq map[string]any,
	searchEnabled bool,
	keyName, keyHash string,
	startTime time.Time,
) {
	anthropicReq, parsedReq, err := buildAnthropicRequestFromChat(body)
	if err != nil {
		slog.Error("anthropic chat request translation failed", "model", modelName, "error", err)
		httputil.WriteError(w, http.StatusBadRequest, "invalid chat completions request")
		return
	}
	anthropicBody, err := json.Marshal(anthropicReq)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to build upstream request")
		return
	}

	ctx, cancel, disarmTimeout := requestTimeout(r.Context(), model.Timeout)
	defer cancel()
	if parsedReq.Stream {
		disarmTimeout()
	}

	selected := model
	resp, err := sendAnthropicMessagesHTTP(ctx, p.client, anthropicBody, selected, r.Header, parsedReq.Stream)
	if (err != nil && ctx.Err() == nil) || (err == nil && resp.StatusCode >= 500) {
		lb.RecordOutcome(selected.Backend, false)
		if alternate, altRelease := lb.ResolveAlternate(cfg, selected, selected.Backend); alternate != nil {
			defer altRelease()
			slog.Warn("failing over translated anthropic chat request",
				"model", modelName, "failed", selected.Backend, "alternate", alternate.Backend, "error", err)
			if resp != nil {
				resp.Body.Close()
			}
			selected = alternate
			resp, err = sendAnthropicMessagesHTTP(ctx, p.client, anthropicBody, selected, r.Header, parsedReq.Stream)
		}
	}
	if err != nil {
		lb.RecordOutcome(selected.Backend, false)
		if ctx.Err() != nil {
			if r.Context().Err() == nil {
				httputil.WriteError(w, http.StatusGatewayTimeout, "upstream request timed out")
			}
			return
		}
		slog.Error("anthropic chat upstream request failed", "model", modelName, "error", err)
		httputil.WriteError(w, http.StatusBadGateway, "upstream request failed")
		return
	}
	defer resp.Body.Close()

	if requestID := resp.Header.Get("Request-Id"); requestID != "" {
		w.Header().Set("Request-Id", requestID)
	}
	if resp.StatusCode >= 400 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, api.MaxResponseBodySize))
		slog.Error("anthropic backend returned error for chat completions",
			"model", modelName, "status", resp.StatusCode, "body", string(errBody))
		httputil.WriteError(w, resp.StatusCode, fmt.Sprintf("backend returned HTTP %d", resp.StatusCode))
		logUsage(p.usage, usageLogInput{
			startTime: startTime, statusCode: resp.StatusCode,
			keyName: keyName, keyHash: keyHash,
			model: modelName, aliasFrom: aliasFrom, endpoint: "/v1/chat/completions",
			backend:      selected.Backend,
			requestBytes: int64(len(body)), responseBytes: int64(len(errBody)),
		})
		return
	}

	rc := proxyRequestContext{
		model: selected, modelName: modelName, aliasFrom: aliasFrom, endpoint: "/v1/chat/completions",
		requestBody: body, keyName: keyName, keyHash: keyHash, startTime: startTime,
	}
	includeUsage := chatStreamIncludesUsage(parsedReq.StreamOptions)

	if parsedReq.Stream {
		if searchEnabled {
			// Translate incrementally into a pipe, then reuse the normal OpenAI
			// stream search/tool-call machinery. The pipe preserves streaming;
			// it does not buffer the completion in memory.
			translatedReader, translatedWriter := io.Pipe()
			go func() {
				_, _ = streamAnthropicToChatSSE(translatedWriter, resp.Body, modelName, includeUsage, nil)
				translatedWriter.Close()
			}()
			defer translatedReader.Close()
			translatedResp := &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       translatedReader,
				Request:    resp.Request,
			}
			p.handleStreamingWithSearch(ctx, w, translatedResp, parsedChatReq, rc)
			return
		}

		httputil.SetSecurityHeaders(w)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		flush := func() {
			if flusher != nil {
				flusher.Flush()
			}
		}
		responseBytes, usageData := streamAnthropicToChatSSE(w, resp.Body, modelName, includeUsage, flush)
		logUsageFromChatResponse(p.usage, usageData, rc, responseBytes)
		return
	}

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, api.MaxResponseBodySize))
	if err != nil {
		httputil.WriteError(w, http.StatusBadGateway, "failed to read upstream response")
		return
	}
	chatResp, _, err := buildChatResponseFromAnthropic(respBody, modelName)
	if err != nil {
		slog.Error("anthropic chat response translation failed", "model", modelName, "error", err)
		httputil.WriteError(w, http.StatusBadGateway, "invalid upstream response")
		return
	}
	translatedBody, err := json.Marshal(chatResp)
	if err != nil {
		httputil.WriteError(w, http.StatusBadGateway, "invalid upstream response")
		return
	}
	translatedResp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(translatedBody)),
		Request:    resp.Request,
	}
	httputil.SetSecurityHeaders(w)
	if searchEnabled {
		p.handleNonStreamingWithSearch(w, translatedResp, parsedChatReq, rc)
	} else {
		p.handleNonStreamingChatWithFilter(w, translatedResp, rc)
	}
}

func sendAnthropicMessagesHTTP(
	ctx context.Context,
	client *http.Client,
	body []byte,
	model *config.ModelConfig,
	sourceHeaders http.Header,
	stream bool,
) (*http.Response, error) {
	upstreamURL := strings.TrimRight(model.Backend, "/") + "/v1/messages"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create anthropic messages request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	version := defaultAnthropicVersion
	if sourceHeaders != nil && sourceHeaders.Get("Anthropic-Version") != "" {
		version = sourceHeaders.Get("Anthropic-Version")
	}
	req.Header.Set("Anthropic-Version", version)
	if sourceHeaders != nil {
		for _, header := range []string{"Anthropic-Beta", "X-Request-ID"} {
			if value := sourceHeaders.Get(header); value != "" {
				req.Header.Set(header, value)
			}
		}
	}
	if model.APIKey != "" {
		req.Header.Set("X-Api-Key", model.APIKey)
	}
	return client.Do(req)
}

func chatStreamIncludesUsage(raw json.RawMessage) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return false
	}
	var options struct {
		IncludeUsage bool `json:"include_usage"`
	}
	return json.Unmarshal(raw, &options) == nil && options.IncludeUsage
}

// sendAnthropicChatCompletionsRequest is the non-streaming resend primitive
// used by proxy-managed search loops after the first translated response.
func sendAnthropicChatCompletionsRequest(
	ctx context.Context,
	client *http.Client,
	chatBody []byte,
	model *config.ModelConfig,
) (*api.ChatResponse, error) {
	anthropicReq, _, err := buildAnthropicRequestFromChat(chatBody)
	if err != nil {
		return nil, err
	}
	anthropicReq["stream"] = false
	anthropicBody, err := json.Marshal(anthropicReq)
	if err != nil {
		return nil, err
	}
	resp, err := sendAnthropicMessagesHTTP(ctx, client, anthropicBody, model, nil, false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, api.MaxResponseBodySize))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("anthropic backend returned HTTP %d", resp.StatusCode)
	}
	chatResp, _, err := buildChatResponseFromAnthropic(body, model.Name)
	return chatResp, err
}

// reStreamFromAnthropic is the streaming resend primitive used after the
// proxy executes a web_search tool call.
func (p *ProxyHandler) reStreamFromAnthropic(
	ctx context.Context,
	w http.ResponseWriter,
	chatBody []byte,
	model *config.ModelConfig,
	flush func(),
) {
	anthropicReq, parsedReq, err := buildAnthropicRequestFromChat(chatBody)
	if err != nil {
		emitTranslatedChatStreamError(w, flush)
		return
	}
	anthropicReq["stream"] = true
	anthropicBody, err := json.Marshal(anthropicReq)
	if err != nil {
		emitTranslatedChatStreamError(w, flush)
		return
	}
	resp, err := sendAnthropicMessagesHTTP(ctx, p.client, anthropicBody, model, nil, true)
	if err != nil {
		slog.Error("anthropic chat re-stream failed", "model", model.Name, "error", err)
		emitTranslatedChatStreamError(w, flush)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		emitTranslatedChatStreamError(w, flush)
		return
	}
	streamAnthropicToChatSSE(w, resp.Body, model.Name, chatStreamIncludesUsage(parsedReq.StreamOptions), flush)
}

func emitTranslatedChatStreamError(w io.Writer, flush func()) {
	if flush == nil {
		flush = func() {}
	}
	data, _ := json.Marshal(map[string]any{
		"error": map[string]any{"message": "upstream request failed", "type": "api_error"},
	})
	fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", data)
	flush()
}
