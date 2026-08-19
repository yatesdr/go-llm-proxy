package handler

import (
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"time"

	"go-llm-proxy/internal/config"
	"go-llm-proxy/internal/httputil"
	"go-llm-proxy/internal/lb"
)

var awsRegions = []string{
	"us-east-1", "us-east-2", "us-west-1", "us-west-2",
	"ca-central-1", "sa-east-1", "eu-west-1", "eu-west-2", "eu-west-3",
	"eu-central-1", "eu-north-1", "eu-south-1", "ap-south-1",
	"ap-northeast-1", "ap-northeast-2", "ap-northeast-3", "ap-southeast-1",
	"ap-southeast-2", "ap-southeast-4", "me-south-1", "me-central-1", "af-south-1",
}

// ModelsPage is the Chat workload editor. /admin/models remains an alias so
// bookmarks from earlier releases continue to work.
func (h *AdminHandler) ModelsPage(w http.ResponseWriter, r *http.Request) {
	body := `<div class="subtabs">
  <button class="subtab active" id="subtabModels" type="button" onclick="switchLLMTab('models')">Models</button>
  <button class="subtab" id="subtabHelpers" type="button" onclick="switchLLMTab('helpers')">Model Helpers</button>
  <button class="subtab" id="subtabAliasing" type="button" onclick="switchLLMTab('aliasing')">Model Aliasing</button>
</div>
<div id="pane-models">
<div class="card">
  <div class="card-header">
    <h2>LLM Models</h2>
    <div class="card-tools">
      <input id="modelFilter" class="filter-input" type="search" placeholder="Filter models&hellip;" autocomplete="off">
      <button class="btn btn-primary" type="button" onclick="openModel(null)">+ Add Model</button>
    </div>
  </div>
  <div class="table-wrap"><table class="data-table">
    <thead><tr><th>Name</th><th style="width:90px">Protocol</th><th>Upstream model</th><th style="width:180px" title="One dot per backend server — click the row for detail">Backends</th><th style="width:110px" title="Native input the model accepts beyond text">Multi-Modal</th><th style="width:130px" title="Intercepts applied by the proxy for this model">Augmented</th><th style="width:130px;text-align:right">Actions</th></tr></thead>
    <tbody id="modelsBody"><tr><td colspan="7" class="empty-cell">Loading…</td></tr></tbody>
  </table></div>
</div>
</div>
<div id="pane-helpers" style="display:none">
<div class="card">
  <h2>Vision</h2>
  <p class="helper-text" style="margin:0 0 10px">Images sent to a model without native vision support are described by the first model below, in order. On failure or an empty result, the proxy tries the next.</p>
  <div id="visionCascade" class="cascade-list"></div>
  <div class="cascade-add"><select id="visionAddSelect"></select><button type="button" class="btn btn-secondary btn-sm" onclick="addVisionModel()">+ Add</button></div>
</div>
<div class="card">
  <h2>Web Search</h2>
  <p class="helper-text" style="margin:0 0 10px">Search providers are tried in order; the proxy falls back to the next on failure. Supports Tavily and Brave.</p>
  <div id="searchCascade" class="cascade-list"></div>
  <div class="cascade-add">
    <select id="searchAddProvider"><option value="tavily">Tavily</option><option value="brave">Brave</option></select>
    <input id="searchAddKey" type="password" placeholder="API key" style="max-width:260px">
    <button type="button" class="btn btn-secondary btn-sm" onclick="addSearchKey()">+ Add</button>
  </div>
</div>
<div class="btn-row"><button class="btn btn-primary" type="button" onclick="saveHelpers()">Save helpers</button></div>
</div>
<div id="pane-aliasing" style="display:none">
<div class="card">
  <h2>Aliases &amp; unrecognized requests</h2>
  <p class="helper-text" style="margin:0 0 12px">Unrecognized requests are kept in memory since this proxy restart and capped at 200 entries.</p>
  <h3>Unrecognized requests</h3>
  <div class="table-wrap"><table class="data-table">
    <thead><tr><th>Model ID</th><th style="width:80px">Count</th><th style="width:180px">Last seen</th><th style="width:150px">Endpoint</th><th style="width:290px">Alias to</th></tr></thead>
    <tbody id="unknownModelsBody"><tr><td colspan="5" class="empty-cell">Loading…</td></tr></tbody>
  </table></div>
  <h3 style="margin-top:18px">Configured aliases</h3>
  <div class="table-wrap"><table class="data-table">
    <thead><tr><th>Alias</th><th>Target model</th><th style="width:100px;text-align:right">Actions</th></tr></thead>
    <tbody id="aliasesBody"><tr><td colspan="3" class="empty-cell">Loading…</td></tr></tbody>
  </table></div>
</div>
</div>` + modelModalHTML()
	h.renderShell(w, "chat", "Admin · LLM", body, modelsPageJS())
}

