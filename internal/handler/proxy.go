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

type ProxyHandler struct {
	config   *config.ConfigStore
	client   *http.Client
	usage    *usage.UsageLogger // nil if logging disabled
	pipeline *pipeline.Pipeline
}

func NewProxyHandler(cs *config.ConfigStore, usage *usage.UsageLogger, pipeline *pipeline.Pipeline) *ProxyHandler {
	return &ProxyHandler{
		config:   cs,
		usage:    usage,
		pipeline: pipeline,
		client:   httputil.NewHTTPClient(),
	}
}

func (p *ProxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Only allow POST — all proxied endpoints are POST.
	if r.Method != http.MethodPost {
		httputil.WriteError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	// Check for /anthropic prefix — requests via this path must route to anthropic backends.
	cleanPath := path.Clean(r.URL.Path)
	requireAnthropic := false
	if strings.HasPrefix(cleanPath, "/anthropic/") {
		cleanPath = strings.TrimPrefix(cleanPath, "/anthropic")
		requireAnthropic = true
	}

	// Validate the request path against the allowlist.
	if !AllowedPaths.MatchString(cleanPath) {
		httputil.WriteError(w, http.StatusNotFound, "unsupported endpoint")
		return
	}

	// Limit request body size to prevent memory exhaustion.
	r.Body = http.MaxBytesReader(w, r.Body, api.MaxRequestBodySize)
	body, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		httputil.WriteError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}

	// Detect content type for model extraction strategy.
	contentType := r.Header.Get("Content-Type")
	isMultipart := strings.HasPrefix(contentType, "multipart/form-data")

	var modelName string
	if isMultipart {
		modelName = ExtractModelFromMultipart(body, contentType)
	} else {
		modelName = ExtractModelFromJSON(body)
	}
	if modelName == "" {
		httputil.WriteError(w, http.StatusBadRequest, "missing model field in request")
		return
	}

	// Snapshot config once to avoid race on reload.
	cfg := p.config.Get()

	// Check key authorization for this model.
	key := auth.KeyFromContext(r.Context())
	if !auth.KeyAllowsModel(key, modelName) {
		httputil.WriteError(w, http.StatusForbidden, "not authorized for requested model")
		return
	}

	model := config.FindModel(cfg, modelName)
	if model == nil {
		httputil.WriteError(w, http.StatusNotFound, "unknown model")
		return
	}

	// Requests via /anthropic/ must only route to anthropic-type backends.
	if requireAnthropic && model.Type != config.BackendAnthropic {
		httputil.WriteError(w, http.StatusBadRequest, "model is not an anthropic backend")
		return
	}

	// Rewrite the model name in the body if the backend expects a different name.
	if model.Model != modelName {
		if isMultipart {
			body = RewriteModelInMultipart(body, contentType, model.Model)
		} else {
			body = RewriteModelName(body, model.Model)
		}
	}

	// Pipeline: selective parse for Chat Completions requests with processable content.
	if p.pipeline != nil && cleanPath == "/v1/chat/completions" && !isMultipart &&
		p.pipeline.BodyNeedsProcessing(body) {
		var chatReq map[string]any
		if err := json.Unmarshal(body, &chatReq); err != nil {
			slog.Warn("pipeline: failed to parse request body for processing", "error", err)
		} else {
			processed, pErr := p.pipeline.ProcessRequest(r.Context(), chatReq, model)
			if pErr != nil {
				slog.Warn("pipeline: processing failed, sending original request", "error", pErr)
			} else {
				newBody, mErr := json.Marshal(processed)
				if mErr != nil {
					slog.Error("pipeline: failed to re-marshal processed request", "error", mErr)
				} else {
					body = newBody
				}
			}
		}
	}

	// Build the upstream URL.
	// OpenAI backends include /v1 in their base URL, so strip /v1 from the client path.
	// Anthropic backends omit /v1 from their base URL (the SDK adds it), so keep the full path.
	relPath := cleanPath
	if model.Type != config.BackendAnthropic {
		relPath = strings.TrimPrefix(cleanPath, "/v1")
	}
	upstreamURL := strings.TrimRight(model.Backend, "/") + relPath

	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(model.Timeout)*time.Second)
	defer cancel()

	upReq, err := http.NewRequestWithContext(ctx, r.Method, upstreamURL, bytes.NewReader(body))
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to create upstream request")
		return
	}

	// Copy only specific headers from the client.
	copyHeaders(upReq.Header, r.Header, model.Type)

	// Set the backend API key using the appropriate auth scheme.
	if model.APIKey != "" {
		if model.Type == config.BackendAnthropic {
			upReq.Header.Set("X-Api-Key", model.APIKey)
		} else {
			upReq.Header.Set("Authorization", "Bearer "+model.APIKey)
		}
	}

	keyName := ""
	keyHash := ""
	if key != nil {
		keyName = key.Name
		keyHash = usage.HashKey(key.Key)
	}
	slog.Info("proxying request",
		"model", modelName,
		"path", cleanPath,
		"key", keyName,
	)

	startTime := time.Now()

	resp, err := p.client.Do(upReq)
	if err != nil {
		if ctx.Err() != nil {
			httputil.WriteError(w, http.StatusGatewayTimeout, "upstream request timed out")
			return
		}
		slog.Error("upstream request failed", "error", err, "model", modelName)
		httputil.WriteError(w, http.StatusBadGateway, "upstream request failed")
		return
	}
	defer resp.Body.Close()

	// Detect if this is a streaming response (SSE).
	isStreaming := strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")

	// Copy only allowed response headers.
	for k := range AllowedResponseHeaders {
		if v := resp.Header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}

	httputil.SetSecurityHeaders(w)

	// For error responses, sanitize before sending to the client.
	// Backend error bodies may contain internal URLs, API keys, or infrastructure details.
	if resp.StatusCode >= 400 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, api.MaxResponseBodySize))
		slog.Error("upstream returned error", "model", modelName, "status", resp.StatusCode,
			"body", string(errBody))
		httputil.WriteError(w, resp.StatusCode, fmt.Sprintf("backend returned HTTP %d", resp.StatusCode))

		if p.usage != nil {
			rec := usage.UsageRecord{
				Timestamp: startTime, KeyHash: keyHash, KeyName: keyName,
				Model: modelName, Endpoint: cleanPath, StatusCode: resp.StatusCode,
				RequestBytes: int64(len(body)), ResponseBytes: int64(len(errBody)),
				DurationMS: time.Since(startTime).Milliseconds(),
			}
			go p.usage.Log(rec)
		}
		return
	}

	w.WriteHeader(resp.StatusCode)

	// Stream the response, flushing after each read for SSE support.
	// If usage logging is enabled, tee the response into a buffer for token extraction.
	flusher, canFlush := w.(http.Flusher)
	buf := make([]byte, 4096)
	var totalBytes int64
	var responseBuf bytes.Buffer
	captureResponse := p.usage != nil
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			totalBytes += int64(n)
			if totalBytes > api.MaxResponseBodySize {
				slog.Error("upstream response exceeded size limit", "model", modelName, "bytes", totalBytes)
				captureResponse = false
				break
			}
			if captureResponse {
				responseBuf.Write(buf[:n])
			}
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				break
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if readErr != nil {
			break
		}
	}

	duration := time.Since(startTime)

	// Log usage metrics asynchronously if enabled.
	if p.usage != nil {
		backendType := ""
		if model.Type == config.BackendAnthropic {
			backendType = config.BackendAnthropic
		}
		var tokens usage.TokenUsage
		if captureResponse {
			tokens = usage.ExtractTokenUsage(responseBuf.Bytes(), backendType, isStreaming)
		}
		rec := usage.UsageRecord{
			Timestamp:     startTime,
			KeyHash:       keyHash,
			KeyName:       keyName,
			Model:         modelName,
			Endpoint:      cleanPath,
			StatusCode:    resp.StatusCode,
			RequestBytes:  int64(len(body)),
			ResponseBytes: totalBytes,
			InputTokens:   tokens.InputTokens,
			OutputTokens:  tokens.OutputTokens,
			TotalTokens:   tokens.TotalTokens,
			DurationMS:    duration.Milliseconds(),
		}
		go p.usage.Log(rec)
	}
}
