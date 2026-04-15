package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go-llm-proxy/internal/api"
	"go-llm-proxy/internal/awsauth"
	"go-llm-proxy/internal/config"
	"go-llm-proxy/internal/httputil"
	"go-llm-proxy/internal/usage"
)

// handleBedrock dispatches an Anthropic Messages request to AWS Bedrock's
// Converse / ConverseStream API. The request is translated to Converse,
// signed with SigV4 (or sent with a Bedrock API key bearer token), and the
// response is translated back to the Anthropic Messages shape — non-streaming
// as a JSON document, streaming as Anthropic SSE events.
//
// Pipeline pre-processors (vision, PDF) are not invoked here: Bedrock-hosted
// Claude models support vision and document blocks natively via Converse.
// If a future use case requires the pipeline (e.g. routing PDFs through a
// Tesseract step), that integration would need to translate the post-pipeline
// Chat-Completions-shaped state back into Converse blocks.
func (h *MessagesHandler) handleBedrock(
	ctx context.Context, w http.ResponseWriter,
	body []byte, req messagesRequest, model *config.ModelConfig,
	keyName, keyHash string, startTime time.Time,
) {
	slog.Info("proxying messages request (bedrock)", "model", req.Model, "key", keyName, "stream", req.Stream)

	converseReq, err := buildConverseRequestFromAnthropic(req)
	if err != nil {
		slog.Error("bedrock translation failed", "model", req.Model, "error", err)
		httputil.WriteAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "request translation failed")
		return
	}

	applyConverseSamplingDefaults(converseReq, model)

	converseBody, err := json.Marshal(converseReq)
	if err != nil {
		httputil.WriteAnthropicError(w, http.StatusInternalServerError, "api_error", "failed to build upstream request")
		return
	}

	upstreamURL, err := buildBedrockURL(model, req.Stream)
	if err != nil {
		slog.Error("bedrock url build failed", "model", req.Model, "error", err)
		httputil.WriteAnthropicError(w, http.StatusInternalServerError, "api_error", "invalid backend configuration")
		return
	}

	upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(converseBody))
	if err != nil {
		httputil.WriteAnthropicError(w, http.StatusInternalServerError, "api_error", "failed to create upstream request")
		return
	}
	upReq.Header.Set("Content-Type", "application/json")
	if req.Stream {
		// Bedrock returns vnd.amazon.eventstream regardless of Accept, but
		// setting Accept makes intent explicit and matches AWS SDK behavior.
		upReq.Header.Set("Accept", "application/vnd.amazon.eventstream")
	} else {
		upReq.Header.Set("Accept", "application/json")
	}

	if model.APIKey != "" {
		// Bedrock API keys (introduced 2025) are bearer tokens — no SigV4
		// signing. Equivalent to the OpenAI-style auth the rest of the
		// proxy uses.
		upReq.Header.Set("Authorization", "Bearer "+model.APIKey)
	} else {
		awsauth.SignRequest(upReq, converseBody, awsauth.Credentials{
			AccessKeyID:     model.AWSAccessKey,
			SecretAccessKey: model.AWSSecretKey,
			SessionToken:    model.AWSSessionToken,
		}, model.Region, "bedrock", time.Now())
	}

	resp, err := h.client.Do(upReq)
	if err != nil {
		if ctx.Err() != nil {
			httputil.WriteAnthropicError(w, http.StatusGatewayTimeout, "api_error", "upstream request timed out")
			return
		}
		slog.Error("bedrock upstream request failed", "error", err, "model", req.Model)
		httputil.WriteAnthropicError(w, http.StatusBadGateway, "api_error", "upstream request failed")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, api.MaxResponseBodySize))
		slog.Error("bedrock returned error",
			"model", req.Model, "status", resp.StatusCode, "body", string(errBody))
		// Sanitized error: never forward raw AWS error bodies to clients —
		// they may include account IDs, ARNs, request IDs.
		errMsg := fmt.Sprintf("bedrock returned HTTP %d", resp.StatusCode)
		if req.Stream {
			emitBedrockSSEError(w, errMsg)
		} else {
			httputil.WriteAnthropicError(w, resp.StatusCode, "api_error", errMsg)
		}
		if h.usage != nil {
			rec := usage.UsageRecord{
				Timestamp: startTime, KeyHash: keyHash, KeyName: keyName,
				Model: req.Model, Endpoint: "/v1/messages", StatusCode: resp.StatusCode,
				RequestBytes: int64(len(body)), ResponseBytes: int64(len(errBody)),
				DurationMS: time.Since(startTime).Milliseconds(),
			}
			go h.usage.Log(rec)
		}
		return
	}

	if req.Stream {
		httputil.SetSecurityHeaders(w)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		respBytes, usageData := streamBedrockToAnthropicSSE(w, resp.Body, req.Model)
		if h.usage != nil {
			rec := usage.UsageRecord{
				Timestamp:    startTime,
				KeyHash:      keyHash,
				KeyName:      keyName,
				Model:        req.Model,
				Endpoint:     "/v1/messages",
				StatusCode:   resp.StatusCode,
				RequestBytes: int64(len(body)),
				ResponseBytes: respBytes,
				DurationMS:   time.Since(startTime).Milliseconds(),
			}
			if usageData != nil {
				rec.InputTokens = usageData.Input
				rec.OutputTokens = usageData.Output
				rec.TotalTokens = usageData.Input + usageData.Output
			}
			go h.usage.Log(rec)
		}
		return
	}

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, api.MaxResponseBodySize))
	if err != nil {
		httputil.WriteAnthropicError(w, http.StatusBadGateway, "api_error", "failed to read upstream response")
		return
	}

	out, usageData, err := buildAnthropicResponseFromConverse(respBody, req.Model)
	if err != nil {
		slog.Error("bedrock response decode failed", "model", req.Model, "error", err)
		httputil.WriteAnthropicError(w, http.StatusBadGateway, "api_error", "invalid upstream response")
		return
	}

	httputil.SetSecurityHeaders(w)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)

	if h.usage != nil {
		rec := usage.UsageRecord{
			Timestamp:    startTime,
			KeyHash:      keyHash,
			KeyName:      keyName,
			Model:        req.Model,
			Endpoint:     "/v1/messages",
			StatusCode:   resp.StatusCode,
			RequestBytes: int64(len(body)),
			ResponseBytes: int64(len(respBody)),
			DurationMS:   time.Since(startTime).Milliseconds(),
		}
		if usageData != nil {
			rec.InputTokens = usageData.Input
			rec.OutputTokens = usageData.Output
			rec.TotalTokens = usageData.Input + usageData.Output
		}
		go h.usage.Log(rec)
	}
}

