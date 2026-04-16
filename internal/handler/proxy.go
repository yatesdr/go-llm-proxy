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

// proxyRequestContext bundles the per-request metadata that flows through the
// proxy handler methods, avoiding 10+ parameter sprawl.
type proxyRequestContext struct {
	model       *config.ModelConfig
	modelName   string
	endpoint    string
	requestBody []byte
	keyName     string
	keyHash     string
	startTime   time.Time
}

func (p *ProxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httputil.WriteError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	cleanPath := path.Clean(r.URL.Path)
	requireAnthropic := false
	if strings.HasPrefix(cleanPath, "/anthropic/") {
		cleanPath = strings.TrimPrefix(cleanPath, "/anthropic")
		requireAnthropic = true
	}

	if !AllowedPaths.MatchString(cleanPath) {
		httputil.WriteError(w, http.StatusNotFound, "unsupported endpoint")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, api.MaxRequestBodySize)
	body, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		httputil.WriteError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}

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

	cfg := p.config.Get()

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

	if requireAnthropic && model.Type != config.BackendAnthropic {
		httputil.WriteError(w, http.StatusBadRequest, "model is not an anthropic backend")
		return
	}

	if model.Model != modelName {
		if isMultipart {
			body = RewriteModelInMultipart(body, contentType, model.Model)
		} else {
			body = RewriteModelName(body, model.Model)
		}
	}

	// AWS Bedrock backends use the Converse API (translated from Chat
	// Completions, signed with SigV4 or a Bedrock API key). Dispatched here
	// because the wire protocol diverges entirely from the standard OpenAI-
	// compatible passthrough below. Implementation in proxy_bedrock.go.
	if model.Type == config.BackendBedrock {
		if cleanPath != "/v1/chat/completions" {
			httputil.WriteError(w, http.StatusBadRequest,
				"bedrock models only support /v1/chat/completions on this endpoint; use /v1/messages for the Anthropic protocol")
			return
		}
		if isMultipart {
			httputil.WriteError(w, http.StatusBadRequest, "multipart requests are not supported for bedrock backends")
			return
		}
		keyName := ""
		keyHash := ""
		if key != nil {
			keyName = key.Name
			keyHash = usage.HashKey(key.Key)
		}
		ctx, cancel := context.WithTimeout(r.Context(), time.Duration(model.Timeout)*time.Second)
		defer cancel()
		p.handleBedrockChat(ctx, w, body, modelName, model, keyName, keyHash, r.Header.Get("X-Request-ID"), time.Now())
		return
	}

	// Pipeline + search: parse body once for both if needed.
	isChatCompletions := cleanPath == "/v1/chat/completions" && !isMultipart
	var parsedChatReq map[string]any

	if p.pipeline != nil && isChatCompletions && p.pipeline.BodyNeedsProcessing(body) {
		if err := json.Unmarshal(body, &parsedChatReq); err != nil {
			slog.Warn("pipeline: failed to parse request body for processing", "error", err)
		} else {
			processed, pErr := p.pipeline.ProcessRequest(r.Context(), parsedChatReq, model)
			if pErr != nil {
				slog.Warn("pipeline: processing failed, sending original request", "error", pErr)
			} else {
				parsedChatReq = processed
				if newBody, mErr := json.Marshal(processed); mErr != nil {
					slog.Error("pipeline: failed to re-marshal processed request", "error", mErr)
				} else {
					body = newBody
				}
			}
		}
	}

	// Check if post-response search is possible. Reuse already-parsed body if available.
	searchEnabled := false
	if p.pipeline != nil && isChatCompletions && p.pipeline.ResolveWebSearchKey(model) != "" {
		if parsedChatReq != nil {
			searchEnabled = true
		} else if err := json.Unmarshal(body, &parsedChatReq); err == nil {
			searchEnabled = true
		}
	}

	// Apply model's default sampling parameters to Chat Completions requests.
	if isChatCompletions && model.Defaults != nil {
		if parsedChatReq == nil {
			if err := json.Unmarshal(body, &parsedChatReq); err != nil {
				slog.Warn("failed to parse chat request for defaults", "error", err)
			}
		}
		if parsedChatReq != nil {
			model.ApplySamplingDefaults(parsedChatReq)
			if newBody, err := json.Marshal(parsedChatReq); err == nil {
				body = newBody
			}
		}
	}

	// Build the upstream URL.
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

	copyHeaders(upReq.Header, r.Header, model.Type)

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
	slog.Info("proxying request", "model", modelName, "path", cleanPath, "key", keyName)

	startTime := time.Now()
	rc := proxyRequestContext{
		model: model, modelName: modelName, endpoint: cleanPath,
		requestBody: body, keyName: keyName, keyHash: keyHash, startTime: startTime,
	}

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

	isStreaming := strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")

	copyResponseHeaders(w, resp)
	httputil.SetSecurityHeaders(w)

	if resp.StatusCode >= 400 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, api.MaxResponseBodySize))
		slog.Error("upstream returned error", "model", modelName, "status", resp.StatusCode,
			"body", string(errBody))
		httputil.WriteError(w, resp.StatusCode, fmt.Sprintf("backend returned HTTP %d", resp.StatusCode))

		logUsage(p.usage, usageLogInput{
			startTime: startTime, statusCode: resp.StatusCode,
			keyName: keyName, keyHash: keyHash,
			model: modelName, endpoint: cleanPath,
			requestBytes: int64(len(body)), responseBytes: int64(len(errBody)),
		})
		return
	}

	if searchEnabled && !isStreaming {
		p.handleNonStreamingWithSearch(w, resp, parsedChatReq, rc)
		return
	}
	if searchEnabled && isStreaming {
		p.handleStreamingWithSearch(ctx, w, resp, parsedChatReq, rc)
		return
	}

	// For Chat Completions (without search), filter <think> tags from content.
	if isChatCompletions && isStreaming {
		usageData := streamChatWithThinkFilter(w, resp)
		logUsageFromChatResponse(p.usage, usageData, rc, 0)
		return
	}
	if isChatCompletions && !isStreaming {
		p.handleNonStreamingChatWithFilter(w, resp, rc)
		return
	}

	w.WriteHeader(resp.StatusCode)
	p.streamRawResponse(w, resp, rc, isStreaming)
}

