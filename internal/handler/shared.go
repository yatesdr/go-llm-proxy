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
	"mime"
	"mime/multipart"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"go-llm-proxy/internal/api"
	"go-llm-proxy/internal/config"
	"go-llm-proxy/internal/httputil"
	"go-llm-proxy/internal/pipeline"
	"go-llm-proxy/internal/usage"
)

// AllowedPaths restricts which sub-paths can be proxied to backends.
var AllowedPaths = regexp.MustCompile(`^/v1/(chat/completions|completions|embeddings|images/generations|audio/(transcriptions|translations|speech))$`)

// AllowedResponseHeaders controls which upstream headers are forwarded to clients.
var AllowedResponseHeaders = map[string]bool{
	"Content-Type":          true,
	"Content-Length":        true,
	"X-Request-ID":         true, // OpenAI
	"Openai-Processing-Ms": true,
	"Openai-Model":         true,
	"Request-Id":           true, // Anthropic (different header from X-Request-ID)
}

// ExtractModelFromJSON pulls the model name from a JSON request body.
func ExtractModelFromJSON(body []byte) string {
	var req struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &req) == nil {
		return req.Model
	}
	return ""
}

// ExtractModelFromMultipart pulls the model name from a multipart/form-data body.
func ExtractModelFromMultipart(body []byte, contentType string) string {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return ""
	}
	boundary := params["boundary"]
	if boundary == "" {
		return ""
	}

	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		part, err := reader.NextPart()
		if err != nil {
			break
		}
		if part.FormName() == "model" {
			val, err := io.ReadAll(part)
			part.Close()
			if err == nil {
				return strings.TrimSpace(string(val))
			}
			break
		}
		part.Close()
	}
	return ""
}

// RewriteModelName replaces the "model" field in a JSON body. Other field values
// are preserved as raw bytes via json.RawMessage, but top-level key order may change.
func RewriteModelName(body []byte, newName string) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	if _, ok := m["model"]; !ok {
		return body
	}
	nameBytes, err := json.Marshal(newName)
	if err != nil {
		return body
	}
	m["model"] = json.RawMessage(nameBytes)
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// RewriteModelInMultipart rebuilds a multipart body with the model field replaced.
func RewriteModelInMultipart(body []byte, contentType string, newModel string) []byte {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return body
	}
	boundary := params["boundary"]
	if boundary == "" {
		return body
	}

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	writer.SetBoundary(boundary) // preserve original boundary so Content-Type header stays valid

	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		part, err := reader.NextPart()
		if err != nil {
			break
		}

		if part.FormName() == "model" {
			// Replace model value.
			fw, err := writer.CreateFormField("model")
			if err != nil {
				part.Close()
				return body
			}
			if _, err := fw.Write([]byte(newModel)); err != nil {
				part.Close()
				return body
			}
			part.Close()
			continue
		}

		// Copy other parts as-is.
		header := part.Header
		pw, err := writer.CreatePart(header)
		if err != nil {
			part.Close()
			return body
		}
		if _, err := io.Copy(pw, part); err != nil {
			part.Close()
			return body
		}
		part.Close()
	}
	writer.Close()
	return buf.Bytes()
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
// uses a mutex to prevent concurrent writes between the keepalive goroutine and
// the main goroutine.
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

	// Mutex protects writes to w between the keepalive goroutine and the
	// main goroutine after processing completes.
	var mu sync.Mutex
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				mu.Lock()
				fmt.Fprintf(w, ": keepalive\n\n")
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				mu.Unlock()
			}
		}
	}()

	result, err := pl.ProcessRequest(ctx, chatReq, model)

	// Signal the goroutine to stop writing, then take the lock to ensure
	// it has finished any in-progress write before we return.
	close(done)
	mu.Lock()
	mu.Unlock()

	return result, true, err
}

// logUsageRecord logs a usage record for both Messages and Responses handlers.
func logUsageRecord(ul *usage.UsageLogger, usageData *api.ChunkUsage, statusCode int, model, endpoint string,
	requestBytes, responseBytes int64, keyName, keyHash string, startTime time.Time) {
	if ul == nil {
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
		Endpoint:      endpoint,
		StatusCode:    statusCode,
		RequestBytes:  requestBytes,
		ResponseBytes: responseBytes,
		InputTokens:   tokens.InputTokens,
		OutputTokens:  tokens.OutputTokens,
		TotalTokens:   tokens.TotalTokens,
		DurationMS:    time.Since(startTime).Milliseconds(),
	}
	go ul.Log(rec)
}
