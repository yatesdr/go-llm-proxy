package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go-llm-proxy/internal/auth"
	"go-llm-proxy/internal/config"
	"go-llm-proxy/internal/pipeline"
)

type fakeSearch struct {
	configured bool
	err        error
	queries    []string
}

func (f *fakeSearch) SearchConfigured() bool { return f.configured }
func (f *fakeSearch) SearchGlobal(_ context.Context, q string) ([]pipeline.SearchHit, error) {
	f.queries = append(f.queries, q)
	if f.err != nil {
		return nil, f.err
	}
	return []pipeline.SearchHit{{Title: "Go", URL: "https://go.dev", Snippet: "The Go language"}}, nil
}

func postSearch(h http.Handler, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/v1/search", strings.NewReader(body)))
	return w
}

func TestSearchReturnsHitsAndFoldsDomains(t *testing.T) {
	f := &fakeSearch{configured: true}
	w := postSearch(NewSearchHandler(f), `{"query":"go release","include_domains":["go.dev","github.com"],"exclude_domains":["spam.example"]}`)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var got struct{ Results []map[string]string }
	json.Unmarshal(w.Body.Bytes(), &got)
	if len(got.Results) != 1 || got.Results[0]["url"] != "https://go.dev" || got.Results[0]["content"] != "The Go language" {
		t.Fatalf("results %s", w.Body)
	}
	if want := "go release (site:go.dev OR site:github.com) -site:spam.example"; f.queries[0] != want {
		t.Fatalf("query %q, want %q", f.queries[0], want)
	}
}

func TestSearchErrors(t *testing.T) {
	if w := postSearch(NewSearchHandler(&fakeSearch{configured: true}), `{"query":"  "}`); w.Code != 400 {
		t.Fatalf("empty query: %d", w.Code)
	}
	if w := postSearch(NewSearchHandler(&fakeSearch{}), `{"query":"x"}`); w.Code != 503 {
		t.Fatalf("not configured: %d", w.Code)
	}
	if w := postSearch(NewSearchHandler(&fakeSearch{configured: true, err: errors.New("tavily returned HTTP 429")}), `{"query":"x"}`); w.Code != 429 {
		t.Fatalf("rate limited: %d", w.Code)
	}
	w := postSearch(NewSearchHandler(&fakeSearch{configured: true, err: errors.New("tavily returned HTTP 500 key sk-secret")}), `{"query":"x"}`)
	if w.Code != 502 || strings.Contains(w.Body.String(), "sk-secret") {
		t.Fatalf("upstream error: %d %s", w.Code, w.Body)
	}
}

func TestCapabilitiesListServicesAndDefaults(t *testing.T) {
	cs := config.NewTestConfigStore(&config.Config{ClientDefaults: &config.ClientDefaults{Main: "GLM-5.3-Flash", Opus: "glm-5.3-zai"}})
	w := httptest.NewRecorder()
	NewCapabilitiesHandler(cs, &fakeSearch{configured: true}).ServeHTTP(w, httptest.NewRequest("GET", "/v1/capabilities", nil))
	var got map[string]any
	json.Unmarshal(w.Body.Bytes(), &got)
	d, _ := got["defaults"].(map[string]any)
	if got["service"] != "go-llm-proxy" || got["web_search"] != "/v1/search" || got["documents"] != nil || d["main"] != "GLM-5.3-Flash" || d["opus"] != "glm-5.3-zai" {
		t.Fatalf("%s", w.Body)
	}
	w = httptest.NewRecorder()
	NewCapabilitiesHandler(config.NewTestConfigStore(&config.Config{}), &fakeSearch{}).ServeHTTP(w, httptest.NewRequest("GET", "/v1/capabilities", nil))
	if !strings.Contains(w.Body.String(), `"defaults":{}`) || !strings.Contains(w.Body.String(), `"web_search":null`) ||
		!strings.Contains(w.Body.String(), `"documents_restructure":null`) {
		t.Fatalf("%s", w.Body)
	}
	w = httptest.NewRecorder()
	docs := config.NewTestConfigStore(&config.Config{Documents: config.DocumentsConfig{PaddleOCR: &config.PaddleOCRConfig{}}})
	NewCapabilitiesHandler(docs, &fakeSearch{}).ServeHTTP(w, httptest.NewRequest("GET", "/v1/capabilities", nil))
	got = nil
	json.Unmarshal(w.Body.Bytes(), &got)
	if got["documents"] != "/layout-parsing" || got["documents_restructure"] != "/restructure-pages" {
		t.Fatalf("documents: %s", w.Body)
	}
}

func TestCapabilitiesListCodeLinkModelsTheKeyMayUse(t *testing.T) {
	cs := config.NewTestConfigStore(&config.Config{
		Models: []config.ModelConfig{{Name: "GLM-5.3-Flash", CodeLink: true}, {Name: "nomic-embed"}, {Name: "deepseek-4.1-flash-1017", CodeLink: true}},
		Keys: []config.KeyConfig{{Key: "all-models-key-0001", Name: "all"},
			{Key: "limited-key-000001", Name: "limited", Models: []string{"deepseek-4.1-flash-1017", "nomic-embed"}}}}) // gitleaks:allow
	h := auth.AuthMiddleware(cs, NewCapabilitiesHandler(cs, &fakeSearch{}))
	get := func(key string) []any {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/v1/capabilities", nil)
		r.Header.Set("Authorization", "Bearer "+key)
		h.ServeHTTP(w, r)
		var got map[string]any
		json.Unmarshal(w.Body.Bytes(), &got)
		m, _ := got["models"].([]any)
		return m
	}
	if m := get("all-models-key-0001"); len(m) != 2 || m[0] != "GLM-5.3-Flash" || m[1] != "deepseek-4.1-flash-1017" {
		t.Fatalf("all marked: %v", m)
	}
	if m := get("limited-key-000001"); len(m) != 1 || m[0] != "deepseek-4.1-flash-1017" {
		t.Fatalf("key-limited: %v", m)
	}
	none := config.NewTestConfigStore(&config.Config{Models: []config.ModelConfig{{Name: "a"}}})
	w := httptest.NewRecorder()
	NewCapabilitiesHandler(none, &fakeSearch{}).ServeHTTP(w, httptest.NewRequest("GET", "/v1/capabilities", nil))
	if !strings.Contains(w.Body.String(), `"models":null`) {
		t.Fatalf("unmarked: %s", w.Body)
	}
}

func TestCapabilitiesListNativeClassifierModels(t *testing.T) {
	cs := config.NewTestConfigStore(&config.Config{Models: []config.ModelConfig{
		{Name: "a", NativeClassifier: true}, {Name: "b"}, {Name: "c", NativeClassifier: true},
	}})
	w := httptest.NewRecorder()
	NewCapabilitiesHandler(cs, &fakeSearch{}).ServeHTTP(w, httptest.NewRequest("GET", "/v1/capabilities", nil))
	var got struct {
		Native []string `json:"native_classifier_models"`
	}
	json.Unmarshal(w.Body.Bytes(), &got)
	if len(got.Native) != 2 || got.Native[0] != "a" || got.Native[1] != "c" {
		t.Fatalf("%s", w.Body)
	}
	w = httptest.NewRecorder()
	NewCapabilitiesHandler(config.NewTestConfigStore(&config.Config{}), &fakeSearch{}).ServeHTTP(w, httptest.NewRequest("GET", "/v1/capabilities", nil))
	if !strings.Contains(w.Body.String(), `"native_classifier_models":[]`) {
		t.Fatalf("%s", w.Body)
	}
}
