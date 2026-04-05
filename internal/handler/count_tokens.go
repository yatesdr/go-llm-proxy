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

// CountTokensHandler handles POST /v1/messages/count_tokens.
// For native Anthropic backends, it proxies the request through.
// For translated (Chat Completions) backends, it returns a rough estimate
// based on character count so Claude Code's context management works.
type CountTokensHandler struct {
	config *config.ConfigStore
	client *http.Client
	usage  *usage.UsageLogger
}

func NewCountTokensHandler(cs *config.ConfigStore, ul *usage.UsageLogger) *CountTokensHandler {
	return &CountTokensHandler{
		config: cs,
		usage:  ul,
		client: httputil.NewHTTPClient(),
	}
}

// roughTokenEstimate estimates tokens from a JSON body using ~4 chars/token.
// This matches Claude Code's own roughTokenCountEstimation fallback.
func roughTokenEstimate(body []byte) int {
	return len(body) / 4
}

func (h *CountTokensHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httputil.WriteAnthropicError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, api.MaxRequestBodySize)
	body, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		httputil.WriteAnthropicError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "request body too large")
		return
	}

	var req struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &req) != nil || req.Model == "" {
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

	// Native Anthropic backends: proxy through to the real count_tokens endpoint.
	if model.Type == config.BackendAnthropic {
		h.proxyNative(r.Context(), w, r, body, model)
		return
	}

	// Translated backends: return a rough estimate.
	tokens := roughTokenEstimate(body)
	slog.Debug("count_tokens: returning rough estimate for translated backend",
		"model", req.Model, "chars", len(body), "estimated_tokens", tokens)

	httputil.SetSecurityHeaders(w)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"input_tokens": tokens,
	})
}

func (h *CountTokensHandler) proxyNative(ctx context.Context, w http.ResponseWriter, r *http.Request, body []byte, model *config.ModelConfig) {
	if model.Model != "" {
		body = RewriteModelName(body, model.Model)
	}

	upstreamURL := strings.TrimRight(model.Backend, "/") + "/v1/messages/count_tokens"

	ctx, cancel := context.WithTimeout(ctx, time.Duration(model.Timeout)*time.Second)
	defer cancel()

	upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(body))
	if err != nil {
		httputil.WriteAnthropicError(w, http.StatusInternalServerError, "api_error", "failed to create upstream request")
		return
	}

	copyHeaders(upReq.Header, r.Header, config.BackendAnthropic)
	if model.APIKey != "" {
		upReq.Header.Set("X-Api-Key", model.APIKey)
	}

	resp, err := h.client.Do(upReq)
	if err != nil {
		if ctx.Err() != nil {
			httputil.WriteAnthropicError(w, http.StatusGatewayTimeout, "api_error", "upstream request timed out")
			return
		}
		slog.Error("count_tokens upstream request failed", "error", err)
		httputil.WriteAnthropicError(w, http.StatusBadGateway, "api_error", "upstream request failed")
		return
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, api.MaxResponseBodySize))

	if resp.StatusCode >= 400 {
		slog.Error("count_tokens upstream error", "status", resp.StatusCode, "body", string(respBody))
		httputil.WriteAnthropicError(w, resp.StatusCode, "api_error",
			fmt.Sprintf("backend returned HTTP %d", resp.StatusCode))
		return
	}

	copyResponseHeaders(w, resp)
	httputil.SetSecurityHeaders(w)
	w.WriteHeader(resp.StatusCode)
	w.Write(respBody)
}
