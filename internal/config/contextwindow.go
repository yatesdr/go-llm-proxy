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
		if m.ContextWindow > 0 {
			// Already set in config — skip detection.
			slog.Info("context window configured",
				"model", m.Name, "context_window", m.ContextWindow)
			continue
		}
		go detectOne(client, cs, m.Name, m.Backend, m.Model, m.APIKey, m.Type)
	}
}

func detectOne(client *http.Client, cs *ConfigStore, name, backend, modelID, apiKey, backendType string) {
	var ctxWindow int
	var err error

	switch backendType {
	case BackendAnthropic:
		ctxWindow, err = detectAnthropic(client, backend, modelID, apiKey)
	case BackendBedrock:
		// Bedrock has no API endpoint that returns model context window;
		// the control plane (GetFoundationModel / GetInferenceProfile)
		// reports modality and region info but not max tokens. Fall back
		// to a lookup table of well-known model-ID prefixes. If the model
		// isn't in the table, return 0 quietly — the backend will reject
		// over-limit requests, and operators can set context_window
		// explicitly in config.
		ctxWindow = lookupBedrockContextWindow(modelID)
		if ctxWindow == 0 {
			slog.Debug("bedrock model has no known context window; set context_window in config",
				"model", name, "bedrock_model_id", modelID)
			return
		}
	default:
		ctxWindow, err = detectOpenAI(client, backend, modelID, apiKey)
	}

	if err != nil {
		slog.Warn("failed to detect context window",
			"model", name, "backend", backend, "error", err)
		return
	}
	if ctxWindow <= 0 {
		slog.Warn("backend did not report context window",
			"model", name, "backend", backend)
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

	slog.Info("detected context window",
		"model", name, "context_window", ctxWindow)
}

// detectOpenAI queries GET /models on an OpenAI-compatible backend and
// extracts max_model_len from the matching model entry.
func detectOpenAI(client *http.Client, backend, modelID, apiKey string) (int, error) {
	base := strings.TrimRight(backend, "/")

	// Try llama.cpp /props endpoint first — it reports actual runtime n_ctx
	// (respects --ctx-size), unlike /models which reports n_ctx_train.
	if ctxWindow := detectLlamaCppProps(client, base, apiKey); ctxWindow > 0 {
		return ctxWindow, nil
	}

	// Fall back to /models endpoint for other backends.
	modelsURL := base + "/models"

	req, err := http.NewRequest(http.MethodGet, modelsURL, nil)
	if err != nil {
		return 0, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

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
		if m.ID == modelID {
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

	return 0, fmt.Errorf("model %q not found or no context window reported", modelID)
}

// detectLlamaCppProps queries the llama.cpp /props endpoint to get the actual
// runtime context size (n_ctx) which respects --ctx-size configuration.
func detectLlamaCppProps(client *http.Client, base, apiKey string) int {
	// Strip /v1 suffix if present to get the server root.
	propsBase := strings.TrimSuffix(base, "/v1")
	propsURL := propsBase + "/props"

	req, err := http.NewRequest(http.MethodGet, propsURL, nil)
	if err != nil {
		return 0
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

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
	for prefix, ctx := range bedrockContextWindows {
		if strings.HasPrefix(trimmed, prefix) {
			return ctx
		}
	}
	return 0
}

// Keyed by Bedrock model-ID prefix (longest match wins — but we iterate
// in insertion order since the map is small and collisions are rare).
// Values are the model's advertised max input context per AWS docs.
var bedrockContextWindows = map[string]int{
	// Anthropic Claude family — 200k unless noted. Claude 3 Opus is older
	// but still 200k.
	"anthropic.claude-3-5-sonnet":   200000,
	"anthropic.claude-3-5-haiku":    200000,
	"anthropic.claude-3-7-sonnet":   200000,
	"anthropic.claude-sonnet-4":     200000,
	"anthropic.claude-opus-4":       200000,
	"anthropic.claude-3-opus":       200000,
	"anthropic.claude-3-sonnet":     200000,
	"anthropic.claude-3-haiku":      200000,
	"anthropic.claude-v2":           100000,
	"anthropic.claude-instant":      100000,

	// Amazon Nova family.
	"amazon.nova-pro":   300000,
	"amazon.nova-lite":  300000,
	"amazon.nova-micro": 128000,

	// Amazon Titan text models.
	"amazon.titan-text-premier":  32000,
	"amazon.titan-text-express":  8000,
	"amazon.titan-text-lite":     4000,

	// Meta Llama family.
	"meta.llama3-70b":  8000,
	"meta.llama3-8b":   8000,
	"meta.llama3-1":    128000,
	"meta.llama3-2":    128000,
	"meta.llama3-3":    128000,
	"meta.llama4":      10000000,

	// Mistral family.
	"mistral.mistral-7b":        32000,
	"mistral.mistral-large":     128000,
	"mistral.mistral-small":     32000,
	"mistral.mixtral-8x7b":      32000,
	"mistral.pixtral-large":     128000,

	// Cohere Command family.
	"cohere.command-r-plus": 128000,
	"cohere.command-r":      128000,
	"cohere.command":        4000,

	// Z.ai GLM family (added to Bedrock in 2026).
	"zai.glm-4.7-flash": 128000,
	"zai.glm-4.6":       128000,
	"zai.glm-4.5":       128000,

	// DeepSeek family.
	"deepseek.r1":  128000,
	"deepseek.v3":  128000,
}

// detectAnthropic queries GET /v1/models/{model_id} on an Anthropic backend
// and extracts max_input_tokens.
func detectAnthropic(client *http.Client, backend, modelID, apiKey string) (int, error) {
	base := strings.TrimRight(backend, "/")
	modelURL := base + "/v1/models/" + modelID

	req, err := http.NewRequest(http.MethodGet, modelURL, nil)
	if err != nil {
		return 0, err
	}
	if apiKey != "" {
		req.Header.Set("X-Api-Key", apiKey)
	}
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
