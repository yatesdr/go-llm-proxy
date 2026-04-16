package handler

import (
	"log/slog"
	"net/http"

	"go-llm-proxy/internal/config"
	"go-llm-proxy/internal/httputil"
)

// awsRegions lists the AWS regions where Bedrock is generally available.
// Used to seed the typeahead in the model modal; the user can enter a
// region not in this list and the server will accept it (validation is
// deferred to the backend).
var awsRegions = []string{
	"us-east-1", "us-east-2", "us-west-1", "us-west-2",
	"ca-central-1", "sa-east-1",
	"eu-west-1", "eu-west-2", "eu-west-3", "eu-central-1", "eu-north-1", "eu-south-1",
	"ap-south-1", "ap-northeast-1", "ap-northeast-2", "ap-northeast-3",
	"ap-southeast-1", "ap-southeast-2", "ap-southeast-4",
	"me-south-1", "me-central-1", "af-south-1",
}

// ModelsPage renders the /admin/models HTML page.
func (h *AdminHandler) ModelsPage(w http.ResponseWriter, r *http.Request) {
	body := `<div class="toolbar">
  <h2>Models</h2>
  <button class="btn btn-primary btn-sm" type="button" onclick="openModelModal(null)">+ Add Model</button>
</div>
<div class="card">
  <div class="table-wrap">
    <table class="data-table">
      <thead><tr>
        <th>Name</th><th>Type</th><th>Backend</th><th>Context</th><th style="text-align:center">Vision</th><th>Health</th><th style="width:140px;text-align:right">Actions</th>
      </tr></thead>
      <tbody id="modelsBody"><tr><td colspan="7" style="text-align:center;color:var(--muted)">Loading…</td></tr></tbody>
    </table>
  </div>
</div>` + modelModalHTML()
	h.renderShell(w, "models", "Admin · Models", body, modelsPageJS())
}

// ModelsData serves the JSON payload the /admin/models page fetches.
func (h *AdminHandler) ModelsData(w http.ResponseWriter, r *http.Request) {
	cfg := h.cs.Get()

	var healthMap map[string]config.ModelHealth
	if h.health != nil {
		healthMap = h.health.GetStatus()
	}

	models := make([]map[string]any, 0, len(cfg.Models))
	for _, m := range cfg.Models {
		entry := map[string]any{
			"name":              m.Name,
			"backend":           m.Backend,
			"type":              m.Type,
			"model":             m.Model,
			"timeout":           m.Timeout,
			"context_window":    m.ContextWindow,
			"supports_vision":   m.SupportsVision,
			"force_pipeline":    m.ForcePipeline,
			"responses_mode":    m.ResponsesMode,
			"messages_mode":     m.MessagesMode,
			"has_api_key":       m.APIKey != "",
			"api_key_masked":    config.MaskSecret(m.APIKey),
			"region":            m.Region,
			"aws_access_key":    m.AWSAccessKey,
			"has_aws_secret":    m.AWSSecretKey != "",
			"aws_secret_mask":   config.MaskSecret(m.AWSSecretKey),
			"has_aws_session":   m.AWSSessionToken != "",
			"guardrail_id":      m.GuardrailID,
			"guardrail_version": m.GuardrailVersion,
			"guardrail_trace":   m.GuardrailTrace,
		}
		if m.Defaults != nil {
			entry["defaults"] = samplingDefaultsToMap(m.Defaults)
		} else {
			entry["defaults"] = nil
		}
		if healthMap != nil {
			if hs, ok := healthMap[m.Name]; ok {
				entry["health"] = map[string]any{
					"online":   hs.Online,
					"external": hs.External,
					"error":    hs.Error,
				}
			}
		}
		models = append(models, entry)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"models":  models,
		"regions": awsRegions,
	})
}