func (h *AdminHandler) ModelsData(w http.ResponseWriter, r *http.Request) {
	cfg := h.cs.Get()
	var modelHealth map[string]config.ModelHealth
	var backendHealth map[string]config.BackendHealth
	if h.health != nil {
		modelHealth = h.health.GetStatus()
		backendHealth = h.health.GetBackendStatus()
	}
	models := make([]map[string]any, 0, len(cfg.Models))
	for _, m := range cfg.Models {
		backends := backendData(m.Backends, backendHealth)
		entry := map[string]any{
			"name": m.Name, "type": m.Type, "model": m.Model, "timeout": m.Timeout,
			"context_window": m.ContextWindow, "supports_vision": m.SupportsVision, "supports_audio": m.SupportsAudio,
			"force_pipeline": m.ForcePipeline, "rewrite_vision": m.RewriteVision, "rewrite_documents": m.RewriteDocuments, "rewrite_web_search": m.RewriteWebSearch, "responses_mode": m.ResponsesMode,
			"messages_mode": m.MessagesMode, "backends": backends,
			"region": m.Region, "aws_access_key": m.AWSAccessKey,
			"has_aws_secret": m.AWSSecretKey != "", "aws_secret_mask": config.MaskSecret(m.AWSSecretKey),
			"has_aws_session": m.AWSSessionToken != "", "guardrail_id": m.GuardrailID,
			"guardrail_version": m.GuardrailVersion, "guardrail_trace": m.GuardrailTrace,
		}
		// Retain these response fields for older admin-page tests/clients. The
		// active editor uses the backends array and never writes model api_key.
		if len(m.Backends) > 0 {
			entry["backend"] = m.Backends[0].URL
			entry["has_api_key"] = m.Backends[0].APIKey != ""
			entry["api_key_masked"] = config.MaskSecret(m.Backends[0].APIKey)
		}
		if m.Defaults != nil {
			entry["defaults"] = samplingDefaultsToMap(m.Defaults)
		}
		if hs, ok := modelHealth[m.Name]; ok {
			entry["health"] = map[string]any{"online": hs.Online, "external": hs.External, "error": hs.Error}
		}
		models = append(models, entry)
	}
	aliasNames := make([]string, 0, len(cfg.Aliases))
	for alias := range cfg.Aliases {
		aliasNames = append(aliasNames, alias)
	}
	sort.Strings(aliasNames)
	aliases := make([]map[string]any, 0, len(aliasNames))
	for _, alias := range aliasNames {
		aliases = append(aliases, map[string]any{"alias": alias, "target": cfg.Aliases[alias]})
	}
	unknown := Snapshot()
	unknownModels := make([]map[string]any, 0, len(unknown))
	for _, entry := range unknown {
		unknownModels = append(unknownModels, map[string]any{
			"id": entry.ID, "count": entry.Count,
			"last_seen": entry.LastSeen.UTC().Format(time.RFC3339), "endpoint": entry.LastEndpoint,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"models": models, "regions": awsRegions, "aliases": aliases, "unknown_models": unknownModels,
		"helpers": map[string]any{
			"vision": cfg.Processors.Vision, "has_search_key": cfg.Processors.WebSearchKey != "",
			"search_key_mask": config.MaskSecret(cfg.Processors.WebSearchKey),
			"legacy_audio":    cfg.Processors.Audio, "legacy_ocr": cfg.Processors.OCR,
			"vision_models":   cfg.Processors.EffectiveVisionModels(),
			"web_search_keys": searchKeyEntriesJSON(cfg.Processors.EffectiveSearchKeys()),
		},
	})
}

func searchKeyEntriesJSON(entries []config.WebSearchKeyEntry) []map[string]any {
	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		out = append(out, map[string]any{"provider": e.Provider, "key_mask": config.MaskKey(e.Key)})
	}
	return out
}

func backendData(backends []config.BackendConfig, health map[string]config.BackendHealth) []map[string]any {
	out := make([]map[string]any, 0, len(backends))
	for _, b := range backends {
		live := lb.Status(b.URL)
		row := map[string]any{
			"url": b.URL, "has_api_key": b.APIKey != "", "api_key_mask": config.MaskSecret(b.APIKey),
			"weight": b.Weight, "max_inflight": b.MaxInflight, "disabled": b.Disabled,
			"inflight": live.Inflight, "breaker": live.Breaker,
		}
		if bh, ok := health[b.URL]; ok {
			row["online"] = bh.Online
			row["error"] = bh.Error
		}
		out = append(out, row)
	}
	return out
}

type backendInputDTO struct {
	URL         string  `json:"url"`
	APIKey      *string `json:"api_key"`
	Weight      int     `json:"weight"`
	MaxInflight int     `json:"max_inflight"`
	Disabled    bool    `json:"disabled"`
}

func resolveBackendRows(existing []config.BackendConfig, rows []backendInputDTO) []config.BackendConfig {
	keyForURL := func(target string) string {
		for _, b := range existing {
			if b.URL == target {
				return b.APIKey
			}
		}
		return ""
	}
	out := make([]config.BackendConfig, 0, len(rows))
	for _, row := range rows {
		b := config.BackendConfig{URL: row.URL, Weight: row.Weight, MaxInflight: row.MaxInflight, Disabled: row.Disabled}
		if row.APIKey != nil {
			b.APIKey = *row.APIKey
		} else {
			b.APIKey = keyForURL(row.URL)
		}
		out = append(out, b)
	}
	return out
}

func (h *AdminHandler) ModelsMutate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action       string         `json:"action"`
		OriginalName string         `json:"original_name"`
		Name         string         `json:"name"`
		Alias        string         `json:"alias"`
		Target       string         `json:"target"`
		Force        bool           `json:"force"`
		Model        *modelInputDTO `json:"model"`
	}
	if err := decodeJSONBody(r, &req, 128*1024); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	switch req.Action {
	case "add", "update":
		if req.Model == nil {
			httputil.WriteError(w, http.StatusBadRequest, "model is required")
			return
		}
		if req.Action == "update" && req.OriginalName == "" {
			httputil.WriteError(w, http.StatusBadRequest, "original_name is required")
			return
		}
		mc, err := req.Model.toConfig(h.cs.Get(), req.OriginalName)
		if err == nil {
			if req.Action == "add" {
				err = h.cs.AddModel(mc)
			} else {
				err = h.cs.UpdateModel(req.OriginalName, mc)
			}
		}
		if err != nil {
			writeMutateError(w, err)
			return
		}
		slog.Info("admin: chat model saved", "name", mc.Name, "backends", len(mc.Backends))
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": mc.Name})
	case "delete":
		if err := h.cs.DeleteModel(req.Name, req.Force); err != nil {
			writeMutateError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case "alias_add":
		if err := h.cs.AddAlias(req.Alias, req.Target); err != nil {
			writeMutateError(w, err)
			return
		}
		Remove(req.Alias)
		slog.Info("admin: model alias added", "alias", req.Alias, "target", req.Target)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case "alias_delete":
		if err := h.cs.DeleteAlias(req.Alias); err != nil {
			writeMutateError(w, err)
			return
		}
		slog.Info("admin: model alias deleted", "alias", req.Alias)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		httputil.WriteError(w, http.StatusBadRequest, "unknown action")
	}
}

