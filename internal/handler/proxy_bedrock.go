package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"go-llm-proxy/internal/api"
	"go-llm-proxy/internal/awsauth"
	"go-llm-proxy/internal/config"
	"go-llm-proxy/internal/httputil"
	"go-llm-proxy/internal/usage"
)

// handleBedrockChat dispatches an OAI Chat Completions request to AWS
// Bedrock Converse / ConverseStream. It is the OAI counterpart to
// MessagesHandler.handleBedrock — same upstream protocol, different
// client-facing shape.
func (p *ProxyHandler) handleBedrockChat(
	ctx context.Context, w http.ResponseWriter,
	body []byte, modelName string, model *config.ModelConfig,
	keyName, keyHash string, startTime time.Time,
) {
	converseReq, parsedReq, err := buildConverseRequestFromChat(body)
	if err != nil {
		slog.Error("bedrock chat translation failed", "model", modelName, "error", err)
		httputil.WriteError(w, http.StatusBadRequest, "invalid chat completions request")
		return
	}

	applyConverseSamplingDefaultsForChat(converseReq, model)

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

	upstreamURL, err := buildBedrockURL(model, parsedReq.Stream)
	if err != nil {
		slog.Error("bedrock url build failed", "model", modelName, "error", err)
		httputil.WriteError(w, http.StatusInternalServerError, "invalid backend configuration")
		return
	}

	upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(converseBody))
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to create upstream request")
		return
	}
	upReq.Header.Set("Content-Type", "application/json")
	if parsedReq.Stream {
		upReq.Header.Set("Accept", "application/vnd.amazon.eventstream")
	} else {
		upReq.Header.Set("Accept", "application/json")
	}

	if model.APIKey != "" {
		upReq.Header.Set("Authorization", "Bearer "+model.APIKey)
	} else {
		awsauth.SignRequest(upReq, converseBody, awsauth.Credentials{
			AccessKeyID:     model.AWSAccessKey,
			SecretAccessKey: model.AWSSecretKey,
			SessionToken:    model.AWSSessionToken,
		}, model.Region, "bedrock", time.Now())
	}

	slog.Info("proxying chat completions request (bedrock)",
		"model", modelName, "key", keyName, "stream", parsedReq.Stream)

	resp, err := p.client.Do(upReq)
	if err != nil {
		if ctx.Err() != nil {
			httputil.WriteError(w, http.StatusGatewayTimeout, "upstream request timed out")
			return
		}
		slog.Error("bedrock upstream request failed", "error", err, "model", modelName)
		httputil.WriteError(w, http.StatusBadGateway, "upstream request failed")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, api.MaxResponseBodySize))
		slog.Error("bedrock returned error",
			"model", modelName, "status", resp.StatusCode, "body", string(errBody))
		errMsg := fmt.Sprintf("bedrock returned HTTP %d", resp.StatusCode)
		if parsedReq.Stream {
			emitChatSSEError(w, errMsg)
		} else {
			httputil.WriteError(w, resp.StatusCode, errMsg)
		}
		if p.usage != nil {
			rec := usage.UsageRecord{
				Timestamp: startTime, KeyHash: keyHash, KeyName: keyName,
				Model: modelName, Endpoint: "/v1/chat/completions", StatusCode: resp.StatusCode,
				RequestBytes: int64(len(body)), ResponseBytes: int64(len(errBody)),
				DurationMS: time.Since(startTime).Milliseconds(),
			}
			go p.usage.Log(rec)
		}
		return
	}

	if parsedReq.Stream {
		httputil.SetSecurityHeaders(w)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		respBytes, usageData := streamBedrockToChatSSE(w, resp.Body, modelName, includeUsage)
		logBedrockUsage(p.usage, modelName, "/v1/chat/completions", resp.StatusCode,
			int64(len(body)), respBytes, usageData, keyName, keyHash, startTime)
		return
	}

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, api.MaxResponseBodySize))
	if err != nil {
		httputil.WriteError(w, http.StatusBadGateway, "failed to read upstream response")
		return
	}

	chatResp, usageData, err := buildChatResponseFromConverse(respBody, modelName)
	if err != nil {
		slog.Error("bedrock response decode failed", "model", modelName, "error", err)
		httputil.WriteError(w, http.StatusBadGateway, "invalid upstream response")
		return
	}

	httputil.SetSecurityHeaders(w)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(chatResp)

	logBedrockUsage(p.usage, modelName, "/v1/chat/completions", resp.StatusCode,
		int64(len(body)), int64(len(respBody)), usageData, keyName, keyHash, startTime)
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

func logBedrockUsage(ul *usage.UsageLogger, modelName, endpoint string, statusCode int,
	reqBytes, respBytes int64, u *converseUsage, keyName, keyHash string, startTime time.Time) {
	if ul == nil {
		return
	}
	rec := usage.UsageRecord{
		Timestamp:     startTime,
		KeyHash:       keyHash,
		KeyName:       keyName,
		Model:         modelName,
		Endpoint:      endpoint,
		StatusCode:    statusCode,
		RequestBytes:  reqBytes,
		ResponseBytes: respBytes,
		DurationMS:    time.Since(startTime).Milliseconds(),
	}
	if u != nil {
		rec.InputTokens = u.Input
		rec.OutputTokens = u.Output
		rec.TotalTokens = u.Input + u.Output
	}
	go ul.Log(rec)
}