// ModelsMutate handles POST /admin/models/mutate.
func (h *AdminHandler) ModelsMutate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action       string         `json:"action"`
		OriginalName string         `json:"original_name"`
		Name         string         `json:"name"`
		Force        bool           `json:"force"`
		Model        *modelInputDTO `json:"model"`
	}
	if err := decodeJSONBody(r, &req, 32*1024); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	switch req.Action {
	case "add":
		if req.Model == nil {
			httputil.WriteError(w, http.StatusBadRequest, "model is required")
			return
		}
		mc, err := req.Model.toConfig(h.cs.Get(), "")
		if err != nil {
			httputil.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := h.cs.AddModel(mc); err != nil {
			writeMutateError(w, err)
			return
		}
		slog.Info("admin: model added", "name", mc.Name, "type", mc.Type)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": mc.Name})

	case "update":
		if req.Model == nil {
			httputil.WriteError(w, http.StatusBadRequest, "model is required")
			return
		}
		if req.OriginalName == "" {
			httputil.WriteError(w, http.StatusBadRequest, "original_name is required")
			return
		}
		mc, err := req.Model.toConfig(h.cs.Get(), req.OriginalName)
		if err != nil {
			httputil.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := h.cs.UpdateModel(req.OriginalName, mc); err != nil {
			writeMutateError(w, err)
			return
		}
		slog.Info("admin: model updated", "original", req.OriginalName, "name", mc.Name)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": mc.Name})

	case "delete":
		if req.Name == "" {
			httputil.WriteError(w, http.StatusBadRequest, "name is required")
			return
		}
		if err := h.cs.DeleteModel(req.Name, req.Force); err != nil {
			writeMutateError(w, err)
			return
		}
		slog.Info("admin: model deleted", "name", req.Name, "force", req.Force)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})

	default:
		httputil.WriteError(w, http.StatusBadRequest, "unknown action")
	}
}

// modelInputDTO carries model-edit input from the client. Secret fields are
// pointer-typed so we can distinguish "omitted" (keep current value) from
// "explicit empty" (clear the value) when updating.
type modelInputDTO struct {
	Name             string              `json:"name"`
	Backend          string              `json:"backend"`
	Type             string              `json:"type"`
	Model            string              `json:"model"`
	Timeout          int                 `json:"timeout"`
	ContextWindow    int                 `json:"context_window"`
	SupportsVision   bool                `json:"supports_vision"`
	ForcePipeline    bool                `json:"force_pipeline"`
	ResponsesMode    string              `json:"responses_mode"`
	MessagesMode     string              `json:"messages_mode"`
	APIKey           *string             `json:"api_key"`
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
	Vision       string `json:"vision"`
	OCR          string `json:"ocr"`
	WebSearchKey string `json:"web_search_key"`
}

