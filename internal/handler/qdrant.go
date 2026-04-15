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

// Path patterns for app isolation. Any path accepted by isAllowedQdrantPath
// must match exactly one of these (or the bare-collection-name infoPattern
// used for the GET schema case), otherwise isolation isn't applied and the
// caller could reach a collection's contents as a different tenant.
var (
	putPointsPattern    = regexp.MustCompile(`^/collections/[^/]+/points$`)
	searchPattern       = regexp.MustCompile(`^/collections/[^/]+/points/(search|scroll|query|count)$`)
	deletePointsPattern = regexp.MustCompile(`^/collections/[^/]+/points/delete$`)
	getCollectionInfo   = regexp.MustCompile(`^/collections/[^/]+$`)
)

// isAllowedQdrantPath decides whether a (method, path) combination is
// permitted for an app-keyed client.
//
// The previous implementation proxied **every** path through to Qdrant
// after applying isolation only to a narrow allowlist of points-level
// routes. That meant app keys could create/delete collections, read
// snapshots, hit /cluster, etc. — capabilities that bypass the
// per-tenant filter entirely. Tenant A could list or destroy tenant B's
// collections.
//
// The fix is to flip the model to strict allowlist: only the operational,
// isolation-covered routes are accepted; everything else returns 403.
// Operators who need admin routes (schema migrations, snapshots, cluster
// health) should go straight to Qdrant with the backend API key, not
// through this proxy.
func isAllowedQdrantPath(method, path string) bool {
	switch method {
	case http.MethodGet:
		// Collection schema is safe to expose — it doesn't leak points.
		return getCollectionInfo.MatchString(path)
	case http.MethodPut:
		// Upsert points; isolation injects the caller's app name.
		return putPointsPattern.MatchString(path)
	case http.MethodPost:
		// Search / query / scroll / count / delete — all isolation-filtered.
		return searchPattern.MatchString(path) || deletePointsPattern.MatchString(path)
	}
	return false
}

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

	// Strict allowlist: block admin/discovery routes (list collections,
	// snapshots, cluster, telemetry, /collections/{name} create+delete,
	// etc.). Only operational, isolation-filtered paths are accepted.
	if !isAllowedQdrantPath(r.Method, qdrantPath) {
		slog.Warn("qdrant: blocked non-allowlisted path",
			"app", appKey.Name, "method", r.Method, "path", qdrantPath)
		httputil.WriteError(w, http.StatusForbidden,
			"this path is not available via the proxy; only points-level operations are exposed to app keys")
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

	logUsage(h.usage, usageLogInput{
		startTime: startTime, statusCode: resp.StatusCode,
		keyName: appKey.Name, keyHash: usage.HashKey(appKey.Key),
		model: "qdrant", endpoint: "/qdrant" + qdrantPath,
		requestBytes: int64(len(body)), responseBytes: int64(len(respBody)),
	})
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
