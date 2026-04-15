package handler

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go-llm-proxy/internal/auth"
	"go-llm-proxy/internal/config"
)

func TestIsAllowedQdrantPath(t *testing.T) {
	cases := []struct {
		method, path string
		want         bool
		why          string
	}{
		// Operational routes: isolation-injected, allowed.
		{http.MethodPut, "/collections/c/points", true, "upsert points"},
		{http.MethodPost, "/collections/c/points/search", true, "vector search"},
		{http.MethodPost, "/collections/c/points/query", true, "query"},
		{http.MethodPost, "/collections/c/points/scroll", true, "scroll"},
		{http.MethodPost, "/collections/c/points/count", true, "count"},
		{http.MethodPost, "/collections/c/points/delete", true, "delete points by filter"},
		{http.MethodGet, "/collections/c", true, "collection schema"},

		// Admin / discovery routes: blocked.
		{http.MethodGet, "/collections", false, "list collections — cross-tenant enumeration"},
		{http.MethodPut, "/collections/c", false, "create collection"},
		{http.MethodPatch, "/collections/c", false, "update collection config"},
		{http.MethodDelete, "/collections/c", false, "delete collection"},
		{http.MethodGet, "/collections/c/snapshots", false, "snapshots"},
		{http.MethodGet, "/cluster", false, "cluster info"},
		{http.MethodGet, "/telemetry", false, "telemetry"},
		{http.MethodGet, "/metrics", false, "metrics"},
		{http.MethodGet, "/", false, "root"},
		{http.MethodPost, "/collections/c/points/unknown", false, "unknown points op"},
		{http.MethodDelete, "/collections/c/points/abc", false, "delete single point by id (not isolation-filtered)"},
		{http.MethodPut, "/collections/c/index", false, "index management"},
	}
	for _, tc := range cases {
		if got := isAllowedQdrantPath(tc.method, tc.path); got != tc.want {
			t.Errorf("isAllowedQdrantPath(%s %s) = %v, want %v (%s)",
				tc.method, tc.path, got, tc.want, tc.why)
		}
	}
}

func TestQdrantHandler_BlocksAdminRoutes(t *testing.T) {
	// Upstream that would succeed if the proxy forwarded — proves the
	// block happens proxy-side, not by Qdrant returning 403.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"should-not-be-reached"}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Services: config.ServicesConfig{
			Qdrant: &config.QdrantConfig{
				Backend: upstream.URL,
				AppKeys: []config.AppKeyConfig{{Name: "app1", Key: "appkey1"}},
			},
		},
	}
	cs := config.NewTestConfigStore(cfg)
	h := NewQdrantHandler(cs, nil)

	blocked := []struct{ method, path string }{
		{http.MethodGet, "/qdrant/collections"},
		{http.MethodDelete, "/qdrant/collections/mine"},
		{http.MethodGet, "/qdrant/cluster"},
		{http.MethodGet, "/qdrant/telemetry"},
		{http.MethodGet, "/qdrant/metrics"},
	}
	for _, tc := range blocked {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		ctx := auth.WithAppKeyContext(req.Context(), &config.AppKeyConfig{Name: "app1", Key: "appkey1"})
		req = req.WithContext(ctx)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		if w.Code != http.StatusForbidden {
			t.Errorf("%s %s: expected 403, got %d (body: %s)",
				tc.method, tc.path, w.Code, w.Body.String())
		}
		// Explicitly confirm the forwarding path did NOT fire.
		if strings.Contains(w.Body.String(), "should-not-be-reached") {
			t.Errorf("%s %s: request was forwarded to upstream (body: %s)",
				tc.method, tc.path, w.Body.String())
		}
	}
}

func TestQdrantHandler_AllowsOperationalRoutes(t *testing.T) {
	var gotPath string
	var gotMethod string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"result":{"operation_id":1,"status":"acknowledged"}}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{
		Services: config.ServicesConfig{
			Qdrant: &config.QdrantConfig{
				Backend: upstream.URL,
				AppKeys: []config.AppKeyConfig{{Name: "app1", Key: "k"}},
			},
		},
	}
	h := NewQdrantHandler(config.NewTestConfigStore(cfg), nil)

	req := httptest.NewRequest(http.MethodPut, "/qdrant/collections/mine/points", strings.NewReader(`{"points":[{"id":1,"vector":[0.1]}]}`))
	req.Header.Set("Content-Type", "application/json")
	ctx := auth.WithAppKeyContext(req.Context(), &config.AppKeyConfig{Name: "app1", Key: "k"})
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotPath != "/collections/mine/points" {
		t.Errorf("upstream path: %q", gotPath)
	}
	if gotMethod != http.MethodPut {
		t.Errorf("upstream method: %q", gotMethod)
	}
}
