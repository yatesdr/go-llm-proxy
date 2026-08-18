// shared.go contains shared types, constants, and utility functions
// used across request handlers (ProxyHandler, MessagesHandler, and ResponsesHandler).
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"go-llm-proxy/internal/api"
	"go-llm-proxy/internal/config"
	"go-llm-proxy/internal/httputil"
	"go-llm-proxy/internal/pipeline"
)

// AllowedPaths restricts which sub-paths can be proxied to backends.
var AllowedPaths = regexp.MustCompile(`^/v1/(chat/completions|completions|embeddings|rerank|images/generations|audio/(transcriptions|translations|speech))$`)

// AllowedResponseHeaders controls which upstream headers are forwarded to clients.
var AllowedResponseHeaders = map[string]bool{
	"Content-Type":         true,
	"X-Request-ID":         true, // OpenAI
	"Openai-Processing-Ms": true,
	"Openai-Model":         true,
	"Request-Id":           true, // Anthropic (different header from X-Request-ID)
}

var forwardHeaders = []string{
	"Accept",
	"Content-Type",
	"X-Request-ID",
}

var anthropicHeaders = []string{
	"Anthropic-Version",
	"Anthropic-Beta",
}

// copyResponseHeaders copies allowed upstream response headers to the client response.
func copyResponseHeaders(w http.ResponseWriter, resp *http.Response) {
	for k := range AllowedResponseHeaders {
		if v := resp.Header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
}

func copyHeaders(dst, src http.Header, backendType string) {
	for _, h := range forwardHeaders {
		if v := src.Get(h); v != "" {
			dst.Set(h, v)
		}
	}
	if backendType == config.BackendAnthropic {
		for _, h := range anthropicHeaders {
			if v := src.Get(h); v != "" {
				dst.Set(h, v)
			}
		}
	}
}

// sendChatCompletionsRequest sends a non-streaming Chat Completions request to a
// model's backend and returns the parsed response. Used by the search tool loop
// in both Messages and Responses handlers.
//
// A shallow copy of chatReq is made so that setting stream=false does not
// mutate the caller's map.
func sendChatCompletionsRequest(ctx context.Context, client *http.Client, chatReq map[string]any, model *config.ModelConfig) (*api.ChatResponse, error) {
	reqCopy := make(map[string]any, len(chatReq))
	for k, v := range chatReq {
		reqCopy[k] = v
	}
	reqCopy["stream"] = false

	// Apply model's default sampling parameters (only for fields not already set).
	model.ApplySamplingDefaults(reqCopy)

	chatBody, err := json.Marshal(reqCopy)
	if err != nil {
		return nil, fmt.Errorf("marshal chat request: %w", err)
	}
	if model.Type == config.BackendAnthropic {
		return sendAnthropicChatCompletionsRequest(ctx, client, chatBody, model)
	}

	upstreamURL := strings.TrimRight(model.Backend, "/") + api.ChatCompletionsPath
	upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(chatBody))
	if err != nil {
		return nil, fmt.Errorf("create upstream request: %w", err)
	}
	upReq.Header.Set("Content-Type", "application/json")
	if model.APIKey != "" {
		upReq.Header.Set("Authorization", "Bearer "+model.APIKey)
	}

	resp, err := client.Do(upReq)
	if err != nil {
		return nil, fmt.Errorf("upstream request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, api.MaxResponseBodySize))
	if err != nil {
		return nil, fmt.Errorf("read upstream response: %w", err)
	}

	if resp.StatusCode >= 400 {
		slog.Error("search re-send: backend error", "status", resp.StatusCode, "body", string(body))
		return nil, fmt.Errorf("backend returned HTTP %d", resp.StatusCode)
	}

	var chatResp api.ChatResponse
	if err := json.Unmarshal(body, &chatResp); err != nil {
		return nil, fmt.Errorf("parse upstream response: %w", err)
	}
	return &chatResp, nil
}

// runPipelineWithKeepalives runs pipeline processing while sending SSE keepalives
// to prevent client timeouts. Starts streaming headers, runs the pipeline, and
// waits for the keepalive goroutine to exit before returning control to code
// that may write the actual response.
//
// Returns the processed chatReq, whether headers were sent, and any error.
func runPipelineWithKeepalives(ctx context.Context, w http.ResponseWriter, pl *pipeline.Pipeline,
	chatReq map[string]any, model *config.ModelConfig) (map[string]any, bool, error) {

	httputil.SetSecurityHeaders(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	stopKeepalives := startPipelineKeepalives(w, 5*time.Second)
	defer stopKeepalives()

	result, err := pl.ProcessRequest(ctx, chatReq, model)

	return result, true, err
}

func startPipelineKeepalives(w http.ResponseWriter, interval time.Duration) (stop func()) {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				_, _ = fmt.Fprintf(w, ": keepalive\n\n")
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			}
		}
	}()

	return func() {
		close(done)
		wg.Wait()
	}
}