// maxUsageCaptureBytes bounds how much of the upstream response body we
// keep in memory for token-usage extraction. Non-streaming responses carry
// usage near the top of the JSON; streaming responses carry the include_usage
// chunk near the end. Capturing the full body (up to MaxResponseBodySize =
// 100 MB) into a buffer per request doesn't scale under concurrency — a
// handful of parallel large streams can OOM the process.
//
// For non-streaming, 1 MB is well beyond any realistic LLM response (even
// 100K tokens of JSON is ~400 KB).
//
// For streaming, we capture both the prefix (first chunk often has model
// metadata) and the tail (final chunk has usage), which is handled below
// via a head+tail ring strategy.
const (
	maxUsageCaptureBytes   = 1 * 1024 * 1024
	streamTailCaptureBytes = 64 * 1024
)

// streamRawResponse streams the upstream response to the client without parsing.
func (p *ProxyHandler) streamRawResponse(w http.ResponseWriter, resp *http.Response,
	rc proxyRequestContext, isStreaming bool) {

	flusher, canFlush := w.(http.Flusher)
	buf := make([]byte, 4096)
	var totalBytes int64
	capture := newCaptureBuffer(isStreaming)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			totalBytes += int64(n)
			if totalBytes > api.MaxResponseBodySize {
				slog.Error("upstream response exceeded size limit", "model", rc.modelName, "bytes", totalBytes)
				capture.discard()
				break
			}
			if p.usage != nil {
				capture.write(buf[:n])
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

	if p.usage != nil {
		var tokens usage.TokenUsage
		if body := capture.bytes(); body != nil {
			tokens = usage.ExtractTokenUsage(body, rc.model.Type, isStreaming)
		}
		rec := usage.UsageRecord{
			Timestamp:     rc.startTime,
			KeyHash:       rc.keyHash,
			KeyName:       rc.keyName,
			Model:         rc.modelName,
			Endpoint:      rc.endpoint,
			StatusCode:    resp.StatusCode,
			RequestBytes:  int64(len(rc.requestBody)),
			ResponseBytes: totalBytes,
			InputTokens:   tokens.InputTokens,
			OutputTokens:  tokens.OutputTokens,
			TotalTokens:   tokens.TotalTokens,
			DurationMS:    time.Since(rc.startTime).Milliseconds(),
		}
		go p.usage.Log(rec)
	}
}

// handleNonStreamingWithSearch parses a non-streaming Chat Completions response,
// detects web_search tool calls, executes them, and re-sends until a final response.
func (p *ProxyHandler) handleNonStreamingWithSearch(w http.ResponseWriter, resp *http.Response,
	chatReq map[string]any, rc proxyRequestContext) {

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, api.MaxResponseBodySize))
	if err != nil {
		httputil.WriteError(w, http.StatusBadGateway, "failed to read upstream response")
		return
	}

	var chatResp api.ChatResponse
	if err := json.Unmarshal(respBody, &chatResp); err != nil {
		w.WriteHeader(resp.StatusCode)
		w.Write(respBody)
		return
	}

	if len(chatResp.Choices) > 0 && pipeline.HasSearchToolCall(chatResp.Choices[0].Message.ToolCalls) {
		ctx := resp.Request.Context()
		finalResp, err := p.pipeline.HandleNonStreamingSearchLoop(ctx, chatReq, rc.model, &chatResp,
			func(req map[string]any) (*api.ChatResponse, error) {
				return sendChatCompletionsRequest(ctx, p.client, req, rc.model)
			}, 5)
		if err != nil {
			slog.Error("proxy search loop failed", "model", rc.modelName, "error", err)
		} else {
			chatResp = *finalResp
		}
	}

	// Filter think tags from content.
	filterNonStreamingContent(&chatResp)

	finalBody, err := json.Marshal(chatResp)
	if err != nil {
		finalBody = respBody
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(finalBody)

	logUsageFromChatResponse(p.usage, chatResp.Usage, rc, int64(len(finalBody)))
}

