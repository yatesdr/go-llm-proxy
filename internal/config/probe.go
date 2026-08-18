package config

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"go-llm-proxy/internal/httputil"
)

// ProbeResult reports what a one-shot backend probe discovered. Used by the
// admin UI to verify a backend as it is added to a workload.
type ProbeResult struct {
	Reachable     bool   `json:"reachable"`
	Status        int    `json:"status,omitempty"`         // HTTP status of the reachability check
	Engine        string `json:"engine,omitempty"`         // "llama.cpp", "openai-compatible", or "unknown"
	ContextWindow int    `json:"context_window,omitempty"` // detected runtime context size (0 = unknown)
	Error         string `json:"error,omitempty"`
}

// ProbeBackend checks a backend URL: reachability first (same HEAD probe the
// health checker uses), then best-effort engine and context-window detection
// for OpenAI-compatible backends. Never blocks longer than ~10s.
func ProbeBackend(backendURL, apiKey, backendType string) ProbeResult {
	return ProbeBackendForModel(backendURL, apiKey, backendType, "")
}

// ProbeBackendForModel is ProbeBackend with a model ID hint, needed for
// Anthropic (queries /v1/models/{id}) and Bedrock (looks up a static table
// keyed by model ID) context-window detection.
func ProbeBackendForModel(backendURL, apiKey, backendType, modelForDetection string) ProbeResult {
	client := httputil.NewHTTPClient()
	client.Timeout = 10 * time.Second
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}

	res := ProbeResult{}

	req, err := http.NewRequest(http.MethodHead, backendURL, nil)
	if err != nil {
		res.Error = "invalid backend URL: " + err.Error()
		return res
	}
	if apiKey != "" {
		if backendType == BackendAnthropic {
			req.Header.Set("X-Api-Key", apiKey)
		} else {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	resp.Body.Close()
	res.Status = resp.StatusCode
	if resp.StatusCode >= 500 {
		res.Error = fmt.Sprintf("server error: HTTP %d", resp.StatusCode)
		return res
	}
	res.Reachable = true

	if backendType == BackendAnthropic {
		if ctx, err := detectAnthropic(client, backendURL, modelForDetection, apiKey); err == nil && ctx > 0 {
			res.Engine = "anthropic"
			res.ContextWindow = ctx
		} else {
			res.Engine = "anthropic"
		}
		return res
	}
	if backendType == BackendBedrock {
		res.Engine = "bedrock"
		if ctx := lookupBedrockContextWindow(modelForDetection); ctx > 0 {
			res.ContextWindow = ctx
		}
		return res
	}

	base := strings.TrimRight(backendURL, "/")
	if ctx := detectLlamaCppProps(client, base, apiKey); ctx > 0 {
		res.Engine = "llama.cpp"
		res.ContextWindow = ctx
		return res
	}
	if ctx, ok := probeModelsEndpoint(client, base, apiKey); ok {
		res.Engine = "openai-compatible"
		res.ContextWindow = ctx
		return res
	}
	res.Engine = "unknown"
	return res
}

// probeModelsEndpoint GETs {base}/models and reports whether it looks like an
// OpenAI-compatible backend, plus a context window when only one model is
// served (the common case for local inference servers).
func probeModelsEndpoint(client *http.Client, base, apiKey string) (ctxWindow int, ok bool) {
	req, err := http.NewRequest(http.MethodGet, base+"/models", nil)
	if err != nil {
		return 0, false
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, false
	}
	var result struct {
		Data []struct {
			MaxModelLen int `json:"max_model_len"`
			Meta        struct {
				NCtxTrain int `json:"n_ctx_train"`
			} `json:"meta"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return 0, false
	}
	if len(result.Data) == 1 {
		if result.Data[0].MaxModelLen > 0 {
			return result.Data[0].MaxModelLen, true
		}
		if result.Data[0].Meta.NCtxTrain > 0 {
			return result.Data[0].Meta.NCtxTrain, true
		}
	}
	return 0, true
}
