package handler

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"path"
	"regexp"
	"strings"
	"time"

	"go-llm-proxy/internal/api"
	"go-llm-proxy/internal/auth"
	"go-llm-proxy/internal/config"
	"go-llm-proxy/internal/httputil"
	"go-llm-proxy/internal/usage"
)

// QdrantHandler proxies requests to a Qdrant vector database with app isolation.
type QdrantHandler struct {
	config *config.ConfigStore
	client *http.Client
	usage  *usage.UsageLogger
}

// NewQdrantHandler creates a new Qdrant proxy handler.
func NewQdrantHandler(cs *config.ConfigStore, usage *usage.UsageLogger) *QdrantHandler {
	return &QdrantHandler{
		config: cs,
		client: httputil.NewHTTPClient(),
		usage:  usage,
	}
}

// Path patterns for app isolation
var (
	putPointsPattern    = regexp.MustCompile(`^/collections/[^/]+/points$`)
	searchPattern       = regexp.MustCompile(`^/collections/[^/]+/points/(search|scroll|query)$`)
	getPointPattern     = regexp.MustCompile(`^/collections/[^/]+/points/[^/]+$`)
	deletePointsPattern = regexp.MustCompile(`^/collections/[^/]+/points/delete$`)
)

func (h *QdrantHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()

	cfg := h.config.Get()
	if cfg.Services.Qdrant == nil {
		httputil.WriteError(w, http.StatusNotFound, "qdrant service not configured")
		return
	}

	appKey := auth.AppKeyFromContext(r.Context())
	if appKey == nil {
		httputil.WriteError(w, http.StatusUnauthorized, "missing app key")
		return
	}

	// Strip /qdrant prefix and normalize to prevent path traversal.
	qdrantPath := path.Clean(strings.TrimPrefix(r.URL.Path, "/qdrant"))
	if qdrantPath == "." {
		qdrantPath = "/"
	}
	if strings.Contains(qdrantPath, "..") {
		httputil.WriteError(w, http.StatusBadRequest, "invalid path")
		return
	}

	// Read request body if present.
	var body []byte
	var err error
	if r.Body != nil && r.Method != http.MethodGet && r.Method != http.MethodHead {
		r.Body = http.MaxBytesReader(w, r.Body, api.MaxRequestBodySize)
		body, err = io.ReadAll(r.Body)
		r.Body.Close()
		if err != nil {
			httputil.WriteError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
	}

	// Apply app isolation transformations.
	body = h.applyIsolation(qdrantPath, r.Method, body, appKey.Name)

	// Build upstream URL.
	upstreamURL := strings.TrimRight(cfg.Services.Qdrant.Backend, "/") + qdrantPath
	if r.URL.RawQuery != "" {
		upstreamURL += "?" + r.URL.RawQuery
	}

	// Create upstream request.
	upReq, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL, bytes.NewReader(body))
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to create upstream request")
		return
	}

	// Copy relevant headers.
	if ct := r.Header.Get("Content-Type"); ct != "" {
		upReq.Header.Set("Content-Type", ct)
	}
	upReq.Header.Set("Accept", "application/json")

	// Set Qdrant API key.
	if cfg.Services.Qdrant.APIKey != "" {
		upReq.Header.Set("api-key", cfg.Services.Qdrant.APIKey)
	}

	slog.Info("proxying qdrant request", "app", appKey.Name, "method", r.Method, "path", qdrantPath)

	// Execute request.
	resp, err := h.client.Do(upReq)
	if err != nil {
		if r.Context().Err() != nil {
			httputil.WriteError(w, http.StatusGatewayTimeout, "upstream request timed out")
			return
		}
		slog.Error("qdrant upstream request failed", "error", err, "app", appKey.Name)
		httputil.WriteError(w, http.StatusBadGateway, "upstream request failed")
		return
	}
	defer resp.Body.Close()

	// Copy response headers.
	for k, v := range resp.Header {
		if k == "Content-Type" || k == "Content-Length" || k == "X-Request-ID" {
			w.Header()[k] = v
		}
	}
	httputil.SetSecurityHeaders(w)

	// Read and forward response.
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, api.MaxResponseBodySize))
	if err != nil {
		slog.Error("failed to read qdrant response", "error", err)
		httputil.WriteError(w, http.StatusBadGateway, "failed to read upstream response")
		return
	}

	w.WriteHeader(resp.StatusCode)
	w.Write(respBody)

	// Log usage.
	if h.usage != nil {
		rec := usage.UsageRecord{
			Timestamp:     startTime,
			KeyHash:       usage.HashKey(appKey.Key),
			KeyName:       appKey.Name,
			Model:         "qdrant",
			Endpoint:      "/qdrant" + qdrantPath,
			StatusCode:    resp.StatusCode,
			RequestBytes:  int64(len(body)),
			ResponseBytes: int64(len(respBody)),
			DurationMS:    time.Since(startTime).Milliseconds(),
		}
		go h.usage.Log(rec)
	}
}