// handleNonStreamingChatWithFilter handles non-streaming Chat Completions responses,
// filtering <think>...</think> tags from the content.
func (p *ProxyHandler) handleNonStreamingChatWithFilter(w http.ResponseWriter, resp *http.Response, rc proxyRequestContext) {
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, api.MaxResponseBodySize))
	if err != nil {
		httputil.WriteError(w, http.StatusBadGateway, "failed to read upstream response")
		return
	}

	var chatResp api.ChatResponse
	if err := json.Unmarshal(respBody, &chatResp); err != nil {
		// Can't parse - pass through unchanged.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		w.Write(respBody)
		return
	}

	// Filter think tags from content.
	filterNonStreamingContent(&chatResp)

	finalBody, err := json.Marshal(chatResp)
	if err != nil {
		finalBody = respBody
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(finalBody)

	logUsageFromChatResponse(p.usage, chatResp.Usage, rc, int64(len(finalBody)))
}

// handleStreamingWithSearch parses an SSE stream from a Chat Completions backend,
// detecting web_search tool calls. If found, it executes the search and re-streams.
// If no search calls are detected, the stream passes through to the client unchanged.
func (p *ProxyHandler) handleStreamingWithSearch(ctx context.Context, w http.ResponseWriter,
	resp *http.Response, chatReq map[string]any, rc proxyRequestContext) {

	flusher, canFlush := w.(http.Flusher)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	flush := func() {
		if canFlush {
			flusher.Flush()
		}
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)

	var searchState pipeline.StreamingSearchState
	var finishReason string
	var usageData *api.ChunkUsage
	var bufferedLines []string
	buffering := false
	var contentAccum strings.Builder
	var responseBytes int64

	for scanner.Scan() {
		line := scanner.Text()
		responseBytes += int64(len(line)) + 1

		if responseBytes > api.MaxResponseBodySize {
			break
		}

		if !strings.HasPrefix(line, "data: ") {
			if !buffering {
				fmt.Fprintf(w, "%s\n", line)
				flush()
			} else {
				bufferedLines = append(bufferedLines, line)
			}
			continue
		}

		data := line[6:]
		if data == "[DONE]" {
			if !buffering {
				fmt.Fprintf(w, "data: [DONE]\n\n")
				flush()
			}
			break
		}

		var chunk api.ChatChunk
		if json.Unmarshal([]byte(data), &chunk) != nil {
			if !buffering {
				fmt.Fprintf(w, "%s\n", line)
				flush()
			}
			continue
		}

		if chunk.Usage != nil {
			usageData = chunk.Usage
		}

		if len(chunk.Choices) > 0 {
			choice := chunk.Choices[0]
			delta := choice.Delta

			if delta.Content != nil && *delta.Content != "" {
				contentAccum.WriteString(*delta.Content)
			}

			for _, tc := range delta.ToolCalls {
				if tc.ID != "" {
					if !buffering {
						buffering = true
					}
					name := ""
					if tc.Function != nil {
						name = tc.Function.Name
					}
					searchState.AccumulateToolCall(tc.ID, name)
				}
				if tc.Function != nil && tc.Function.Arguments != "" {
					searchState.AppendArgs(tc.Index, tc.Function.Arguments)
				}
			}

			if choice.FinishReason != nil {
				finishReason = *choice.FinishReason
			}
		}

		if buffering {
			bufferedLines = append(bufferedLines, line)
		} else {
			fmt.Fprintf(w, "%s\n", line)
			flush()
		}
	}

	if buffering && finishReason == "tool_calls" && searchState.OnlySearchCalls() {
		slog.Debug("proxy: detected web_search in streaming response, executing search",
			"model", rc.modelName, "calls", len(searchState.ToolCalls()))

		toolCalls := searchState.ToChatChoiceToolCalls()
		searchDone := make(chan struct{})
		var newChatReq map[string]any
		var searchErr error

		go func() {
			defer close(searchDone)
			newChatReq, _, searchErr = p.pipeline.ExecuteSearchAndResend(
				ctx, chatReq, rc.model, toolCalls, contentAccum.String())
		}()

		completed := waitForSearchOrDisconnect(ctx, searchDone,
			func() {
				fmt.Fprintf(w, ": searching\n\n")
				flush()
			},
			2*time.Second, 5*time.Second, "proxy streaming")
		if !completed {
			// Client disconnected; goroutine has been awaited so no race
			// and no leak — just stop emitting to a dead connection.
			return
		}

		if searchErr != nil {
			slog.Warn("proxy streaming search failed, replaying original", "error", searchErr)
			replayBufferedLines(w, bufferedLines)
			flush()
		} else if newChatReq != nil {
			p.reStreamFromBackend(ctx, w, flusher, canFlush, newChatReq, rc.model)
		}
	} else if buffering {
		replayBufferedLines(w, bufferedLines)
		flush()
	}

	logUsageFromChatResponse(p.usage, usageData, rc, responseBytes)
}

