package handler

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"go-llm-proxy/internal/auth"
	"go-llm-proxy/internal/config"
	"go-llm-proxy/internal/httputil"
)

// ModelsHandler serves GET /v1/models — returns the aggregated model list.
type ModelsHandler struct {
	config *config.ConfigStore
}

func NewModelsHandler(cs *config.ConfigStore) *ModelsHandler {
	return &ModelsHandler{config: cs}
}

func (h *ModelsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	cfg := h.config.Get()
	key := auth.KeyFromContext(r.Context())

	type modelObj struct {
		ID          string `json:"id"`
		Object      string `json:"object"`
		Created     int64  `json:"created"`
		OwnedBy     string `json:"owned_by"`
		MaxModelLen int    `json:"max_model_len,omitempty"`
	}

	models := make([]modelObj, 0, len(cfg.Models))
	for _, m := range cfg.Models {
		if !auth.KeyAllowsModel(key, m.Name) {
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

	httputil.SetSecurityHeaders(w)
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{
		"object": "list",
		"data":   models,
	}); err != nil {
		slog.Error("failed to write models response", "error", err)
	}
}