// BackendsProbe validates a candidate backend without saving it. If the key
// is omitted, it is recovered from the matching configured model/backend.
func (h *AdminHandler) BackendsProbe(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL, Model, Type string
		UpstreamModel    string  `json:"upstream_model"`
		APIKey           *string `json:"api_key"`
	}
	if err := decodeJSONBody(r, &req, 16*1024); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if u, err := url.Parse(req.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		httputil.WriteError(w, http.StatusBadRequest, "url must be a valid http(s) URL")
		return
	}
	key := ""
	if req.APIKey != nil {
		key = *req.APIKey
	} else if m := config.FindModel(h.cs.Get(), req.Model); m != nil {
		for _, b := range m.Backends {
			if b.URL == req.URL {
				key = b.APIKey
				break
			}
		}
	}
	detectModel := req.UpstreamModel
	if detectModel == "" {
		detectModel = req.Model
	}
	writeJSON(w, http.StatusOK, config.ProbeBackendForModel(req.URL, key, req.Type, detectModel))
}

type modelInputDTO struct {
	Name             string              `json:"name"`
	Backend          string              `json:"backend"` // legacy admin client
	Type             string              `json:"type"`
	Model            string              `json:"model"`
	ResponsesMode    *string             `json:"responses_mode"` // preserved when omitted; the admin UI no longer surfaces this
	MessagesMode     *string             `json:"messages_mode"`  // preserved when omitted; the admin UI no longer surfaces this
	Timeout          int                 `json:"timeout"`
	ContextWindow    int                 `json:"context_window"`
	SupportsVision   bool                `json:"supports_vision"`
	SupportsAudio    *bool               `json:"supports_audio"` // legacy capability; hidden in the new UI
	ForcePipeline    bool                `json:"force_pipeline"`
	RewriteVision    string              `json:"rewrite_vision"`
	RewriteDocuments string              `json:"rewrite_documents"`
	RewriteWebSearch string              `json:"rewrite_web_search"`
	Backends         []backendInputDTO   `json:"backends"`
	APIKey           *string             `json:"api_key"` // legacy admin client
	Region           string              `json:"region"`
	AWSAccessKey     string              `json:"aws_access_key"`
	AWSSecretKey     *string             `json:"aws_secret_key"`
	AWSSession       *string             `json:"aws_session_token"`
	GuardrailID      string              `json:"guardrail_id"`
	GuardrailVersion string              `json:"guardrail_version"`
	GuardrailTrace   string              `json:"guardrail_trace"`
	Defaults         *samplingInputDTO   `json:"defaults"`
	Processors       *processorsInputDTO `json:"processors"`
}

type samplingInputDTO struct {
	Temperature      *float64 `json:"temperature"`
	TopP             *float64 `json:"top_p"`
	TopK             *int     `json:"top_k"`
	MaxNewTokens     *int     `json:"max_new_tokens"`
	FrequencyPenalty *float64 `json:"frequency_penalty"`
	PresencePenalty  *float64 `json:"presence_penalty"`
	ReasoningEffort  *string  `json:"reasoning_effort"`
	Stop             []string `json:"stop"`
}

type processorsInputDTO struct {
	Vision       string  `json:"vision"`
	OCR          string  `json:"ocr"`
	WebSearchKey *string `json:"web_search_key"`
}

func (d *modelInputDTO) toConfig(cfg *config.Config, originalName string) (config.ModelConfig, error) {
	existing := config.FindModel(cfg, originalName)
	rows := d.Backends
	if len(rows) == 0 && d.Backend != "" { // compatibility with the old admin payload
		rows = []backendInputDTO{{URL: d.Backend, APIKey: d.APIKey, Weight: 1}}
	}
	var existingBackends []config.BackendConfig
	if existing != nil {
		existingBackends = existing.Backends
	}
	m := config.ModelConfig{
		Name: d.Name, Backends: resolveBackendRows(existingBackends, rows), Type: d.Type, Model: d.Model,
		Timeout: d.Timeout, ContextWindow: d.ContextWindow, SupportsVision: d.SupportsVision,
		ForcePipeline: d.ForcePipeline, RewriteVision: d.RewriteVision, RewriteDocuments: d.RewriteDocuments, RewriteWebSearch: d.RewriteWebSearch,
		Region: d.Region, AWSAccessKey: d.AWSAccessKey, GuardrailID: d.GuardrailID,
		GuardrailVersion: d.GuardrailVersion, GuardrailTrace: d.GuardrailTrace,
	}
	if d.SupportsAudio != nil {
		m.SupportsAudio = *d.SupportsAudio
	} else if existing != nil {
		m.SupportsAudio = existing.SupportsAudio
	}
	if existing != nil {
		// These Bedrock fields are deliberately absent from the compact editor;
		// preserve them unless a caller explicitly supplies replacements.
		if m.GuardrailID == "" {
			m.GuardrailID = existing.GuardrailID
		}
		if m.GuardrailVersion == "" {
			m.GuardrailVersion = existing.GuardrailVersion
		}
		if m.GuardrailTrace == "" {
			m.GuardrailTrace = existing.GuardrailTrace
		}
	}
	// responses_mode/messages_mode are advanced escape hatches no longer
	// surfaced in the admin UI; preserve a hand-edited value unless the
	// caller explicitly supplies a replacement (nil = omitted).
	if d.ResponsesMode != nil {
		m.ResponsesMode = *d.ResponsesMode
	} else if existing != nil {
		m.ResponsesMode = existing.ResponsesMode
	}
	if d.MessagesMode != nil {
		m.MessagesMode = *d.MessagesMode
	} else if existing != nil {
		m.MessagesMode = existing.MessagesMode
	}
	m.AWSSecretKey = resolveModelSecret(d.AWSSecretKey, existing, func(x *config.ModelConfig) string { return x.AWSSecretKey })
	m.AWSSessionToken = resolveModelSecret(d.AWSSession, existing, func(x *config.ModelConfig) string { return x.AWSSessionToken })
	if d.Defaults != nil {
		m.Defaults = &config.SamplingDefaults{Temperature: d.Defaults.Temperature, TopP: d.Defaults.TopP, TopK: d.Defaults.TopK, MaxNewTokens: d.Defaults.MaxNewTokens, FrequencyPenalty: d.Defaults.FrequencyPenalty, PresencePenalty: d.Defaults.PresencePenalty, ReasoningEffort: d.Defaults.ReasoningEffort, Stop: d.Defaults.Stop}
		if existing != nil && existing.Defaults != nil {
			m.Defaults.MaxNewTokens = existing.Defaults.MaxNewTokens
			m.Defaults.FrequencyPenalty = existing.Defaults.FrequencyPenalty
			m.Defaults.PresencePenalty = existing.Defaults.PresencePenalty
			m.Defaults.Stop = existing.Defaults.Stop
		}
	} else if existing != nil {
		m.Defaults = existing.Defaults
	}
	if d.Processors != nil { // legacy API compatibility; hidden in the new UI
		webKey := ""
		if d.Processors.WebSearchKey != nil {
			webKey = *d.Processors.WebSearchKey
		} else if existing != nil && existing.Processors != nil {
			webKey = existing.Processors.WebSearchKey
		}
		m.Processors = &config.ProcessorsConfig{Vision: d.Processors.Vision, OCR: d.Processors.OCR, WebSearchKey: webKey}
	} else if existing != nil {
		m.Processors = existing.Processors
	}
	return m, nil
}

