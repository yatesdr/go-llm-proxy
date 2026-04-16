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

// handleBedrockChat dispatches an OAI Chat Completions request to AWS
// Bedrock Converse / ConverseStream. It is the OAI counterpart to
// MessagesHandler.handleBedrock — same upstream protocol, different
// client-facing shape.
//
// Known limitation (shared with handleBedrock): mid-stream credential
// refresh is not supported. If SigV4 temporary credentials expire while
// a stream is in flight, the connection will surface as a client-visible
// disconnect/reconnect.
func (p *ProxyHandler) handleBedrockChat(
	ctx context.Context, w http.ResponseWriter,
	body []byte, modelName string, model *config.ModelConfig,
	keyName, keyHash, requestID string, startTime time.Time,
) {
	converseReq, parsedReq, err := buildConverseRequestFromChat(body)
	if err != nil {
		slog.Error("bedrock chat translation failed",
			"model", modelName, "error", err, "request_id", requestID)
		httputil.WriteError(w, http.StatusBadRequest, "invalid chat completions request")
		return
	}

	applyConverseSamplingDefaults(converseReq, model)
	applyGuardrails(converseReq, model)

	includeUsage := false
	if len(parsedReq.StreamOptions) > 0 {
		var so struct {
			IncludeUsage bool `json:"include_usage"`
		}
		json.Unmarshal(parsedReq.StreamOptions, &so)
		includeUsage = so.IncludeUsage
	}

	converseBody, err := json.Marshal(converseReq)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to build upstream request")
		return
	}

	upReq, err := prepareBedrockRequest(ctx, model, converseBody, parsedReq.Stream, time.Now())
	if err != nil {
		slog.Error("bedrock request build failed",
			"model", modelName, "error", err, "request_id", requestID)
		httputil.WriteError(w, http.StatusInternalServerError, "invalid backend configuration")
		return
	}
	if requestID != "" {
		upReq.Header.Set("X-Request-ID", requestID)
	}

	slog.Info("proxying chat completions request (bedrock)",
		"model", modelName, "key", keyName, "stream", parsedReq.Stream, "request_id", requestID)

	resp, err := p.client.Do(upReq)
	if err != nil {
		if ctx.Err() != nil {
			httputil.WriteError(w, http.StatusGatewayTimeout, "upstream request timed out")
			return
		}
		slog.Error("bedrock upstream request failed",
			"error", err, "model", modelName, "request_id", requestID)
		httputil.WriteError(w, http.StatusBadGateway, "upstream request failed")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, api.MaxResponseBodySize))
		clientStatus, errType, publicMsg, upstreamType, scrubbed := classifyBedrockError(resp.StatusCode, errBody)

		slog.Error("bedrock returned error",
			"model", modelName, "upstream_status", resp.StatusCode,
			"client_status", clientStatus, "category", errType, "request_id", requestID)
		slog.Debug("bedrock error detail",
			"model", modelName, "upstream_type", upstreamType,
			"scrubbed_body", scrubbed, "request_id", requestID)

		renderBedrockError(w, shapeOAI, parsedReq.Stream, clientStatus, errType, publicMsg)
		logUsage(p.usage, usageLogInput{
			startTime: startTime, statusCode: resp.StatusCode,
			keyName: keyName, keyHash: keyHash,
			model: modelName, endpoint: "/v1/chat/completions",
			requestBytes: int64(len(body)), responseBytes: int64(len(errBody)),
		})
		return
	}

	if parsedReq.Stream {
		httputil.SetSecurityHeaders(w)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		respBytes, usageData := streamBedrockToChatSSE(w, resp.Body, modelName, includeUsage, requestID)
		logUsageConverse(p.usage, usageLogInput{
			startTime: startTime, statusCode: resp.StatusCode,
			keyName: keyName, keyHash: keyHash,
			model: modelName, endpoint: "/v1/chat/completions",
			requestBytes: int64(len(body)), responseBytes: respBytes,
		}, usageData)
		return
	}

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, api.MaxResponseBodySize))
	if err != nil {
		httputil.WriteError(w, http.StatusBadGateway, "failed to read upstream response")
		return
	}

	chatResp, usageData, err := buildChatResponseFromConverse(respBody, modelName)
	if err != nil {
		slog.Error("bedrock response decode failed",
			"model", modelName, "error", err, "request_id", requestID)
		httputil.WriteError(w, http.StatusBadGateway, "invalid upstream response")
		return
	}

	httputil.SetSecurityHeaders(w)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(chatResp)

	logUsageConverse(p.usage, usageLogInput{
		startTime: startTime, statusCode: resp.StatusCode,
		keyName: keyName, keyHash: keyHash,
		model: modelName, endpoint: "/v1/chat/completions",
		requestBytes: int64(len(body)), responseBytes: int64(len(respBody)),
	}, usageData)
}

// emitChatSSEError writes an OAI-style SSE error chunk followed by [DONE].
// Used when an upstream non-streaming error occurs but the client expects a
// stream — rendering the error inline keeps the contract intact.
func emitChatSSEError(w http.ResponseWriter, msg string) {
	httputil.SetSecurityHeaders(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	errObj := map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "api_error",
		},
	}
	data, _ := json.Marshal(errObj)
	fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", data)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
