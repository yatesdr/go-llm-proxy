package handler

import (
	"log/slog"
	"net/http"
	"net/url"

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
	body := `<div class="toolbar">
  <div><h2>Chat</h2><p class="helper-text">Each model owns its backend servers. Add another backend to add capacity; requests are balanced automatically.</p></div>
  <button class="btn btn-primary btn-sm" type="button" onclick="openModel(null)">+ Add Chat Model</button>
</div>
<div class="card">
  <div class="table-wrap"><table class="data-table">
    <thead><tr><th>Name</th><th>Protocol</th><th>Upstream model</th><th>Backends</th><th>Vision</th><th>Health</th><th style="text-align:right">Actions</th></tr></thead>
    <tbody id="modelsBody"><tr><td colspan="7" class="empty-cell">Loading…</td></tr></tbody>
  </table></div>
</div>
<div class="card" style="margin-top:16px">
  <h3 style="margin-top:0">Chat helpers</h3>
  <p class="helper-text">Choose a vision-capable chat model for image descriptions. Web search is optional.</p>
  <form id="helpersForm" onsubmit="saveHelpers(event)">
    <div class="field-grid">
      <div class="field"><label>Vision fallback</label><select id="helperVision"><option value="">Disabled</option></select></div>
      <div class="field"><label>Web search API key</label><div class="secret-row"><span id="searchMask" class="mono">—</span><button type="button" class="btn btn-secondary btn-sm" onclick="editSearchKey()">Change</button><button type="button" class="btn btn-danger btn-sm" onclick="clearSearchKey()">Clear</button></div><input id="searchKey" type="password" style="display:none;margin-top:6px" placeholder="Tavily or Brave key"></div>
    </div>
    <div style="margin-top:12px"><button class="btn btn-primary btn-sm" type="submit">Save helpers</button></div>
  </form>
</div>` + modelModalHTML()
	h.renderShell(w, "chat", "Admin · Chat", body, modelsPageJS())
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
			"context_window": m.ContextWindow, "supports_vision": m.SupportsVision,
			"force_pipeline": m.ForcePipeline, "responses_mode": m.ResponsesMode,
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
	writeJSON(w, http.StatusOK, map[string]any{
		"models": models, "regions": awsRegions,
		"helpers": map[string]any{
			"vision": cfg.Processors.Vision, "has_search_key": cfg.Processors.WebSearchKey != "",
			"search_key_mask": config.MaskSecret(cfg.Processors.WebSearchKey),
			"legacy_audio":    cfg.Processors.Audio, "legacy_ocr": cfg.Processors.OCR,
		},
	})
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
	default:
		httputil.WriteError(w, http.StatusBadRequest, "unknown action")
	}
}

// BackendsProbe validates a candidate backend without saving it. If the key
// is omitted, it is recovered from the matching configured model/backend.
func (h *AdminHandler) BackendsProbe(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL, Model, Type string
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
	writeJSON(w, http.StatusOK, config.ProbeBackend(req.URL, key, req.Type))
}