func resolveModelSecret(in *string, existing *config.ModelConfig, get func(*config.ModelConfig) string) string {
	if in != nil {
		return *in
	}
	if existing != nil {
		return get(existing)
	}
	return ""
}

func samplingDefaultsToMap(d *config.SamplingDefaults) map[string]any {
	out := map[string]any{}
	if d.Temperature != nil {
		out["temperature"] = *d.Temperature
	}
	if d.TopP != nil {
		out["top_p"] = *d.TopP
	}
	if d.TopK != nil {
		out["top_k"] = *d.TopK
	}
	if d.MaxNewTokens != nil {
		out["max_new_tokens"] = *d.MaxNewTokens
	}
	if d.FrequencyPenalty != nil {
		out["frequency_penalty"] = *d.FrequencyPenalty
	}
	if d.PresencePenalty != nil {
		out["presence_penalty"] = *d.PresencePenalty
	}
	if d.ReasoningEffort != nil {
		out["reasoning_effort"] = *d.ReasoningEffort
	}
	if len(d.Stop) > 0 {
		out["stop"] = d.Stop
	}
	return out
}

func modelModalHTML() string {
	return `<div id="modelModal" class="modal-backdrop" onclick="if(event.target.id==='modelModal')closeModel()">
  <div class="modal" style="max-width:920px;width:calc(100vw - 32px)" role="dialog">
    <div class="modal-header"><h2 id="modelTitle">Model</h2><button class="modal-close" onclick="closeModel()" type="button">&times;</button></div>
    <form id="modelForm" onsubmit="saveModel(event)"><div class="modal-body">
      <div class="section"><h3>Model</h3><div class="field-grid">
        <div class="field"><label title="The model name clients send in their requests, e.g. &quot;model&quot;: &quot;glm-5.2&quot;. This is what the proxy publishes &mdash; it can differ from the upstream model ID set below with the backends.">Published model ID</label><input name="name" type="text" required placeholder="GLM-5.2"></div>
        <div class="field"><label title="How long the proxy waits for the backend to respond before giving up.">Timeout (seconds)</label><input name="timeout" type="number" min="1" placeholder="300"></div>
        <div class="field"><label title="Max input+output tokens the model supports. Shown to clients and used for capacity checks; leave blank to auto-detect where possible.">Context window</label><div class="secret-row"><input name="context_window" type="number" min="0" placeholder="auto-detect" style="flex:1"><button type="button" class="btn btn-secondary btn-sm" onclick="detectContextWindow(this)">Detect</button></div><div class="hint" id="ctxDetectStatus" style="display:none;margin-top:4px"></div></div>
        <div class="field">
          <div class="grid-3">
            <div class="field"><label title="Images are described by the vision helper cascade before reaching this model, instead of being forwarded as-is.">Vision</label><select name="rewrite_vision" id="rewriteVisionSelect"><option value="">Auto</option><option value="on">Rewrite</option><option value="off">None</option></select></div>
            <div class="field"><label title="PDFs are handled by the proxy before reaching this model: native text extraction first, then the Documents-tab processor, an OCR model, or vision-PDF fallback.">Documents</label><select name="rewrite_documents" id="rewriteDocumentsSelect"><option value="">Auto</option><option value="on">Rewrite</option><option value="off">None</option></select></div>
            <div class="field"><label title="The web_search tool is added to requests for this model when a search key is configured, and the proxy executes it server-side.">Web search</label><select name="rewrite_web_search" id="rewriteSearchSelect"><option value="">Auto</option><option value="on">Rewrite</option><option value="off">None</option></select></div>
          </div>
        </div>
      </div></div>
      <div class="section">
        <h3 style="margin-bottom:6px">Backends</h3>
        <div class="field-grid" style="margin-bottom:12px">
          <div class="field"><label title="The model name sent to every backend below (leave blank to reuse the published model ID), and the API shape they all speak. All backends in this pool must serve the same upstream model.">Upstream model ID</label><div class="secret-row"><input name="model" type="text" placeholder="same as published model ID" style="flex:1"><select name="type" onchange="toggleBedrock()" style="max-width:140px" title="Upstream protocol"><option value="">OpenAI</option><option value="anthropic">Anthropic</option><option value="bedrock">Bedrock</option></select></div></div>
          <div class="field checkbox-row"><input id="supportsVision" name="supports_vision" type="checkbox" onchange="updateAutoHints()"><label for="supportsVision">Vision capable</label></div>
        </div>
        <div class="be-head"><span></span><span>Server URL</span><span>API key</span><span title="Load-balancing weight — higher gets more traffic relative to other backends">Wt</span><span title="Max concurrent requests to this backend (0 = unlimited)">Max</span><span title="Temporarily disabled — kept in the list but never routed to">Off</span><span></span></div>
        <div id="backendRows"></div>
        <div style="display:flex;justify-content:flex-end;margin-top:2px"><button type="button" class="btn btn-secondary btn-sm" onclick="addBackend(null)">+ Add Backend</button></div>
      </div>
      <div class="section"><h3>Sampling defaults</h3><div class="grid-4">
        <div class="field"><label title="Higher = more random/creative output, lower = more focused and deterministic. Leave blank to use the backend's own default.">Temperature</label><input name="temperature" type="number" step="0.05" min="0" max="2" placeholder="backend default"></div>
        <div class="field"><label title="Nucleus sampling: only considers tokens within this cumulative probability mass. Leave blank to use the backend's own default.">Top-p</label><input name="top_p" type="number" step="0.05" min="0" max="1" placeholder="backend default"></div>
        <div class="field"><label title="Only considers the k most likely next tokens at each step. Leave blank to use the backend's own default.">Top-k</label><input name="top_k" type="number" min="0" placeholder="backend default"></div>
        <div class="field"><label title="Requested thinking depth for reasoning-capable models. Ignored by models that don't support it.">Reasoning effort</label><select name="reasoning_effort"><option value="">Backend default</option><option>low</option><option>medium</option><option>high</option></select></div>
      </div></div>
      <div id="bedrockFields" class="section" style="display:none"><h3>Bedrock</h3><div class="field-grid">
        <div class="field"><label title="AWS region hosting this Bedrock model, e.g. us-east-1.">Region</label><input name="region" type="text" placeholder="us-east-1"></div><div class="field"><label title="IAM access key with bedrock:InvokeModel permission for this model.">AWS access key ID</label><input name="aws_access_key" type="text"></div>
        <div class="field"><label>AWS secret key</label><input name="aws_secret_key" type="password" placeholder="leave blank to keep"></div><div class="field"><label title="Only needed for temporary/STS credentials, not long-lived IAM keys.">Session token</label><input name="aws_session_token" type="password" placeholder="leave blank to keep"></div>
      </div></div>
      <div id="modelErr" class="inline-err" style="display:none"></div>
    </div><div class="modal-footer"><button type="button" class="btn btn-secondary" onclick="closeModel()">Cancel</button><button id="modelSave" class="btn btn-primary">Save model</button></div></form>
  </div>
</div>`
}