// toConfig translates the DTO into a ModelConfig suitable for passing to
// AddModel / UpdateModel. Secret fields that are nil (omitted) are copied
// from the existing model (found by originalName, or by the DTO's own Name
// if originalName is empty, i.e. the add case).
func (d *modelInputDTO) toConfig(cfg *config.Config, originalName string) (config.ModelConfig, error) {
	mc := config.ModelConfig{
		Name:             d.Name,
		Backend:          d.Backend,
		Type:             d.Type,
		Model:            d.Model,
		Timeout:          d.Timeout,
		ContextWindow:    d.ContextWindow,
		SupportsVision:   d.SupportsVision,
		ForcePipeline:    d.ForcePipeline,
		ResponsesMode:    d.ResponsesMode,
		MessagesMode:     d.MessagesMode,
		Region:           d.Region,
		AWSAccessKey:     d.AWSAccessKey,
		GuardrailID:      d.GuardrailID,
		GuardrailVersion: d.GuardrailVersion,
		GuardrailTrace:   d.GuardrailTrace,
	}

	// Resolve secret fields, preserving existing values when omitted.
	var existing *config.ModelConfig
	if originalName != "" {
		existing = config.FindModel(cfg, originalName)
	}
	mc.APIKey = resolveSecret(d.APIKey, existing, func(m *config.ModelConfig) string { return m.APIKey })
	mc.AWSSecretKey = resolveSecret(d.AWSSecretKey, existing, func(m *config.ModelConfig) string { return m.AWSSecretKey })
	mc.AWSSessionToken = resolveSecret(d.AWSSession, existing, func(m *config.ModelConfig) string { return m.AWSSessionToken })

	if d.Defaults != nil {
		mc.Defaults = &config.SamplingDefaults{
			Temperature:      d.Defaults.Temperature,
			TopP:             d.Defaults.TopP,
			TopK:             d.Defaults.TopK,
			MaxNewTokens:     d.Defaults.MaxNewTokens,
			FrequencyPenalty: d.Defaults.FrequencyPenalty,
			PresencePenalty:  d.Defaults.PresencePenalty,
			ReasoningEffort:  d.Defaults.ReasoningEffort,
			Stop:             d.Defaults.Stop,
		}
	}
	if d.Processors != nil {
		mc.Processors = &config.ProcessorsConfig{
			Vision:       d.Processors.Vision,
			OCR:          d.Processors.OCR,
			WebSearchKey: d.Processors.WebSearchKey,
		}
	}
	return mc, nil
}

