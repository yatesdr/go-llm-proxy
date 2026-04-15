package handler

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go-llm-proxy/internal/awsauth"
	"go-llm-proxy/internal/config"
)

// Shared Bedrock dispatch plumbing used by both MessagesHandler
// (/v1/messages → Converse) and ProxyHandler (/v1/chat/completions →
// Converse). The per-shape translation and response-rendering differ, but
// the URL construction, signing, and request shape are identical — they
// live here so a change to SigV4 usage or URL layout updates both paths
// at once.

// buildBedrockURL constructs the Converse / ConverseStream URL. The model ID
// is URL-path-escaped so inference profile IDs like
// "us.anthropic.claude-sonnet-4-20250514-v1:0" (with a colon) are valid in
// the path. Colons pass through url.PathEscape unchanged per RFC 3986, and
// Bedrock accepts them literally.
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

// prepareBedrockRequest builds the signed/authenticated http.Request for a
// Converse call. Returns an error if the URL is malformed; auth is applied
// inline (Bedrock API key bearer token, OR SigV4 signing with IAM creds
// pulled from the model config and/or AWS_* env fallbacks).
//
// The caller owns the returned request and is responsible for setting any
// additional headers (e.g. X-Request-ID from the client) and calling Do.
func prepareBedrockRequest(
	ctx context.Context, model *config.ModelConfig,
	converseBody []byte, stream bool, now time.Time,
) (*http.Request, error) {
	upstreamURL, err := buildBedrockURL(model, stream)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(converseBody))
	if err != nil {
		return nil, fmt.Errorf("build bedrock request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if stream {
		// Bedrock returns vnd.amazon.eventstream regardless of Accept, but
		// setting Accept makes intent explicit and matches AWS SDK behavior.
		req.Header.Set("Accept", "application/vnd.amazon.eventstream")
	} else {
		req.Header.Set("Accept", "application/json")
	}

	if model.APIKey != "" {
		// Bedrock API keys (introduced 2025) are bearer tokens — no SigV4.
		// Equivalent to the OpenAI-style auth the rest of the proxy uses.
		req.Header.Set("Authorization", "Bearer "+model.APIKey)
	} else {
		awsauth.SignRequest(req, converseBody, awsauth.Credentials{
			AccessKeyID:     model.AWSAccessKey,
			SecretAccessKey: model.AWSSecretKey,
			SessionToken:    model.AWSSessionToken,
		}, model.Region, "bedrock", now)
	}
	return req, nil
}