// buildBedrockURL constructs the Converse / ConverseStream URL. The model ID
// is URL-path-encoded so inference profile IDs like
// "us.anthropic.claude-sonnet-4-20250514-v1:0" (with a colon) are valid in
// the path. Bedrock wants the colon percent-encoded as %3A.
func buildBedrockURL(model *config.ModelConfig, stream bool) (string, error) {
	base := strings.TrimRight(model.Backend, "/")
	if base == "" {
		return "", fmt.Errorf("model %q: empty backend URL", model.Name)
	}
	if model.Model == "" {
		return "", fmt.Errorf("model %q: missing model id", model.Name)
	}
	op := "converse"
	if stream {
		op = "converse-stream"
	}
	return fmt.Sprintf("%s/model/%s/%s", base, url.PathEscape(model.Model), op), nil
}

// emitBedrockSSEError writes a single Anthropic-shaped SSE error event when
// the upstream returned a non-streaming error but the client requested a
// stream. Headers are set inline because the response body has not yet been
// touched at this point.
func emitBedrockSSEError(w http.ResponseWriter, msg string) {
	httputil.SetSecurityHeaders(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	errData, _ := json.Marshal(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    "api_error",
			"message": msg,
		},
	})
	fmt.Fprintf(w, "event: error\ndata: %s\n\n", errData)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
