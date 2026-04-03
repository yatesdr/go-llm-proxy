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

	"go-llm-proxy/internal/api"
	"go-llm-proxy/internal/config"
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
func sendChatCompletionsRequest(ctx context.Context, client *http.Client, chatReq map[string]any, model *config.ModelConfig) (*api.ChatResponse, error) {
	chatReq["stream"] = false
	chatBody, err := json.Marshal(chatReq)
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