// applyIsolation modifies request bodies to enforce app-level isolation.
func (h *QdrantHandler) applyIsolation(path, method string, body []byte, appName string) []byte {
	if len(body) == 0 {
		return body
	}

	// PUT /collections/{name}/points - inject app into payloads
	if method == http.MethodPut && putPointsPattern.MatchString(path) {
		return h.injectAppIntoPoints(body, appName)
	}

	// POST /collections/{name}/points/search|scroll|query - inject app filter
	if method == http.MethodPost && searchPattern.MatchString(path) {
		return h.injectAppFilter(body, appName)
	}

	// POST /collections/{name}/points/delete - inject app filter
	if method == http.MethodPost && deletePointsPattern.MatchString(path) {
		return h.injectAppFilter(body, appName)
	}

	return body
}

// injectAppIntoPoints adds "app" field to each point's payload.
func (h *QdrantHandler) injectAppIntoPoints(body []byte, appName string) []byte {
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		slog.Warn("qdrant: failed to parse points request for isolation", "error", err)
		return body
	}

	points, ok := req["points"].([]any)
	if !ok {
		return body
	}

	modified := false
	for _, p := range points {
		point, ok := p.(map[string]any)
		if !ok {
			continue
		}

		payload, ok := point["payload"].(map[string]any)
		if !ok {
			// Create payload if not present.
			payload = make(map[string]any)
			point["payload"] = payload
		}

		payload["app"] = appName
		modified = true
	}

	if !modified {
		return body
	}

	newBody, err := json.Marshal(req)
	if err != nil {
		slog.Warn("qdrant: failed to re-marshal points request", "error", err)
		return body
	}

	slog.Debug("qdrant: injected app into points", "app", appName, "count", len(points))
	return newBody
}

// injectAppFilter adds a filter clause to restrict results to the app's data.
func (h *QdrantHandler) injectAppFilter(body []byte, appName string) []byte {
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		slog.Warn("qdrant: failed to parse search request for isolation", "error", err)
		return body
	}

	appFilter := map[string]any{
		"key": "app",
		"match": map[string]any{
			"value": appName,
		},
	}

	filter, exists := req["filter"].(map[string]any)
	if !exists {
		// No existing filter - create one with just the app filter.
		req["filter"] = map[string]any{
			"must": []any{appFilter},
		}
	} else {
		// Existing filter - add app filter to must clause.
		must, ok := filter["must"].([]any)
		if !ok {
			must = []any{}
		}
		must = append(must, appFilter)
		filter["must"] = must
	}

	newBody, err := json.Marshal(req)
	if err != nil {
		slog.Warn("qdrant: failed to re-marshal search request", "error", err)
		return body
	}

	slog.Debug("qdrant: injected app filter", "app", appName)
	return newBody
}
