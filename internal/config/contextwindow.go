package config

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"go-llm-proxy/internal/httputil"
)

// DetectContextWindows queries each backend's models endpoint to discover
// context window sizes. Results are stored on the ConfigStore's model entries.
// Runs asynchronously — failures are logged but never block startup.
func DetectContextWindows(cs *ConfigStore) {
	cfg := cs.Get()
	client := httputil.NewHTTPClient()
	client.Timeout = 10 * time.Second

	for i := range cfg.Models {
		m := &cfg.Models[i]
		go detectOne(client, cs, m.Name, *m, m.ContextWindow)
	}
}

// detectOne runs backend detection for a single model. Detection wins when it
// returns a positive value (the live backend is more authoritative than a
// hand-edited config). The configured value is the fallback: if detection
// errors or returns 0, whatever was in config.yaml is left in place.
func detectOne(client *http.Client, cs *ConfigStore, name string, model ModelConfig, configured int) {
	var ctxWindow int
	var err error

	switch model.Type {
	case BackendAnthropic:
		ctxWindow, err = detectAnthropic(client, model)
	case BackendBedrock:
		// Bedrock has no API endpoint that returns model context window;
		// GetFoundationModel reports modality/region but not max tokens,
		// and Mantle's OpenAI-compatible /v1/models omits it too. Fall
		// back to a lookup table of well-known model-ID prefixes. On a
		// miss we leave detection empty and let the configured value
		// stand.
		ctxWindow = lookupBedrockContextWindow(model.Model)
	default:
		ctxWindow, err = detectOpenAI(client, model)
	}

	if err != nil {
		if configured > 0 {
			slog.Info("context window detection failed; keeping configured value",
				"model", name, "configured", configured, "error", err)
		} else {
			slog.Warn("failed to detect context window",
				"model", name, "backend", model.Backend, "error", err)
		}
		return
	}
	if ctxWindow <= 0 {
		if configured > 0 {
			slog.Info("context window not reported by backend; keeping configured value",
				"model", name, "configured", configured)
		} else {
			slog.Debug("backend did not report context window; set context_window in config",
				"model", name, "backend", model.Backend)
		}
		return
	}

	// Update the config under the write lock so a concurrent reload
	// doesn't discard our result.
	cs.mu.Lock()
	for i := range cs.config.Models {
		if cs.config.Models[i].Name == name {
			cs.config.Models[i].ContextWindow = ctxWindow
			break
		}
	}
	cs.mu.Unlock()

	switch {
	case configured > 0 && configured != ctxWindow:
		slog.Info("detected context window overrides configured value",
			"model", name, "configured", configured, "detected", ctxWindow)
	case configured > 0:
		slog.Info("detected context window matches configured value",
			"model", name, "context_window", ctxWindow)
	default:
		slog.Info("detected context window",
			"model", name, "context_window", ctxWindow)
	}
}

