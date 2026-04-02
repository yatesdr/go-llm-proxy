package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// ModelsHandler serves GET /v1/models — returns the aggregated model list.
type ModelsHandler struct {
	config *ConfigStore
}

func NewModelsHandler(cs *ConfigStore) *ModelsHandler {
	return &ModelsHandler{config: cs}
}

func (h *ModelsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	cfg := h.config.Get()
	key := keyFromContext(r.Context())

	type modelObj struct {
		ID          string `json:"id"`
		Object      string `json:"object"`
		Created     int64  `json:"created"`
		OwnedBy     string `json:"owned_by"`
		MaxModelLen int    `json:"max_model_len,omitempty"`
	}

	models := make([]modelObj, 0, len(cfg.Models))
	for _, m := range cfg.Models {
		if !keyAllowsModel(key, m.Name) {
			continue
		}
		models = append(models, modelObj{
			ID:          m.Name,
			Object:      "model",
			Created:     0,
			OwnedBy:     "organization",
			MaxModelLen: m.ContextWindow,
		})
	}

	setSecurityHeaders(w)
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{
		"object": "list",
		"data":   models,
	}); err != nil {
		slog.Error("failed to write models response", "error", err)
	}
}