// resolveSecret returns the new value if set (even if empty string means
// "clear"), or the existing value when the field is nil (omitted = keep).
func resolveSecret(incoming *string, existing *config.ModelConfig, get func(*config.ModelConfig) string) string {
	if incoming != nil {
		return *incoming
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

// modelModalHTML returns the hidden modal shell that both Add and Edit use.
func modelModalHTML() string {
	return `<div id="modelModal" class="modal-backdrop" onclick="onBackdropClick(event)">
  <div class="modal" role="dialog" aria-labelledby="modelModalTitle">
    <div class="modal-header">
      <h2 id="modelModalTitle">Model</h2>
      <button class="modal-close" type="button" onclick="closeModelModal()" aria-label="Close">&times;</button>
    </div>
    <form id="modelForm" onsubmit="submitModel(event)">
      <div class="modal-body">

        <div class="section"><h3>Identity</h3>
          <div class="field-grid">
            <div class="field">
              <label>Name <span class="tip" tabindex="0" data-tip="Name clients use to address this model in requests.">?</span></label>
              <input type="text" name="name" required>
            </div>
            <div class="field">
              <label>Type <span class="tip" tabindex="0" data-tip="Backend protocol. ''/openai = OpenAI Chat Completions, anthropic = native Anthropic Messages, bedrock = AWS Bedrock Converse.">?</span></label>
              <select name="type" onchange="onTypeChange()">
                <option value="">openai (default)</option>
                <option value="openai">openai</option>
                <option value="anthropic">anthropic</option>
                <option value="bedrock">bedrock</option>
              </select>
            </div>
            <div class="field field-full">
              <label>Backend URL <span class="tip" tabindex="0" data-tip="Upstream server URL. For Bedrock it's derived from region unless you need a VPC endpoint.">?</span></label>
              <input type="url" name="backend" id="backendInput" placeholder="http://localhost:8000/v1">
            </div>
            <div class="field field-full">
              <label>Upstream model name <span class="tip" tabindex="0" data-tip="Model identifier sent to the backend. Defaults to the name above when empty. Useful when the backend expects a different ID.">?</span></label>
              <input type="text" name="model" placeholder="same as name">
            </div>
          </div>
        </div>

        <div class="section"><h3>Limits</h3>
          <div class="field-grid">
            <div class="field">
              <label>Timeout (seconds) <span class="tip" tabindex="0" data-tip="Request timeout. Default 300s.">?</span></label>
              <input type="number" name="timeout" min="0" step="1" placeholder="300">
            </div>
            <div class="field">
              <label>Context window (tokens) <span class="tip" tabindex="0" data-tip="Max context size. 0 = auto-detect from backend.">?</span></label>
              <input type="number" name="context_window" min="0" step="1" placeholder="auto">
            </div>
            <div class="field checkbox-row">
              <input type="checkbox" id="supports_vision" name="supports_vision">
              <label for="supports_vision">Supports vision <span class="tip" tabindex="0" data-tip="Model can accept image inputs natively (no pipeline transform needed).">?</span></label>
            </div>
            <div class="field checkbox-row">
              <input type="checkbox" id="force_pipeline" name="force_pipeline">
              <label for="force_pipeline">Force pipeline <span class="tip" tabindex="0" data-tip="Run the processing pipeline even when the backend could handle the request natively.">?</span></label>
            </div>
          </div>
        </div>

        <div class="section"><h3>Modes</h3>
          <div class="field-grid">
            <div class="field">
              <label>Responses mode <span class="tip" tabindex="0" data-tip="Auto: probe backend, cache result. Native: always passthrough. Translate: convert to Chat Completions.">?</span></label>
              <select name="responses_mode">
                <option value="">auto (default)</option>
                <option value="native">native</option>
                <option value="translate">translate</option>
              </select>
            </div>
            <div class="field">
              <label>Messages mode <span class="tip" tabindex="0" data-tip="Auto: Anthropic backends passthrough, others translate. Native: force passthrough. Translate: convert.">?</span></label>
              <select name="messages_mode">
                <option value="">auto (default)</option>
                <option value="native">native</option>
                <option value="translate">translate</option>
              </select>
            </div>
          </div>
        </div>

        <div class="section"><h3>Secrets</h3>
          <div class="field field-full">
            <label>API key <span class="tip" tabindex="0" data-tip="Sent to the backend. Leave blank to keep the current value (edit mode) or to omit auth (add mode).">?</span></label>
            <div class="secret-row">
              <span class="mono" id="apiKeyMask">—</span>
              <button type="button" class="btn btn-secondary btn-sm" onclick="rotateSecret('api_key', this)">Rotate</button>
              <button type="button" class="btn btn-danger btn-sm" onclick="clearSecret('api_key')">Clear</button>
            </div>
            <input type="password" name="api_key" id="api_key_input" style="display:none;margin-top:6px" placeholder="new api key">
          </div>
        </div>

        <div class="section" id="bedrockSection" style="display:none"><h3>Bedrock (SigV4)</h3>
          <div class="field-grid">
            <div class="field">
              <label>Region <span class="tip" tabindex="0" data-tip="AWS region, e.g. us-east-1. Required for bedrock type.">?</span></label>
              <input type="text" name="region" list="regionsList">
              <datalist id="regionsList"></datalist>
            </div>
            <div class="field">
              <label>AWS access key ID <span class="tip" tabindex="0" data-tip="IAM access key (AKIA...). Ignored when the api_key field is set (api key auth).">?</span></label>
              <input type="text" name="aws_access_key">
            </div>
            <div class="field field-full">
              <label>AWS secret access key <span class="tip" tabindex="0" data-tip="IAM secret. Leave blank to keep current value.">?</span></label>
              <div class="secret-row">
                <span class="mono" id="awsSecretMask">—</span>
                <button type="button" class="btn btn-secondary btn-sm" onclick="rotateSecret('aws_secret_key', this)">Rotate</button>
                <button type="button" class="btn btn-danger btn-sm" onclick="clearSecret('aws_secret_key')">Clear</button>
              </div>
              <input type="password" name="aws_secret_key" id="aws_secret_key_input" style="display:none;margin-top:6px">
            </div>
            <div class="field field-full">
              <label>AWS session token <span class="tip" tabindex="0" data-tip="Optional STS temporary-credential session token. Leave blank to keep current value.">?</span></label>
              <div class="secret-row">
                <span class="mono" id="awsSessionMask">—</span>
                <button type="button" class="btn btn-secondary btn-sm" onclick="rotateSecret('aws_session_token', this)">Rotate</button>
                <button type="button" class="btn btn-danger btn-sm" onclick="clearSecret('aws_session_token')">Clear</button>
              </div>
              <input type="password" name="aws_session_token" id="aws_session_token_input" style="display:none;margin-top:6px">
            </div>
            <div class="field">
              <label>Guardrail ID <span class="tip" tabindex="0" data-tip="Optional Bedrock guardrail identifier. Applied to every request for this model.">?</span></label>
              <input type="text" name="guardrail_id">
            </div>
            <div class="field">
              <label>Guardrail version <span class="tip" tabindex="0" data-tip="Guardrail version. Use DRAFT or a numeric version.">?</span></label>
              <input type="text" name="guardrail_version" placeholder="DRAFT">
            </div>
            <div class="field field-full">
              <label>Guardrail trace <span class="tip" tabindex="0" data-tip="Trace mode for guardrail evaluations.">?</span></label>
              <select name="guardrail_trace">
                <option value="">unset</option>
                <option value="enabled">enabled</option>
                <option value="disabled">disabled</option>
                <option value="enabled_full">enabled_full</option>
              </select>
            </div>
          </div>
        </div>

        <div class="section"><h3>Sampling defaults (optional)</h3>
          <div class="field-grid">
            <div class="field"><label>Temperature <span class="tip" tabindex="0" data-tip="0 = deterministic; higher = more random. Range 0–2.">?</span></label><input type="number" name="temperature" step="0.05" min="0" max="2"></div>
            <div class="field"><label>Top-p <span class="tip" tabindex="0" data-tip="Nucleus sampling threshold. Range 0–1.">?</span></label><input type="number" name="top_p" step="0.05" min="0" max="1"></div>
            <div class="field"><label>Top-k <span class="tip" tabindex="0" data-tip="Limit vocabulary to top K tokens. Integer.">?</span></label><input type="number" name="top_k" step="1" min="0"></div>
            <div class="field"><label>Max new tokens <span class="tip" tabindex="0" data-tip="Maximum tokens to generate (maps to max_tokens).">?</span></label><input type="number" name="max_new_tokens" step="1" min="0"></div>
            <div class="field"><label>Frequency penalty <span class="tip" tabindex="0" data-tip="Penalize repeats by frequency. Range 0–2.">?</span></label><input type="number" name="frequency_penalty" step="0.05" min="0" max="2"></div>
            <div class="field"><label>Presence penalty <span class="tip" tabindex="0" data-tip="Penalize tokens that appeared at all. Range 0–2.">?</span></label><input type="number" name="presence_penalty" step="0.05" min="0" max="2"></div>
            <div class="field">
              <label>Reasoning effort <span class="tip" tabindex="0" data-tip="Thinking budget: low, medium, or high.">?</span></label>
              <select name="reasoning_effort">
                <option value="">unset</option>
                <option value="low">low</option>
                <option value="medium">medium</option>
                <option value="high">high</option>
              </select>
            </div>
            <div class="field"><label>Stop sequences <span class="tip" tabindex="0" data-tip="Comma-separated strings that end generation.">?</span></label><input type="text" name="stop" placeholder="e.g. ###, END"></div>
          </div>
        </div>

        <div id="modelFormErr" class="inline-err" style="display:none"></div>
      </div>
      <div class="modal-footer">
        <button type="button" class="btn btn-secondary" onclick="closeModelModal()">Cancel</button>
        <button type="submit" class="btn btn-primary" id="modelSaveBtn">Save</button>
      </div>
    </form>
  </div>
</div>`
}

// modelsPageJS returns the JavaScript for the models tab.
func modelsPageJS() string {
	return `
var mstate = {models: [], regions: [], editing: null, secretOverrides: {}};

function loadModels(){
  apiGet("/admin/models/data").then(function(d){
    mstate.models = d.models || [];
    mstate.regions = d.regions || [];
    var dl = document.getElementById("regionsList");
    dl.innerHTML = "";
    for(var i=0;i<mstate.regions.length;i++){
      var o = document.createElement("option");
      o.value = mstate.regions[i];
      dl.appendChild(o);
    }
    renderModels();
  }).catch(function(e){ flash("Load failed: "+e.message, "error"); });
}

function renderModels(){
  var tbody = document.getElementById("modelsBody");
  if(!mstate.models.length){
    tbody.innerHTML = '<tr><td colspan="7" style="text-align:center;color:var(--muted)">No models configured</td></tr>';
    return;
  }
  var html = "";
  for(var i=0;i<mstate.models.length;i++){
    var m = mstate.models[i];
    var t = m.type || "openai";
    var backend = m.backend || "";
    if(backend.length > 48) backend = backend.substring(0,45) + "…";
    var ctx = m.context_window ? Number(m.context_window).toLocaleString() : "auto";
    var vision = m.supports_vision ? "✓" : "";
    var healthHTML = renderHealth(m.health);
    html += '<tr>' +
      '<td><strong>'+esc(m.name)+'</strong></td>' +
      '<td><code>'+esc(t)+'</code></td>' +
      '<td class="mono" title="'+esc(m.backend||"")+'">'+esc(backend)+'</td>' +
      '<td>'+esc(ctx)+'</td>' +
      '<td style="text-align:center">'+vision+'</td>' +
      '<td>'+healthHTML+'</td>' +
      '<td class="row-actions">' +
        '<button class="btn btn-secondary btn-sm" onclick="openModelModal(\\''+escAttr(m.name)+'\\')">Edit</button>' +
        '<button class="btn btn-danger btn-sm" onclick="deleteModel(\\''+escAttr(m.name)+'\\')">Delete</button>' +
      '</td>' +
    '</tr>';
  }
  tbody.innerHTML = html;
}

function renderHealth(h){
  if(!h) return '<span class="health-dot health-unknown" title="Unknown"></span><span class="mono">—</span>';
  var cls = h.online ? "health-online" : "health-offline";
  var label = h.online ? "online" : "offline";
  var title = h.error ? "Offline: "+h.error : label;
  return '<span class="health-dot '+cls+'" title="'+esc(title)+'"></span><span class="mono">'+esc(label)+(h.external?" (external)":"")+'</span>';
}

function openModelModal(name){
  var m = name ? mstate.models.find(function(x){return x.name === name;}) : null;
  mstate.editing = name;
  mstate.secretOverrides = {};
  document.getElementById("modelModalTitle").textContent = name ? ("Edit: "+name) : "Add Model";
  var form = document.getElementById("modelForm");
  form.reset();
  // Reset secret input visibility.
  ["api_key","aws_secret_key","aws_session_token"].forEach(function(id){
    var inp = document.getElementById(id+"_input");
    if(inp){ inp.style.display="none"; inp.value=""; }
  });
  if(m){
    form.elements["name"].value = m.name || "";
    form.elements["backend"].value = m.backend || "";
    form.elements["type"].value = m.type || "";
    form.elements["model"].value = (m.model && m.model !== m.name) ? m.model : "";
    form.elements["timeout"].value = m.timeout || "";
    form.elements["context_window"].value = m.context_window || "";
    form.elements["supports_vision"].checked = !!m.supports_vision;
    form.elements["force_pipeline"].checked = !!m.force_pipeline;
    form.elements["responses_mode"].value = m.responses_mode || "";
    form.elements["messages_mode"].value = m.messages_mode || "";
    form.elements["region"].value = m.region || "";
    form.elements["aws_access_key"].value = m.aws_access_key || "";
    form.elements["guardrail_id"].value = m.guardrail_id || "";
    form.elements["guardrail_version"].value = m.guardrail_version || "";
    form.elements["guardrail_trace"].value = m.guardrail_trace || "";
    document.getElementById("apiKeyMask").textContent = m.has_api_key ? m.api_key_masked : "(not set)";
    document.getElementById("awsSecretMask").textContent = m.has_aws_secret ? m.aws_secret_mask : "(not set)";
    document.getElementById("awsSessionMask").textContent = m.has_aws_session ? "(set)" : "(not set)";
    var d = m.defaults;
    if(d){
      if(d.temperature != null) form.elements["temperature"].value = d.temperature;
      if(d.top_p != null) form.elements["top_p"].value = d.top_p;
      if(d.top_k != null) form.elements["top_k"].value = d.top_k;
      if(d.max_new_tokens != null) form.elements["max_new_tokens"].value = d.max_new_tokens;
      if(d.frequency_penalty != null) form.elements["frequency_penalty"].value = d.frequency_penalty;
      if(d.presence_penalty != null) form.elements["presence_penalty"].value = d.presence_penalty;
      if(d.reasoning_effort) form.elements["reasoning_effort"].value = d.reasoning_effort;
      if(d.stop && d.stop.length) form.elements["stop"].value = d.stop.join(", ");
    }
  } else {
    document.getElementById("apiKeyMask").textContent = "(not set)";
    document.getElementById("awsSecretMask").textContent = "(not set)";
    document.getElementById("awsSessionMask").textContent = "(not set)";
  }
  onTypeChange();
  document.getElementById("modelFormErr").style.display = "none";
  document.getElementById("modelModal").classList.add("open");
  // Focus first field.
  setTimeout(function(){ form.elements["name"].focus(); }, 20);
}

function closeModelModal(){
  document.getElementById("modelModal").classList.remove("open");
  mstate.editing = null;
}

function onBackdropClick(ev){
  if(ev.target.id === "modelModal") closeModelModal();
}

document.addEventListener("keydown", function(ev){
  if(ev.key === "Escape" && document.getElementById("modelModal").classList.contains("open")){
    closeModelModal();
  }
});

function onTypeChange(){
  var form = document.getElementById("modelForm");
  var t = form.elements["type"].value;
  document.getElementById("bedrockSection").style.display = (t === "bedrock") ? "" : "none";
  var bi = document.getElementById("backendInput");
  if(!bi.value){
    if(t === "anthropic") bi.placeholder = "https://api.anthropic.com";
    else if(t === "bedrock") bi.placeholder = "auto from region (or override for VPC endpoints)";
    else bi.placeholder = "http://localhost:8000/v1";
  }
}

function rotateSecret(field, btn){
  var inp = document.getElementById(field + "_input");
  inp.style.display = "block";
  inp.focus();
  mstate.secretOverrides[field] = "rotate";
}

function clearSecret(field){
  mstate.secretOverrides[field] = "clear";
  var mask = {api_key:"apiKeyMask", aws_secret_key:"awsSecretMask", aws_session_token:"awsSessionMask"}[field];
  if(mask) document.getElementById(mask).textContent = "(will be cleared on save)";
  var inp = document.getElementById(field + "_input");
  if(inp){ inp.style.display="none"; inp.value=""; }
}

function collectForm(){
  var form = document.getElementById("modelForm");
  var body = {
    name: form.elements["name"].value.trim(),
    backend: form.elements["backend"].value.trim(),
    type: form.elements["type"].value,
    model: form.elements["model"].value.trim(),
    timeout: parseInt(form.elements["timeout"].value,10) || 0,
    context_window: parseInt(form.elements["context_window"].value,10) || 0,
    supports_vision: form.elements["supports_vision"].checked,
    force_pipeline: form.elements["force_pipeline"].checked,
    responses_mode: form.elements["responses_mode"].value,
    messages_mode: form.elements["messages_mode"].value,
    region: form.elements["region"].value.trim(),
    aws_access_key: form.elements["aws_access_key"].value.trim(),
    guardrail_id: form.elements["guardrail_id"].value.trim(),
    guardrail_version: form.elements["guardrail_version"].value.trim(),
    guardrail_trace: form.elements["guardrail_trace"].value
  };
  // Secret handling
  function secretField(name){
    var override = mstate.secretOverrides[name];
    if(override === "clear") return "";
    if(override === "rotate"){
      var v = form.elements[name].value;
      return v; // may be empty meaning "clear"
    }
    return null; // omit — preserves existing
  }
  body.api_key = secretField("api_key");
  body.aws_secret_key = secretField("aws_secret_key");
  body.aws_session_token = secretField("aws_session_token");
  // Sampling defaults — only send non-empty
  var d = {};
  var anyDefault = false;
  ["temperature","top_p","frequency_penalty","presence_penalty"].forEach(function(k){
    var v = form.elements[k].value;
    if(v !== ""){ d[k] = parseFloat(v); anyDefault = true; }
  });
  ["top_k","max_new_tokens"].forEach(function(k){
    var v = form.elements[k].value;
    if(v !== ""){ d[k] = parseInt(v,10); anyDefault = true; }
  });
  var re = form.elements["reasoning_effort"].value;
  if(re){ d.reasoning_effort = re; anyDefault = true; }
  var stopRaw = form.elements["stop"].value.trim();
  if(stopRaw){
    d.stop = stopRaw.split(",").map(function(s){return s.trim();}).filter(function(s){return s.length;});
    if(d.stop.length) anyDefault = true;
  }
  if(anyDefault) body.defaults = d;
  return body;
}

function submitModel(ev){
  ev.preventDefault();
  var errEl = document.getElementById("modelFormErr");
  errEl.style.display = "none";
  var body = collectForm();
  if(!body.name){ return showFormErr("Name is required"); }
  if(!body.backend && body.type !== "bedrock"){ return showFormErr("Backend URL is required"); }
  if(body.backend){
    try { new URL(body.backend); } catch(e){ return showFormErr("Backend URL is invalid"); }
  }
  if(body.type === "bedrock" && !body.region){ return showFormErr("Bedrock models require a region"); }

  var btn = document.getElementById("modelSaveBtn");
  btn.disabled = true; btn.textContent = "Saving…";
  var req;
  if(mstate.editing){
    req = apiPost("/admin/models/mutate", {action:"update", original_name: mstate.editing, model: body});
  } else {
    req = apiPost("/admin/models/mutate", {action:"add", model: body});
  }
  req.then(function(res){
    btn.disabled = false; btn.textContent = "Save";
    if(!res.ok){ showFormErr(res.json.error && res.json.error.message || "Save failed"); return; }
    closeModelModal();
    flash("Saved", "success");
    loadModels();
  }).catch(function(e){
    btn.disabled = false; btn.textContent = "Save";
    showFormErr(e.message || "Save failed");
  });
}

function showFormErr(msg){
  var el = document.getElementById("modelFormErr");
  el.textContent = msg;
  el.style.display = "block";
}

function deleteModel(name){
  if(!confirm('Delete model "'+name+'"? If keys reference it, the delete will be refused.')) return;
  apiPost("/admin/models/mutate", {action:"delete", name: name}).then(function(res){
    if(!res.ok){
      var msg = (res.json.error && res.json.error.message) || "Delete failed";
      if(res.status === 409 && confirm(msg + "\\n\\nForce-delete anyway? References will be stripped from keys.")){
        apiPost("/admin/models/mutate", {action:"delete", name: name, force: true}).then(function(r2){
          if(!r2.ok){ flash(r2.json.error && r2.json.error.message || "Force delete failed", "error"); return; }
          flash("Deleted", "success"); loadModels();
        });
        return;
      }
      flash(msg, "error");
      return;
    }
    flash("Deleted", "success");
    loadModels();
  });
}

loadModels();
`
}