// detectOpenAI queries GET /models on an OpenAI-compatible backend and
// extracts max_model_len from the matching model entry.
func detectOpenAI(client *http.Client, model ModelConfig) (int, error) {
	base := strings.TrimRight(model.Backend, "/")

	// Try llama.cpp /props endpoint first — it reports actual runtime n_ctx
	// (respects --ctx-size), unlike /models which reports n_ctx_train.
	if ctxWindow := detectLlamaCppProps(client, model, base); ctxWindow > 0 {
		return ctxWindow, nil
	}

	// Fall back to /models endpoint for other backends.
	modelsURL := base + "/models"

	req, err := http.NewRequest(http.MethodGet, modelsURL, nil)
	if err != nil {
		return 0, err
	}
	ApplyUpstreamAuthHeaders(req, model)

	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("models endpoint returned %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MB limit
	if err != nil {
		return 0, err
	}

	var result struct {
		Data []struct {
			ID          string `json:"id"`
			MaxModelLen int    `json:"max_model_len"`
			// llama-server puts context length in meta.n_ctx_train.
			Meta struct {
				NCtxTrain int `json:"n_ctx_train"`
			} `json:"meta"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return 0, err
	}

	for _, m := range result.Data {
		if m.ID == model.Model {
			if m.MaxModelLen > 0 {
				return m.MaxModelLen, nil
			}
			if m.Meta.NCtxTrain > 0 {
				return m.Meta.NCtxTrain, nil
			}
		}
	}

	// If only one model, use it regardless of name match.
	if len(result.Data) == 1 {
		if result.Data[0].MaxModelLen > 0 {
			return result.Data[0].MaxModelLen, nil
		}
		if result.Data[0].Meta.NCtxTrain > 0 {
			return result.Data[0].Meta.NCtxTrain, nil
		}
	}

	return 0, fmt.Errorf("model %q not found or no context window reported", model.Model)
}

// detectLlamaCppProps queries the llama.cpp /props endpoint to get the actual
// runtime context size (n_ctx) which respects --ctx-size configuration.
func detectLlamaCppProps(client *http.Client, model ModelConfig, base string) int {
	// Strip /v1 suffix if present to get the server root.
	propsBase := strings.TrimSuffix(base, "/v1")
	propsURL := propsBase + "/props"

	req, err := http.NewRequest(http.MethodGet, propsURL, nil)
	if err != nil {
		return 0
	}
	ApplyUpstreamAuthHeaders(req, model)

	resp, err := client.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0
	}

	var result struct {
		DefaultGenerationSettings struct {
			NCtx int `json:"n_ctx"`
		} `json:"default_generation_settings"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return 0
	}

	return result.DefaultGenerationSettings.NCtx
}

// lookupBedrockContextWindow returns the advertised max-input context size
// for a Bedrock model ID (or an inference profile that references one).
// Bedrock exposes no API for this — AWS publishes the values in model
// docs — so we keep a small prefix-match table of widely-used models.
//
// The keys are matched by prefix, after stripping the leading region
// qualifier from inference-profile IDs (e.g. "us.anthropic.claude..." →
// "anthropic.claude..."). Returns 0 when unknown; the caller treats 0 as
// "detection unavailable, rely on config override or leave unset".
//
// When AWS adds a model and we haven't updated this table, operators can
// set context_window explicitly in config. The table exists only to avoid
// making them do that for the common cases.
func lookupBedrockContextWindow(modelID string) int {
	// Strip leading "us." / "eu." / "apac." inference-profile qualifier.
	trimmed := modelID
	for _, prefix := range []string{"us.", "eu.", "apac.", "us-gov."} {
		if strings.HasPrefix(trimmed, prefix) {
			trimmed = trimmed[len(prefix):]
			break
		}
	}
	// Longest-prefix-match: map iteration order is random in Go, so iterating
	// until the first hit can pick "cohere.command" (4000) over the longer
	// "cohere.command-r-plus" (128000) for the same model ID. Walk every
	// entry and keep the longest match.
	var bestPrefix string
	var best int
	for prefix, ctx := range bedrockContextWindows {
		if strings.HasPrefix(trimmed, prefix) && len(prefix) > len(bestPrefix) {
			bestPrefix = prefix
			best = ctx
		}
	}
	return best
}

// Keyed by Bedrock model-ID prefix (longest match wins — but we iterate
// in insertion order since the map is small and collisions are rare).
// Values are the model's advertised max input context per AWS docs.
var bedrockContextWindows = map[string]int{
	// Anthropic Claude family — 200k unless noted. Claude 3 Opus is older
	// but still 200k.
	"anthropic.claude-3-5-sonnet": 200000,
	"anthropic.claude-3-5-haiku":  200000,
	"anthropic.claude-3-7-sonnet": 200000,
	"anthropic.claude-sonnet-4":   200000,
	"anthropic.claude-opus-4":     200000,
	"anthropic.claude-3-opus":     200000,
	"anthropic.claude-3-sonnet":   200000,
	"anthropic.claude-3-haiku":    200000,
	"anthropic.claude-v2":         100000,
	"anthropic.claude-instant":    100000,

	// Amazon Nova family.
	"amazon.nova-pro":   300000,
	"amazon.nova-lite":  300000,
	"amazon.nova-micro": 128000,

	// Amazon Titan text models.
	"amazon.titan-text-premier": 32000,
	"amazon.titan-text-express": 8000,
	"amazon.titan-text-lite":    4000,

	// Meta Llama family.
	"meta.llama3-70b": 8000,
	"meta.llama3-8b":  8000,
	"meta.llama3-1":   128000,
	"meta.llama3-2":   128000,
	"meta.llama3-3":   128000,
	"meta.llama4":     10000000,

	// Mistral family.
	"mistral.mistral-7b":    32000,
	"mistral.mistral-large": 128000,
	"mistral.mistral-small": 32000,
	"mistral.mixtral-8x7b":  32000,
	"mistral.pixtral-large": 128000,

	// Cohere Command family.
	"cohere.command-r-plus": 128000,
	"cohere.command-r":      128000,
	"cohere.command":        4000,

	// Z.ai GLM family (added to Bedrock in 2026).
	"zai.glm-4.7-flash": 128000,
	"zai.glm-4.6":       128000,
	"zai.glm-4.5":       128000,

	// DeepSeek family.
	"deepseek.r1": 128000,
	"deepseek.v3": 128000,
}

// detectAnthropic queries GET /v1/models/{model_id} on an Anthropic backend
// and extracts max_input_tokens.
func detectAnthropic(client *http.Client, model ModelConfig) (int, error) {
	base := strings.TrimRight(model.Backend, "/")
	modelURL := base + "/v1/models/" + model.Model

	req, err := http.NewRequest(http.MethodGet, modelURL, nil)
	if err != nil {
		return 0, err
	}
	ApplyUpstreamAuthHeaders(req, model)
	req.Header.Set("Anthropic-Version", "2023-06-01")

	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("models endpoint returned %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, err
	}

	var result struct {
		MaxInputTokens int `json:"max_input_tokens"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return 0, err
	}

	return result.MaxInputTokens, nil
}