type modelInputDTO struct {
	Name             string              `json:"name"`
	Backend          string              `json:"backend"` // legacy admin client
	Type             string              `json:"type"`
	Model            string              `json:"model"`
	ResponsesMode    string              `json:"responses_mode"`
	MessagesMode     string              `json:"messages_mode"`
	Timeout          int                 `json:"timeout"`
	ContextWindow    int                 `json:"context_window"`
	SupportsVision   bool                `json:"supports_vision"`
	SupportsAudio    *bool               `json:"supports_audio"` // legacy capability; hidden in the new UI
	ForcePipeline    bool                `json:"force_pipeline"`
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
		ForcePipeline: d.ForcePipeline, ResponsesMode: d.ResponsesMode, MessagesMode: d.MessagesMode,
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
    <div class="modal-header"><h2 id="modelTitle">Chat model</h2><button class="modal-close" onclick="closeModel()" type="button">&times;</button></div>
    <form id="modelForm" onsubmit="saveModel(event)"><div class="modal-body">
      <div class="section section-required"><h3>Model</h3><div class="field-grid">
        <div class="field"><label>Client-facing name</label><input name="name" required placeholder="GLM-5.2"></div>
        <div class="field"><label>Protocol</label><select name="type" onchange="toggleBedrock()"><option value="">OpenAI</option><option value="anthropic">Anthropic</option><option value="bedrock">AWS Bedrock</option></select></div>
        <div class="field"><label>Upstream model ID</label><input name="model" placeholder="same as client-facing name"></div>
        <div class="field"><label>Timeout (seconds)</label><input name="timeout" type="number" min="1" placeholder="300"></div>
        <div class="field"><label>Context window</label><input name="context_window" type="number" min="0" placeholder="auto-detect"></div>
        <div class="field checkbox-row"><input id="supportsVision" name="supports_vision" type="checkbox"><label for="supportsVision">Accepts images directly</label></div>
      </div></div>
      <div class="section"><div class="toolbar"><div><h3>Backends</h3><p class="helper-text">All servers here must serve the same upstream model.</p></div><button type="button" class="btn btn-secondary btn-sm" onclick="addBackend(null)">+ Add Backend</button></div><div id="backendRows"></div></div>
      <details class="section"><summary><strong>Advanced model settings</strong></summary><div class="field-grid" style="margin-top:12px">
        <div class="field"><label>Responses API</label><select name="responses_mode"><option value="">Auto</option><option value="native">Native</option><option value="translate">Translate</option></select></div>
        <div class="field"><label>Anthropic Messages</label><select name="messages_mode"><option value="">Auto</option><option value="native">Native</option><option value="translate">Translate</option></select></div>
        <div class="field checkbox-row"><input id="forcePipeline" name="force_pipeline" type="checkbox"><label for="forcePipeline">Always run helper pipeline</label></div>
        <div class="field"><label>Temperature default</label><input name="temperature" type="number" step="0.05" min="0" max="2"></div>
        <div class="field"><label>Top-p default</label><input name="top_p" type="number" step="0.05" min="0" max="1"></div>
        <div class="field"><label>Top-k default</label><input name="top_k" type="number" min="0"></div>
        <div class="field"><label>Reasoning effort default</label><select name="reasoning_effort"><option value="">Backend default</option><option>low</option><option>medium</option><option>high</option></select></div>
      </div></details>
      <div id="bedrockFields" class="section" style="display:none"><h3>Bedrock</h3><div class="field-grid">
        <div class="field"><label>Region</label><input name="region" placeholder="us-east-1"></div><div class="field"><label>AWS access key ID</label><input name="aws_access_key"></div>
        <div class="field"><label>AWS secret key</label><input name="aws_secret_key" type="password" placeholder="leave blank to keep"></div><div class="field"><label>Session token</label><input name="aws_session_token" type="password" placeholder="leave blank to keep"></div>
      </div></div>
      <div id="modelErr" class="inline-err" style="display:none"></div>
    </div><div class="modal-footer"><button type="button" class="btn btn-secondary" onclick="closeModel()">Cancel</button><button id="modelSave" class="btn btn-primary">Save model</button></div></form>
  </div>
</div>`
}

func modelsPageJS() string {
	return `
var chatState={models:[],editing:null,helpers:{},searchAction:null};
function loadModels(){apiGet('/admin/models/data').then(function(d){chatState.models=d.models||[];chatState.helpers=d.helpers||{};renderModels();renderHelpers();}).catch(function(e){flash('Load failed: '+e.message,'error');});}
function renderModels(){var el=document.getElementById('modelsBody');if(!chatState.models.length){el.innerHTML='<tr><td colspan="7" class="empty-cell">No chat models configured</td></tr>';return;}el.innerHTML=chatState.models.map(function(m){var active=(m.backends||[]).filter(function(b){return !b.disabled;}).length;var h=m.health||{};var hs=h.online?'<span class="health-dot health-online"></span>online':(h.error?'<span class="health-dot health-offline"></span>offline':'—');return '<tr><td><strong>'+esc(m.name)+'</strong></td><td><code>'+esc(m.type||'openai')+'</code></td><td class="mono">'+esc(m.model||m.name)+'</td><td>'+active+' active / '+(m.backends||[]).length+'</td><td>'+(m.supports_vision?'Yes':'—')+'</td><td>'+hs+'</td><td class="row-actions"><div class="action-group"><button class="btn btn-secondary btn-sm" onclick="openModel(\''+escAttr(m.name)+'\')">Edit</button><button class="btn btn-danger btn-sm" onclick="deleteModel(\''+escAttr(m.name)+'\')">Delete</button></div></td></tr>';}).join('');}
function renderHelpers(){var s=document.getElementById('helperVision');s.innerHTML='<option value="">Disabled</option>'+chatState.models.filter(function(m){return m.supports_vision;}).map(function(m){return '<option value="'+escAttr(m.name)+'">'+esc(m.name)+'</option>';}).join('');s.value=chatState.helpers.vision||'';document.getElementById('searchMask').textContent=chatState.helpers.has_search_key?(chatState.helpers.search_key_mask||'(set)'):'(not set)';}
function editSearchKey(){chatState.searchAction='set';var i=document.getElementById('searchKey');i.style.display='block';i.focus();}
function clearSearchKey(){chatState.searchAction='clear';document.getElementById('searchMask').textContent='(will clear on save)';document.getElementById('searchKey').style.display='none';}
function saveHelpers(e){e.preventDefault();var body={vision:document.getElementById('helperVision').value,audio:chatState.helpers.legacy_audio||'',ocr:chatState.helpers.legacy_ocr||''};if(chatState.searchAction==='clear')body.web_search_key='';if(chatState.searchAction==='set'&&document.getElementById('searchKey').value)body.web_search_key=document.getElementById('searchKey').value;apiPost('/admin/processors/mutate',body).then(function(r){if(!r.ok){flash(r.json.error&&r.json.error.message||'Save failed','error');return;}chatState.searchAction=null;flash('Chat helpers saved','success');loadModels();});}
function openModel(name){var m=name?chatState.models.find(function(x){return x.name===name;}):null;chatState.editing=name;var f=document.getElementById('modelForm');f.reset();document.getElementById('modelTitle').textContent=m?'Edit '+name:'Add chat model';document.getElementById('backendRows').innerHTML='';if(m){['name','type','model','timeout','context_window','responses_mode','messages_mode','region','aws_access_key'].forEach(function(k){if(f.elements[k])f.elements[k].value=m[k]||'';});f.elements.supports_vision.checked=!!m.supports_vision;f.elements.force_pipeline.checked=!!m.force_pipeline;var d=m.defaults||{};['temperature','top_p','top_k','reasoning_effort'].forEach(function(k){f.elements[k].value=d[k]==null?'':d[k];});(m.backends||[]).forEach(addBackend);}else{f.elements.timeout.value=300;addBackend(null);}toggleBedrock();document.getElementById('modelErr').style.display='none';document.getElementById('modelModal').classList.add('open');}
function closeModel(){document.getElementById('modelModal').classList.remove('open');chatState.editing=null;}
function toggleBedrock(){document.getElementById('bedrockFields').style.display=document.getElementById('modelForm').elements.type.value==='bedrock'?'':'none';}
function addBackend(b){b=b||{url:'',weight:1,max_inflight:0,disabled:false};var row=document.createElement('div');row.className='card backend-row';row.style.marginBottom='10px';row.innerHTML='<div class="field-grid"><div class="field field-full"><label>Server URL</label><input class="be-url" type="url" placeholder="http://server:8000/v1" value="'+escAttr(b.url||'')+'"></div><div class="field"><label>API key</label><div class="secret-row"><input class="be-key" type="password" placeholder="'+escAttr(b.has_api_key?(b.api_key_mask+' — leave blank to keep'):'optional')+'"><button type="button" class="btn btn-danger btn-sm" onclick="clearBackendKey(this)">Clear</button></div></div><div class="field"><label>Weight</label><input class="be-weight" type="number" min="1" value="'+(b.weight||1)+'"></div><div class="field"><label>Max concurrent (0 = unlimited)</label><input class="be-max" type="number" min="0" value="'+(b.max_inflight||0)+'"></div><div class="field checkbox-row"><input class="be-disabled" type="checkbox" '+(b.disabled?'checked':'')+'><label>Temporarily disabled</label></div></div><div style="display:flex;gap:8px;align-items:center;margin-top:8px"><button type="button" class="btn btn-secondary btn-sm be-test">Test</button><button type="button" class="btn btn-danger btn-sm be-remove">Remove</button><span class="be-status mono"></span></div>';row.dataset.originalUrl=b.url||'';row.dataset.hasKey=b.has_api_key?'1':'0';row.dataset.clearKey='0';row.querySelector('.be-remove').onclick=function(){row.remove();};row.querySelector('.be-key').oninput=function(){row.dataset.clearKey='0';};row.querySelector('.be-test').onclick=function(){testBackend(row);};document.getElementById('backendRows').appendChild(row);}
function clearBackendKey(btn){var row=btn.closest('.backend-row'),input=row.querySelector('.be-key');input.value='';input.placeholder='will be cleared on save';row.dataset.clearKey='1';}
function testBackend(row){var u=row.querySelector('.be-url').value.trim(),key=row.querySelector('.be-key').value,s=row.querySelector('.be-status');if(!u){s.textContent='Enter a URL';return;}s.textContent='Testing…';var body={url:u,model:chatState.editing||'',type:document.getElementById('modelForm').elements.type.value};if(key)body.api_key=key;apiPost('/admin/backends/probe',body).then(function(r){s.textContent=r.ok?(r.json.engine?'Connected · '+r.json.engine:'Connected'):(r.json.error&&r.json.error.message||'Failed');s.style.color=r.ok?'var(--success)':'var(--danger)';});}
function collectBackends(){return Array.from(document.querySelectorAll('#backendRows .backend-row')).map(function(r){var b={url:r.querySelector('.be-url').value.trim(),weight:parseInt(r.querySelector('.be-weight').value,10)||1,max_inflight:parseInt(r.querySelector('.be-max').value,10)||0,disabled:r.querySelector('.be-disabled').checked};var k=r.querySelector('.be-key').value;if(k)b.api_key=k;else if(r.dataset.clearKey==='1')b.api_key='';return b;});}
function saveModel(e){e.preventDefault();var f=e.target,backends=collectBackends(),err=document.getElementById('modelErr');err.style.display='none';if(!f.elements.name.value.trim()||!backends.length||backends.some(function(b){return !b.url;})){err.textContent='Name and at least one backend URL are required.';err.style.display='block';return;}var body={name:f.elements.name.value.trim(),type:f.elements.type.value,model:f.elements.model.value.trim(),timeout:parseInt(f.elements.timeout.value,10)||300,context_window:parseInt(f.elements.context_window.value,10)||0,supports_vision:f.elements.supports_vision.checked,force_pipeline:f.elements.force_pipeline.checked,responses_mode:f.elements.responses_mode.value,messages_mode:f.elements.messages_mode.value,region:f.elements.region.value.trim(),aws_access_key:f.elements.aws_access_key.value.trim(),backends:backends};var d={};['temperature','top_p'].forEach(function(k){if(f.elements[k].value!=='')d[k]=parseFloat(f.elements[k].value);});if(f.elements.top_k.value!=='')d.top_k=parseInt(f.elements.top_k.value,10);if(f.elements.reasoning_effort.value)d.reasoning_effort=f.elements.reasoning_effort.value;body.defaults=d;if(f.elements.aws_secret_key.value)body.aws_secret_key=f.elements.aws_secret_key.value;if(f.elements.aws_session_token.value)body.aws_session_token=f.elements.aws_session_token.value;var payload=chatState.editing?{action:'update',original_name:chatState.editing,model:body}:{action:'add',model:body};var btn=document.getElementById('modelSave');btn.disabled=true;apiPost('/admin/models/mutate',payload).then(function(r){btn.disabled=false;if(!r.ok){err.textContent=r.json.error&&r.json.error.message||'Save failed';err.style.display='block';return;}closeModel();flash('Chat model saved','success');loadModels();});}
function deleteModel(name){if(!confirm('Delete chat model "'+name+'"?'))return;apiPost('/admin/models/mutate',{action:'delete',name:name}).then(function(r){if(!r.ok){flash(r.json.error&&r.json.error.message||'Delete failed','error');return;}flash('Deleted','success');loadModels();});}
document.addEventListener('keydown',function(e){if(e.key==='Escape')closeModel();});loadModels();`
}
