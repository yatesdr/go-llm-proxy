package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// DetectContextWindows queries each backend's models endpoint to discover
// context window sizes. Results are stored on the ConfigStore's model entries.
// Runs asynchronously — failures are logged but never block startup.
func DetectContextWindows(cs *ConfigStore) {
	cfg := cs.Get()
	client := &http.Client{Timeout: 10 * time.Second}

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

	if backendType == BackendAnthropic {
		ctxWindow, err = detectAnthropic(client, backend, modelID, apiKey)
	} else {
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
	// Append /models to the backend base URL. This works for standard
	// /v1 backends (/v1/models) and non-standard paths like Zhipu's
	// /api/coding/paas/v4 (/api/coding/paas/v4/models).
	base := strings.TrimRight(backend, "/")
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