// replayBufferedLines writes accumulated SSE lines back to the client.
func replayBufferedLines(w http.ResponseWriter, lines []string) {
	for _, l := range lines {
		fmt.Fprintf(w, "%s\n", l)
	}
	fmt.Fprintf(w, "\ndata: [DONE]\n\n")
}

// logUsageFromChatResponse logs usage from a Chat Completions response
// (streaming or non-streaming). Thin wrapper around logUsageChat that
// extracts fields from the per-request proxyRequestContext — saves 6 lines
// of boilerplate at each of the 4 call sites in this file.
func logUsageFromChatResponse(ul *usage.UsageLogger, usageData *api.ChunkUsage,
	rc proxyRequestContext, responseBytes int64) {
	logUsageChat(ul, usageLogInput{
		startTime:     rc.startTime,
		statusCode:    http.StatusOK,
		keyName:       rc.keyName,
		keyHash:       rc.keyHash,
		model:         rc.modelName,
		endpoint:      rc.endpoint,
		requestBytes:  int64(len(rc.requestBody)),
		responseBytes: responseBytes,
	}, usageData)
}

// reStreamFromBackend sends a new streaming request and forwards the SSE response raw.
func (p *ProxyHandler) reStreamFromBackend(ctx context.Context, w http.ResponseWriter,
	flusher http.Flusher, canFlush bool, chatReq map[string]any, model *config.ModelConfig) {

	chatReq["stream"] = true
	chatReq["stream_options"] = map[string]any{"include_usage": true}
	model.ApplySamplingDefaults(chatReq)
	newBody, err := json.Marshal(chatReq)
	if err != nil {
		slog.Error("proxy search re-stream: marshal failed", "error", err)
		return
	}

	upstreamURL := strings.TrimRight(model.Backend, "/") + api.ChatCompletionsPath
	upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(newBody))
	if err != nil {
		slog.Error("proxy search re-stream: request build failed", "error", err)
		return
	}
	upReq.Header.Set("Content-Type", "application/json")
	upReq.Header.Set("Accept", "text/event-stream")
	if model.APIKey != "" {
		upReq.Header.Set("Authorization", "Bearer "+model.APIKey)
	}

	resp, err := p.client.Do(upReq)
	if err != nil {
		slog.Error("proxy search re-stream: upstream failed", "error", err)
		return
	}
	defer resp.Body.Close()

	// Use the think tag filter for re-streamed content.
	reStreamWithThinkFilter(w, resp.Body, flusher, canFlush)
}
