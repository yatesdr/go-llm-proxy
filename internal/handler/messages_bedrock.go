package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"go-llm-proxy/internal/api"
	"go-llm-proxy/internal/config"
	"go-llm-proxy/internal/httputil"
)

// handleBedrock dispatches an Anthropic Messages request to AWS Bedrock's
// Converse / ConverseStream API. The request is translated to Converse,
// signed with SigV4 (or sent with a Bedrock API key bearer token), and the
// response is translated back to the Anthropic Messages shape — non-streaming
// as a JSON document, streaming as Anthropic SSE events.
//
// Known limitation: if the caller's SigV4 temporary credentials expire
// mid-stream, the in-flight request is not re-signed. AWS STS creds live
// ≥15 min; connections that outlive them surface as a client-visible
// disconnect/reconnect. A mid-flight refresh would require re-forming the
// entire signed request and is not worth the complexity for an uncommon edge.
//
// Pipeline pre-processors (vision, PDF) are not invoked here: Bedrock-hosted
// Claude models support vision and document blocks natively via Converse.
// If a future use case requires the pipeline (e.g. routing PDFs through a
// Tesseract step), that integration would need to translate the post-pipeline
// Chat-Completions-shaped state back into Converse blocks.
func (h *MessagesHandler) handleBedrock(
	ctx context.Context, w http.ResponseWriter,
	body []byte, req messagesRequest, model *config.ModelConfig,
	keyName, keyHash, requestID string, startTime time.Time,
) {
	slog.Info("proxying messages request (bedrock)",
		"model", req.Model, "key", keyName, "stream", req.Stream, "request_id", requestID)

	converseReq, err := buildConverseRequestFromAnthropic(req)
	if err != nil {
		slog.Error("bedrock translation failed",
			"model", req.Model, "error", err, "request_id", requestID)
		httputil.WriteAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "request translation failed")
		return
	}

	applyConverseSamplingDefaults(converseReq, model)
	applyGuardrails(converseReq, model)

	converseBody, err := json.Marshal(converseReq)
	if err != nil {
		httputil.WriteAnthropicError(w, http.StatusInternalServerError, "api_error", "failed to build upstream request")
		return
	}

	upReq, err := prepareBedrockRequest(ctx, model, converseBody, req.Stream, time.Now())
	if err != nil {
		slog.Error("bedrock request build failed",
			"model", req.Model, "error", err, "request_id", requestID)
		httputil.WriteAnthropicError(w, http.StatusInternalServerError, "api_error", "invalid backend configuration")
		return
	}
	if requestID != "" {
		upReq.Header.Set("X-Request-ID", requestID)
	}

	resp, err := h.client.Do(upReq)
	if err != nil {
		if ctx.Err() != nil {
			httputil.WriteAnthropicError(w, http.StatusGatewayTimeout, "api_error", "upstream request timed out")
			return
		}
		slog.Error("bedrock upstream request failed",
			"error", err, "model", req.Model, "request_id", requestID)
		httputil.WriteAnthropicError(w, http.StatusBadGateway, "api_error", "upstream request failed")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, api.MaxResponseBodySize))
		clientStatus, errType, publicMsg, upstreamType, scrubbed := classifyBedrockError(resp.StatusCode, errBody)

		// INFO: short, always-safe summary — category only, no upstream detail.
		slog.Error("bedrock returned error",
			"model", req.Model, "upstream_status", resp.StatusCode,
			"client_status", clientStatus, "category", errType, "request_id", requestID)
		// DEBUG (only emitted with -log-debug): full scrubbed context so
		// operators can trace the upstream failure without the info ever
		// reaching the client.
		slog.Debug("bedrock error detail",
			"model", req.Model, "upstream_type", upstreamType,
			"scrubbed_body", scrubbed, "request_id", requestID)

		renderBedrockError(w, shapeAnthropic, req.Stream, clientStatus, errType, publicMsg)
		logUsage(h.usage, usageLogInput{
			startTime: startTime, statusCode: resp.StatusCode,
			keyName: keyName, keyHash: keyHash,
			model: req.Model, endpoint: "/v1/messages",
			requestBytes: int64(len(body)), responseBytes: int64(len(errBody)),
		})
		return
	}

	if req.Stream {
		httputil.SetSecurityHeaders(w)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		respBytes, usageData := streamBedrockToAnthropicSSE(w, resp.Body, req.Model, requestID)
		logUsageConverse(h.usage, usageLogInput{
			startTime: startTime, statusCode: resp.StatusCode,
			keyName: keyName, keyHash: keyHash,
			model: req.Model, endpoint: "/v1/messages",
			requestBytes: int64(len(body)), responseBytes: respBytes,
		}, usageData)
		return
	}

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, api.MaxResponseBodySize))
	if err != nil {
		httputil.WriteAnthropicError(w, http.StatusBadGateway, "api_error", "failed to read upstream response")
		return
	}

	out, usageData, err := buildAnthropicResponseFromConverse(respBody, req.Model)
	if err != nil {
		slog.Error("bedrock response decode failed",
			"model", req.Model, "error", err, "request_id", requestID)
		httputil.WriteAnthropicError(w, http.StatusBadGateway, "api_error", "invalid upstream response")
		return
	}

	httputil.SetSecurityHeaders(w)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)

	logUsageConverse(h.usage, usageLogInput{
		startTime: startTime, statusCode: resp.StatusCode,
		keyName: keyName, keyHash: keyHash,
		model: req.Model, endpoint: "/v1/messages",
		requestBytes: int64(len(body)), responseBytes: int64(len(respBody)),
	}, usageData)
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
