package handler

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"go-llm-proxy/internal/auth"
	"go-llm-proxy/internal/config"
	"go-llm-proxy/internal/httputil"
	"go-llm-proxy/internal/pipeline"
)

// Searcher is the web-search cascade the search endpoint uses.
type Searcher interface {
	SearchGlobal(ctx context.Context, query string) ([]pipeline.SearchHit, error)
	SearchConfigured() bool
}

// SearchHandler serves POST /v1/search: the proxy's own web search (the
// Tavily/Brave cascade in processors.web_search_keys) for clients that hold
// only a proxy key, such as code-link.
//
//	{"query": "…", "include_domains": ["…"], "exclude_domains": ["…"]}
//	→ {"results": [{"title", "url", "content"}]}
type SearchHandler struct{ search Searcher }

func NewSearchHandler(s Searcher) *SearchHandler { return &SearchHandler{search: s} }

func (h *SearchHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Query          string   `json:"query"`
		IncludeDomains []string `json:"include_domains"`
		ExcludeDomains []string `json:"exclude_domains"`
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil || json.Unmarshal(body, &req) != nil {
		httputil.WriteError(w, http.StatusBadRequest, "body must be JSON with a query")
		return
	}
	q := strings.TrimSpace(req.Query)
	if q == "" || len(q) > 2000 {
		httputil.WriteError(w, http.StatusBadRequest, "query must be 1 to 2000 characters")
		return
	}
	if !h.search.SearchConfigured() {
		httputil.WriteError(w, http.StatusServiceUnavailable, "web search is not configured on this proxy")
		return
	}
	hits, err := h.search.SearchGlobal(r.Context(), withDomains(q, req.IncludeDomains, req.ExcludeDomains))
	if err != nil {
		status := http.StatusBadGateway
		if strings.Contains(err.Error(), "HTTP 429") {
			status = http.StatusTooManyRequests
		}
		httputil.WriteError(w, status, "web search failed")
		return
	}
	results := make([]map[string]string, 0, len(hits))
	for _, hit := range hits {
		results = append(results, map[string]string{"title": hit.Title, "url": hit.URL, "content": hit.Snippet})
	}
	httputil.SetSecurityHeaders(w)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"results": results})
}

// withDomains folds domain filters into the query as site: terms, which
// both Tavily and Brave understand.
func withDomains(q string, include, exclude []string) string {
	var sites []string
	for _, d := range include {
		if d = strings.TrimSpace(d); d != "" {
			sites = append(sites, "site:"+d)
		}
	}
	if len(sites) > 1 {
		q += " (" + strings.Join(sites, " OR ") + ")"
	} else if len(sites) == 1 {
		q += " " + sites[0]
	}
	for _, d := range exclude {
		if d = strings.TrimSpace(d); d != "" {
			q += " -site:" + d
		}
	}
	return q
}

// CapabilitiesHandler serves GET /v1/capabilities: which of the proxy's
// services a key can use besides chat, and where, so a client can turn them
// on without keys of its own. Paths are from the proxy's root; a service
// that isn't configured is null.
//
//	{"service": "go-llm-proxy", "web_search": "/v1/search", "documents": "/layout-parsing",
//	 "documents_restructure": "/restructure-pages",
//	 "defaults": {"main": "GLM-5.3-Flash", …}, "models": ["GLM-5.3-Flash", …]}
//
// models are those marked code_link that the key may use, in config
// order; null when no model is marked (the client then decides).
type CapabilitiesHandler struct {
	config *config.ConfigStore
	search Searcher
}

func NewCapabilitiesHandler(cs *config.ConfigStore, s Searcher) *CapabilitiesHandler {
	return &CapabilitiesHandler{config: cs, search: s}
}

func (h *CapabilitiesHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	httputil.SetSecurityHeaders(w)
	w.Header().Set("Content-Type", "application/json")
	cfg := h.config.Get()
	caps := map[string]any{"service": "go-llm-proxy", "web_search": nil, "documents": nil, "documents_restructure": nil, "defaults": map[string]any{}}
	if d := cfg.ClientDefaults; d != nil {
		caps["defaults"] = d
	}
	if h.search.SearchConfigured() {
		caps["web_search"] = "/v1/search"
	}
	if cfg.Documents.PaddleOCR != nil {
		caps["documents"] = "/layout-parsing"
		caps["documents_restructure"] = "/restructure-pages"
	}
	caps["models"] = nil
	key := auth.KeyFromContext(r.Context())
	marked := false
	offered := []string{}
	for _, m := range cfg.Models {
		if !m.CodeLink {
			continue
		}
		marked = true
		if auth.KeyAllowsModel(key, m.Name) {
			offered = append(offered, m.Name)
		}
	}
	if marked {
		caps["models"] = offered
	}
	json.NewEncoder(w).Encode(caps)
}