func modelsPageJS() string {
	return `
var chatState={models:[],aliases:[],unknownModels:[],editing:null,helpers:{},searchAction:null};
function loadModels(){apiGet('/admin/models/data').then(function(d){chatState.models=d.models||[];chatState.aliases=d.aliases||[];chatState.unknownModels=d.unknown_models||[];chatState.helpers=d.helpers||{};renderModels();renderAliases();renderHelpers();}).catch(function(e){flash('Load failed: '+e.message,'error');});}
function backendDot(b){
  var cls='health-unknown',label='no health data';
  if(b.disabled){cls='health-unknown';label='disabled';}
  else if(b.online===true){cls='health-online';label='online';}
  else if(b.online===false){cls='health-offline';label='offline'+(b.error?': '+b.error:'');}
  return '<span class="health-dot '+cls+'" title="'+escAttr(b.url)+' — '+escAttr(label)+'"></span>';
}
function modelDot(m){
  var h=m.health||{};
  if(h.online){
    var down=(m.backends||[]).filter(function(b){return !b.disabled&&b.online===false;}).length;
    if(down>0)return '<span class="health-dot health-degraded" title="Degraded: '+down+' backend(s) down"></span>';
    return '<span class="health-dot health-online" title="Online"></span>';
  }
  if(h.error)return '<span class="health-dot health-offline" title="Offline: '+escAttr(h.error)+'"></span>';
  return '<span class="health-dot health-unknown" title="No health data"></span>';
}
function renderModels(){var el=document.getElementById('modelsBody');if(!chatState.models.length){el.innerHTML='<tr><td colspan="7" class="empty-cell">No models configured</td></tr>';return;}el.innerHTML=chatState.models.map(function(m){var bs=m.backends||[];var active=bs.filter(function(b){return !b.disabled;}).length;var dots=bs.map(backendDot).join('');return '<tr>'+
  '<td>'+modelDot(m)+'<strong>'+esc(m.name)+'</strong></td>'+
  '<td class="mono">'+esc(m.type||'openai')+'</td>'+
  '<td class="mono">'+esc(m.model||m.name)+' <button class="copy-icon" type="button" title="Copy upstream model id" onclick="event.stopPropagation();copyToClipboard(\''+escAttr(m.model||m.name)+'\').then(function(){flash(\'Copied\',\'success\');})"><svg viewBox="0 0 16 16" width="13" height="13" fill="none" stroke="currentColor" stroke-width="1.5"><rect x="5" y="5" width="9" height="9"/><path d="M11 5V2H2v9h3"/></svg></button></td>'+
  '<td>'+dots+' <span class="mono">'+active+'/'+bs.length+'</span></td>'+
  '<td class="mono">'+(function(){var c=[];if(m.supports_vision)c.push('vision');if(m.supports_audio)c.push('audio');return c.length?c.join(' \u00b7 '):'\u2014';})()+'</td>'+
  '<td class="mono">'+(function(){var g=[];if(m.force_pipeline)g.push('pipeline');if(chatState.helpers.vision&&!m.supports_vision)g.push('vision');if(chatState.helpers.has_search_key)g.push('search');return g.length?g.join(' \u00b7 '):'\u2014';})()+'</td>'+
  '<td class="row-actions"><div class="action-group"><button class="btn btn-secondary btn-sm btn-icon" onclick="openModel(\''+escAttr(m.name)+'\')" title="Edit model"><svg viewBox="0 0 16 16" width="13" height="13" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><path d="M11.3 2.2l2.5 2.5L5.5 13H3v-2.5z"/><path d="M9.8 3.7l2.5 2.5"/></svg></button><button class="btn btn-danger btn-sm btn-icon" onclick="deleteModel(\''+escAttr(m.name)+'\')" title="Delete model"><svg viewBox="0 0 16 16" width="13" height="13" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"><path d="M2.5 4h11M5.5 4V2.5h5V4M4 4l.7 9.5h6.6L12 4M6.5 6.8v4.4M9.5 6.8v4.4"/></svg></button></div></td></tr>';}).join('');reapplyFilter('modelFilter','modelsBody');}
function renderAliases(){
  var unknown=document.getElementById('unknownModelsBody');
  if(unknown){unknown.innerHTML=chatState.unknownModels.length?chatState.unknownModels.map(function(u,i){
    var options=chatState.models.map(function(m){return '<option value="'+escAttr(m.name)+'">'+esc(m.name)+'</option>';}).join('');
    var seen=u.last_seen?new Date(u.last_seen).toLocaleString():'\u2014';
    return '<tr><td><code>'+esc(u.id)+'</code></td><td class="mono">'+u.count+'</td><td>'+esc(seen)+'</td><td class="mono">'+esc(u.endpoint)+'</td><td><div class="action-group"><select id="unknownTarget-'+i+'">'+options+'</select><button class="btn btn-primary btn-sm" type="button" onclick="aliasUnknown('+i+')" '+(options?'':'disabled')+'>Alias</button></div></td></tr>';
  }).join(''):'<tr><td colspan="5" class="empty-cell">No unrecognized model requests since restart</td></tr>';}
  var aliases=document.getElementById('aliasesBody');
  if(aliases){aliases.innerHTML=chatState.aliases.length?chatState.aliases.map(function(a,i){return '<tr><td><code>'+esc(a.alias)+'</code></td><td><code>'+esc(a.target)+'</code></td><td class="row-actions"><button class="btn btn-danger btn-sm" type="button" onclick="deleteAlias('+i+')">Delete</button></td></tr>';}).join(''):'<tr><td colspan="3" class="empty-cell">No aliases configured</td></tr>';}
}
function aliasUnknown(i){var u=chatState.unknownModels[i],sel=document.getElementById('unknownTarget-'+i);if(!u||!sel||!sel.value)return;apiPost('/admin/models/mutate',{action:'alias_add',alias:u.id,target:sel.value}).then(function(r){if(!r.ok){flash(r.json.error&&r.json.error.message||'Alias failed','error');return;}flash('Alias created','success');loadModels();});}
function deleteAlias(i){var a=chatState.aliases[i];if(!a)return;openConfirm('Delete alias','Delete alias <strong>'+esc(a.alias)+'</strong>?','','Delete alias',function(){apiPost('/admin/models/mutate',{action:'alias_delete',alias:a.alias}).then(function(r){if(!r.ok){flash(r.json.error&&r.json.error.message||'Delete failed','error');return;}flash('Alias deleted','success');loadModels();});});}
function renderHelpers(){
  chatState.visionList=(chatState.helpers.vision_models||[]).slice();
  chatState.searchList=(chatState.helpers.web_search_keys||[]).map(function(e){return {provider:e.provider,keyMask:e.key_mask,keepIndex:0,newKey:null};});
  chatState.searchList.forEach(function(e,i){e.keepIndex=i;});
  renderVisionCascade();
  renderSearchCascade();
}
function renderVisionCascade(){
  var el=document.getElementById('visionCascade');
  var list=chatState.visionList;
  el.innerHTML=list.length?list.map(function(name,i){
    return '<div class="cascade-row"><span class="cascade-rank">'+(i+1)+'</span>'+
      '<span class="cascade-label">'+esc(name)+'</span>'+
      '<span class="cascade-actions">'+
      (i>0?'<button type="button" class="btn-link" title="Move up" onclick="moveVision('+i+',-1)">&uarr;</button>':'<span class="cascade-spacer"></span>')+
      (i<list.length-1?'<button type="button" class="btn-link" title="Move down" onclick="moveVision('+i+',1)">&darr;</button>':'<span class="cascade-spacer"></span>')+
      '<button type="button" class="btn-link" style="color:var(--red)" title="Remove" onclick="removeVision('+i+')">&times;</button>'+
      '</span></div>';
  }).join(''):'<p class="hint">No vision models configured — images sent to non-vision models will be rejected.</p>';
  var sel=document.getElementById('visionAddSelect');
  var avail=chatState.models.filter(function(m){return m.supports_vision&&list.indexOf(m.name)<0;});
  sel.innerHTML=avail.length?avail.map(function(m){return '<option value="'+escAttr(m.name)+'">'+esc(m.name)+'</option>';}).join(''):'<option value="">No vision-capable models available</option>';
}
function addVisionModel(){
  var sel=document.getElementById('visionAddSelect');
  if(!sel.value)return;
  chatState.visionList.push(sel.value);
  renderVisionCascade();
}
function moveVision(i,dir){
  var list=chatState.visionList,j=i+dir;
  if(j<0||j>=list.length)return;
  var t=list[i];list[i]=list[j];list[j]=t;
  renderVisionCascade();
}
function removeVision(i){ chatState.visionList.splice(i,1); renderVisionCascade(); }

function renderSearchCascade(){
  var el=document.getElementById('searchCascade');
  var list=chatState.searchList;
  el.innerHTML=list.length?list.map(function(e,i){
    return '<div class="cascade-row"><span class="cascade-rank">'+(i+1)+'</span>'+
      '<span class="tag" style="text-transform:capitalize">'+esc(e.provider)+'</span>'+
      '<code class="cascade-label">'+esc(e.newKey?maskNewKey(e.newKey):e.keyMask)+'</code>'+
      '<span class="cascade-actions">'+
      (i>0?'<button type="button" class="btn-link" title="Move up" onclick="moveSearch('+i+',-1)">&uarr;</button>':'<span class="cascade-spacer"></span>')+
      (i<list.length-1?'<button type="button" class="btn-link" title="Move down" onclick="moveSearch('+i+',1)">&darr;</button>':'<span class="cascade-spacer"></span>')+
      '<button type="button" class="btn-link" title="Replace key" onclick="rotateSearch('+i+')">Change</button>'+
      '<button type="button" class="btn-link" style="color:var(--red)" title="Remove" onclick="removeSearch('+i+')">&times;</button>'+
      '</span></div>';
  }).join(''):'<p class="hint">No web search providers configured — search tools are disabled.</p>';
}
function maskNewKey(k){ return k.length>9?k.slice(0,7)+'...'+k.slice(-2):k; }
function addSearchKey(){
  var provider=document.getElementById('searchAddProvider').value;
  var key=document.getElementById('searchAddKey').value.trim();
  if(!key){flash('Enter an API key','error');return;}
  chatState.searchList.push({provider:provider,keyMask:'',keepIndex:null,newKey:key});
  document.getElementById('searchAddKey').value='';
  renderSearchCascade();
}
function moveSearch(i,dir){
  var list=chatState.searchList,j=i+dir;
  if(j<0||j>=list.length)return;
  var t=list[i];list[i]=list[j];list[j]=t;
  renderSearchCascade();
}
function removeSearch(i){ chatState.searchList.splice(i,1); renderSearchCascade(); }
function rotateSearch(i){
  var k=prompt('New API key for this entry:');
  if(!k)return;
  chatState.searchList[i].newKey=k.trim();
  renderSearchCascade();
}
function saveHelpers(){
  var body={
    audio:chatState.helpers.legacy_audio||'',ocr:chatState.helpers.legacy_ocr||'',
    vision_models:chatState.visionList,
    web_search_keys:chatState.searchList.map(function(e){return {provider:e.provider,key:e.newKey||null,keep_index:e.newKey?null:e.keepIndex};})
  };
  apiPost('/admin/processors/mutate',body).then(function(r){
    if(!r.ok){flash(r.json.error&&r.json.error.message||'Save failed','error');return;}
    flash('Model helpers saved','success');loadModels();
  });
}
function openModel(name){var m=name?chatState.models.find(function(x){return x.name===name;}):null;chatState.editing=name;var f=document.getElementById('modelForm');f.reset();document.getElementById('modelTitle').textContent=m?name:'Add model';document.getElementById('backendRows').innerHTML='';if(m){['name','type','model','timeout','context_window','region','aws_access_key'].forEach(function(k){if(f.elements[k])f.elements[k].value=m[k]||'';});f.elements.supports_vision.checked=!!m.supports_vision;f.elements.rewrite_vision.value=m.rewrite_vision||(m.force_pipeline?'on':'');f.elements.rewrite_documents.value=m.rewrite_documents||(m.force_pipeline?'on':'');f.elements.rewrite_web_search.value=m.rewrite_web_search||(m.force_pipeline?'on':'');var d=m.defaults||{};['temperature','top_p','top_k','reasoning_effort'].forEach(function(k){f.elements[k].value=d[k]==null?'':d[k];});(m.backends||[]).forEach(addBackend);}else{f.elements.timeout.value=300;addBackend(null);}toggleBedrock();updateAutoHints();document.getElementById('modelErr').style.display='none';document.getElementById('modelModal').classList.add('open');}
function closeModel(){document.getElementById('modelModal').classList.remove('open');chatState.editing=null;}
function toggleBedrock(){document.getElementById('bedrockFields').style.display=document.getElementById('modelForm').elements.type.value==='bedrock'?'':'none';}
function updateAutoHints(){
  var visionAuto=document.getElementById('modelForm').elements.supports_vision.checked?'off':'on';
  ['rewriteVisionSelect','rewriteDocumentsSelect'].forEach(function(id){var sel=document.getElementById(id);if(sel&&sel.options[0])sel.options[0].textContent='Auto ('+visionAuto+')';});
  var searchSel=document.getElementById('rewriteSearchSelect');
  if(searchSel&&searchSel.options[0]){var searchAuto=(chatState.helpers&&chatState.helpers.has_search_key)?'on':'off';searchSel.options[0].textContent='Auto ('+searchAuto+')';}
}
function addBackend(b){b=b||{url:'',weight:1,max_inflight:0,disabled:false};var row=document.createElement('div');row.className='be-row-wrap';row.dataset.originalUrl=b.url||'';row.dataset.hasKey=b.has_api_key?'1':'0';row.dataset.clearKey='0';
var dotTitle=b.url?(b.disabled?'disabled':(b.online===true?('online \u00b7 '+(b.inflight||0)+' in-flight \u00b7 breaker '+(b.breaker||'closed')):(b.online===false?('offline: '+(b.error||'')):'no health data'))):'new backend';
var dotCls=b.disabled?'health-unknown':(b.online===true?'health-online':(b.online===false?'health-offline':'health-unknown'));
row.innerHTML='<div class="be-row">'+
 '<span><span class="health-dot '+dotCls+'" title="'+escAttr(dotTitle)+'"></span></span>'+
 '<input class="be-url" type="url" placeholder="http://server:8000/v1" value="'+escAttr(b.url||'')+'">'+
 '<span class="be-keywrap"><input class="be-key" type="password" placeholder="'+escAttr(b.has_api_key?(b.api_key_mask+' \u2014 blank keeps'):'optional')+'"><button type="button" class="copy-icon be-clearkey" title="Clear the stored key on save">&times;</button></span>'+
 '<input class="be-weight" type="number" min="1" value="'+(b.weight||1)+'" title="Load-balancing weight">'+
 '<input class="be-max" type="number" min="0" value="'+(b.max_inflight||0)+'" title="Max concurrent (0 = unlimited)">'+
 '<span style="text-align:center"><input class="be-disabled" type="checkbox" '+(b.disabled?'checked':'')+' title="Temporarily disabled"></span>'+
 '<span class="be-actions"><button type="button" class="btn btn-secondary btn-sm be-test">Test</button><button type="button" class="btn btn-danger btn-sm btn-icon be-remove" title="Remove backend"><svg viewBox="0 0 16 16" width="13" height="13" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"><path d="M2.5 4h11M5.5 4V2.5h5V4M4 4l.7 9.5h6.6L12 4M6.5 6.8v4.4M9.5 6.8v4.4"/></svg></button></span>'+
 '</div>'+
 '<div class="be-note"><span class="be-status mono"'+((b.online===false&&b.error)?' style="color:var(--red)"':'')+'>'+((b.online===false&&b.error)?esc(b.error):'')+'</span></div>';
if(!(b.online===false&&b.error))row.querySelector('.be-note').style.display='none';
row.querySelector('.be-remove').onclick=function(){row.remove();};
row.querySelector('.be-key').oninput=function(){row.dataset.clearKey='0';};
row.querySelector('.be-clearkey').onclick=function(){var i=row.querySelector('.be-key');i.value='';i.placeholder='will be cleared on save';row.dataset.clearKey='1';};
row.querySelector('.be-test').onclick=function(){testBackend(row);};
document.getElementById('backendRows').appendChild(row);}
function testBackend(row){var u=row.querySelector('.be-url').value.trim(),key=row.querySelector('.be-key').value,s=row.querySelector('.be-status');row.querySelector('.be-note').style.display='';if(!u){s.textContent='Enter a URL';return;}s.textContent='Testing\u2026';var body={url:u,model:chatState.editing||'',upstream_model:(document.getElementById('modelForm').elements.model.value.trim()||document.getElementById('modelForm').elements.name.value.trim()),type:document.getElementById('modelForm').elements.type.value};if(key)body.api_key=key;apiPost('/admin/backends/probe',body).then(function(r){
  if(!r.ok){s.innerHTML='';s.textContent=(r.json.error&&r.json.error.message)||'Failed';s.style.color='var(--danger)';return;}
  var txt=r.json.engine?'Connected \u00b7 '+r.json.engine:'Connected';
  if(r.json.context_window){
    txt+=' \u00b7 context '+Number(r.json.context_window).toLocaleString();
    s.innerHTML=esc(txt)+' <button type="button" class="btn-link" onclick="applyContextWindow('+r.json.context_window+')">Use</button>';
  } else {
    s.textContent=txt;
  }
  s.style.color='var(--success)';
});}
function applyContextWindow(n){ document.getElementById('modelForm').elements.context_window.value=n; flash('Context window set to '+Number(n).toLocaleString(),'success'); }
function detectContextWindow(btn){
  var rows=Array.from(document.querySelectorAll('#backendRows .be-row-wrap'));
  var row=rows.find(function(r){return r.querySelector('.be-url').value.trim();});
  var status=document.getElementById('ctxDetectStatus');
  status.style.display='';
  if(!row){status.textContent='Add a backend URL first.';status.style.color='var(--red)';return;}
  var u=row.querySelector('.be-url').value.trim(),key=row.querySelector('.be-key').value;
  var body={url:u,model:chatState.editing||'',upstream_model:(document.getElementById('modelForm').elements.model.value.trim()||document.getElementById('modelForm').elements.name.value.trim()),type:document.getElementById('modelForm').elements.type.value};
  if(key)body.api_key=key;
  btn.disabled=true;status.textContent='Checking '+u+'\u2026';status.style.color='var(--steel)';
  apiPost('/admin/backends/probe',body).then(function(r){
    btn.disabled=false;
    if(!r.ok){status.textContent=(r.json.error&&r.json.error.message)||'Probe failed';status.style.color='var(--red)';return;}
    if(r.json.context_window){
      document.getElementById('modelForm').elements.context_window.value=r.json.context_window;
      status.textContent='Detected '+Number(r.json.context_window).toLocaleString()+' from '+u;
      status.style.color='var(--ok)';
    } else {
      status.textContent='This backend\u2019s API does not publish a context window \u2014 enter it manually from the provider\u2019s docs.';
      status.style.color='var(--steel)';
    }
  });
}
function collectBackends(){return Array.from(document.querySelectorAll('#backendRows .be-row-wrap')).map(function(r){var b={url:r.querySelector('.be-url').value.trim(),weight:parseInt(r.querySelector('.be-weight').value,10)||1,max_inflight:parseInt(r.querySelector('.be-max').value,10)||0,disabled:r.querySelector('.be-disabled').checked};var k=r.querySelector('.be-key').value;if(k)b.api_key=k;else if(r.dataset.clearKey==='1')b.api_key='';return b;});}
function saveModel(e){e.preventDefault();var f=e.target,backends=collectBackends(),err=document.getElementById('modelErr');err.style.display='none';if(!f.elements.name.value.trim()||!backends.length||backends.some(function(b){return !b.url;})){err.textContent='Name and at least one backend URL are required.';err.style.display='block';return;}var body={name:f.elements.name.value.trim(),type:f.elements.type.value,model:f.elements.model.value.trim(),timeout:parseInt(f.elements.timeout.value,10)||300,context_window:parseInt(f.elements.context_window.value,10)||0,supports_vision:f.elements.supports_vision.checked,rewrite_vision:f.elements.rewrite_vision.value,rewrite_documents:f.elements.rewrite_documents.value,rewrite_web_search:f.elements.rewrite_web_search.value,region:f.elements.region.value.trim(),aws_access_key:f.elements.aws_access_key.value.trim(),backends:backends};var d={};['temperature','top_p'].forEach(function(k){if(f.elements[k].value!=='')d[k]=parseFloat(f.elements[k].value);});if(f.elements.top_k.value!=='')d.top_k=parseInt(f.elements.top_k.value,10);if(f.elements.reasoning_effort.value)d.reasoning_effort=f.elements.reasoning_effort.value;body.defaults=d;if(f.elements.aws_secret_key.value)body.aws_secret_key=f.elements.aws_secret_key.value;if(f.elements.aws_session_token.value)body.aws_session_token=f.elements.aws_session_token.value;var payload=chatState.editing?{action:'update',original_name:chatState.editing,model:body}:{action:'add',model:body};var btn=document.getElementById('modelSave');btn.disabled=true;apiPost('/admin/models/mutate',payload).then(function(r){btn.disabled=false;if(!r.ok){err.textContent=r.json.error&&r.json.error.message||'Save failed';err.style.display='block';return;}closeModel();flash('Model saved','success');loadModels();});}
function deleteModel(name){openConfirm('Delete model','Delete model <strong>'+esc(name)+'</strong>? Its backends are removed from the running config immediately.','','Delete model',function(){apiPost('/admin/models/mutate',{action:'delete',name:name}).then(function(r){if(!r.ok){flash(r.json.error&&r.json.error.message||'Delete failed','error');return;}flash('Deleted','success');loadModels();});});}
function switchLLMTab(t){
var h=t==='helpers',a=t==='aliasing',m=!h&&!a;
document.getElementById('pane-models').style.display=m?'':'none';
document.getElementById('pane-helpers').style.display=h?'':'none';
document.getElementById('pane-aliasing').style.display=a?'':'none';
document.getElementById('subtabModels').classList.toggle('active',m);
document.getElementById('subtabHelpers').classList.toggle('active',h);
document.getElementById('subtabAliasing').classList.toggle('active',a);
if(history.replaceState)history.replaceState(null,'',h?'?tab=helpers':(a?'?tab=aliasing':location.pathname));
}
var initialLLMTab=new URLSearchParams(location.search).get('tab');
if(initialLLMTab==='helpers'||initialLLMTab==='aliasing')switchLLMTab(initialLLMTab);
document.addEventListener('keydown',function(e){if(e.key==='Escape')closeModel();});attachFilter('modelFilter','modelsBody');loadModels();`
}
