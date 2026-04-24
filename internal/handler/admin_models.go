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
        <th style="width:200px">Name</th><th style="width:90px">Type</th><th>Backend</th><th style="width:90px">Context</th><th style="width:60px;text-align:center">Vision</th><th style="width:60px;text-align:center">Audio</th><th style="width:150px">Health</th><th style="width:130px;text-align:right">Actions</th>
      </tr></thead>
      <tbody id="modelsBody"><tr><td colspan="8" style="text-align:center;color:var(--muted)">Loading…</td></tr></tbody>
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
			"supports_audio":    m.SupportsAudio,
			"force_pipeline":    m.ForcePipeline,
			"responses_mode":    m.ResponsesMode,
			"messages_mode":     m.MessagesMode,
			"has_api_key":       m.APIKey != "",
			"api_key_masked":    config.MaskSecret(m.APIKey),
			"auth_header_name":  m.AuthHeaderName,
			"auth_scheme":       m.AuthScheme,
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
		if m.Processors != nil {
			entry["processors"] = map[string]any{
				"vision":              m.Processors.Vision,
				"ocr":                 m.Processors.OCR,
				"has_web_search_key":  m.Processors.WebSearchKey != "",
				"web_search_key_mask": config.MaskSecret(m.Processors.WebSearchKey),
			}
		} else {
			entry["processors"] = nil
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
	SupportsAudio    bool                `json:"supports_audio"`
	ForcePipeline    bool                `json:"force_pipeline"`
	ResponsesMode    string              `json:"responses_mode"`
	MessagesMode     string              `json:"messages_mode"`
	APIKey           *string             `json:"api_key"`
	AuthHeaderName   string              `json:"auth_header_name"`
	AuthScheme       string              `json:"auth_scheme"`
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
	WebSearchKey *string `json:"web_search_key"` // nil = keep existing; empty = clear; non-empty = replace
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
		SupportsAudio:    d.SupportsAudio,
		ForcePipeline:    d.ForcePipeline,
		ResponsesMode:    d.ResponsesMode,
		MessagesMode:     d.MessagesMode,
		AuthHeaderName:   d.AuthHeaderName,
		AuthScheme:       d.AuthScheme,
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
		// Per-model web_search_key uses the same pointer-style preserve-or-replace
		// semantics as api_key: nil = keep existing, empty string = clear.
		var webKey string
		if d.Processors.WebSearchKey != nil {
			webKey = *d.Processors.WebSearchKey
		} else if existing != nil && existing.Processors != nil {
			webKey = existing.Processors.WebSearchKey
		}
		mc.Processors = &config.ProcessorsConfig{
			Vision:       d.Processors.Vision,
			OCR:          d.Processors.OCR,
			WebSearchKey: webKey,
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

        <div class="section section-required"><h3>Required</h3>
          <div class="field-grid">
            <div class="field">
              <label>Name <span class="tip" tabindex="0" data-tip="Name clients use to address this model in requests (e.g. 'kimi', 'claude-sonnet'). This is the identifier sent in the model field of OpenAI/Anthropic requests.">?</span></label>
              <input type="text" name="name" required>
            </div>
            <div class="field">
              <label>Type <span class="tip" tabindex="0" data-tip="Backend protocol. Default: openai. Choose anthropic for native Anthropic Messages API backends, bedrock for AWS Bedrock Converse. The proxy translates between protocols as needed.">?</span></label>
              <select name="type" onchange="onTypeChange()">
                <option value="">openai (default)</option>
                <option value="openai">openai</option>
                <option value="anthropic">anthropic</option>
                <option value="bedrock">bedrock</option>
              </select>
            </div>
            <div class="field field-full">
              <label>Backend URL <span class="tip" tabindex="0" data-tip="Upstream server URL. Default for bedrock: https://bedrock-runtime.{region}.amazonaws.com (auto-derived — leave empty unless you need a VPC endpoint). Required for openai/anthropic.">?</span></label>
              <input type="url" name="backend" id="backendInput" placeholder="http://localhost:8000/v1">
            </div>
            <div class="field field-full">
              <label>Upstream model name <span class="tip" tabindex="0" data-tip="Model identifier sent to the backend. Default: same as Name. Configure when the backend expects a different ID — e.g. Name='kimi' but backend expects 'moonshot.kimi-k2-thinking'.">?</span></label>
              <input type="text" name="model" placeholder="same as name">
            </div>
            <div class="field field-full">
              <label>API key <span class="tip" tabindex="0" data-tip="Upstream API key. Default: empty (no auth). By default OpenAI-compatible backends send it as Authorization: Bearer and Anthropic backends send it as x-api-key. Use the auth fields below to override that behavior. For bedrock, optional: sets an AWS Bedrock API key; otherwise SigV4 uses the AWS credentials below.">?</span></label>
              <div class="secret-row">
                <span class="mono" id="apiKeyMask">—</span>
                <button type="button" class="btn btn-secondary btn-sm" onclick="rotateSecret('api_key', this)">Rotate</button>
                <button type="button" class="btn btn-danger btn-sm" onclick="clearSecret('api_key')">Clear</button>
              </div>
              <input type="password" name="api_key" id="api_key_input" style="display:none;margin-top:6px" placeholder="enter new key — leave blank to keep existing">
            </div>
            <div class="field">
              <label>Auth header name <span class="tip" tabindex="0" data-tip="Default: Authorization for OpenAI-compatible backends, x-api-key for Anthropic backends. Override when an upstream expects the API key in a different header.">?</span></label>
              <input type="text" name="auth_header_name" placeholder="default for backend type">
            </div>
            <div class="field">
              <label>Auth scheme <span class="tip" tabindex="0" data-tip="Default: bearer for Authorization, raw for other auth headers. Use bearer for Authorization: Bearer &lt;key&gt; or raw to send the key value as-is in the configured header.">?</span></label>
              <select name="auth_scheme">
                <option value="">default</option>
                <option value="bearer">bearer</option>
                <option value="raw">raw</option>
              </select>
            </div>
          </div>
        </div>

        <hr class="section-divider">

        <div class="section"><h3>Capabilities</h3>
          <div class="field-grid">
            <div class="field">
              <label>Context window (tokens) <span class="tip" tabindex="0" data-tip="Max context size advertised on /v1/models. Default: auto-detect from the backend. Set explicitly when auto-detection fails (common for Bedrock models not in the built-in lookup table) or to clamp below the model's real limit.">?</span></label>
              <input type="number" name="context_window" min="0" step="1" placeholder="auto">
            </div>
            <div class="field checkbox-row">
              <input type="checkbox" id="supports_vision" name="supports_vision">
              <label for="supports_vision">Supports vision <span class="tip" tabindex="0" data-tip="Default: off. Check when the model natively accepts image content parts. When off, images are routed through the vision pipeline (which calls a configured vision model to describe the image).">?</span></label>
            </div>
            <div class="field checkbox-row">
              <input type="checkbox" id="supports_audio" name="supports_audio">
              <label for="supports_audio">Supports audio <span class="tip" tabindex="0" data-tip="Default: off. Check for whisper-style transcription endpoints or chat models that natively accept input_audio content parts.">?</span></label>
            </div>
          </div>
        </div>

        <div class="section" id="bedrockSection" style="display:none"><h3>Bedrock credentials</h3>
          <div class="field-grid">
            <div class="field">
              <label>Region <span class="tip" tabindex="0" data-tip="AWS region (e.g. us-east-1). Default: none — required for bedrock. Region determines data residency and which models are available.">?</span></label>
              <input type="text" name="region" list="regionsList">
              <datalist id="regionsList"></datalist>
            </div>
            <div class="field">
              <label>AWS access key ID <span class="tip" tabindex="0" data-tip="IAM access key (AKIA...). Default: falls back to AWS_ACCESS_KEY_ID env var. Ignored when the API key above is set (Bedrock API key takes precedence).">?</span></label>
              <input type="text" name="aws_access_key">
            </div>
            <div class="field field-full">
              <label>AWS secret access key <span class="tip" tabindex="0" data-tip="IAM secret. Default: falls back to AWS_SECRET_ACCESS_KEY env var. Leave blank to keep current value when editing.">?</span></label>
              <div class="secret-row">
                <span class="mono" id="awsSecretMask">—</span>
                <button type="button" class="btn btn-secondary btn-sm" onclick="rotateSecret('aws_secret_key', this)">Rotate</button>
                <button type="button" class="btn btn-danger btn-sm" onclick="clearSecret('aws_secret_key')">Clear</button>
              </div>
              <input type="password" name="aws_secret_key" id="aws_secret_key_input" style="display:none;margin-top:6px" placeholder="enter new secret — leave blank to keep existing">
            </div>
            <div class="field field-full">
              <label>AWS session token <span class="tip" tabindex="0" data-tip="STS session token for temporary credentials. Default: none. Only needed when using temporary credentials (assumed-role, federated login).">?</span></label>
              <div class="secret-row">
                <span class="mono" id="awsSessionMask">—</span>
                <button type="button" class="btn btn-secondary btn-sm" onclick="rotateSecret('aws_session_token', this)">Rotate</button>
                <button type="button" class="btn btn-danger btn-sm" onclick="clearSecret('aws_session_token')">Clear</button>
              </div>
              <input type="password" name="aws_session_token" id="aws_session_token_input" style="display:none;margin-top:6px" placeholder="enter new token — leave blank to keep existing">
            </div>
            <div class="field">
              <label>Guardrail ID <span class="tip" tabindex="0" data-tip="Bedrock guardrail identifier. Default: none. When set, applied to every request for this model to filter prompts/completions against AWS-managed policies.">?</span></label>
              <input type="text" name="guardrail_id">
            </div>
            <div class="field">
              <label>Guardrail version <span class="tip" tabindex="0" data-tip="Default: DRAFT (points to the latest unreleased version). Use a numeric version (1, 2, ...) to pin to a specific published version.">?</span></label>
              <input type="text" name="guardrail_version" placeholder="DRAFT">
            </div>
            <div class="field field-full">
              <label>Guardrail trace <span class="tip" tabindex="0" data-tip="Default: unset. Set to 'enabled' or 'enabled_full' to capture guardrail evaluation traces in the response — useful when debugging false positives.">?</span></label>
              <select name="guardrail_trace">
                <option value="">unset (default)</option>
                <option value="enabled">enabled</option>
                <option value="disabled">disabled</option>
                <option value="enabled_full">enabled_full</option>
              </select>
            </div>
          </div>
        </div>

        <div class="section"><h3>Protocol overrides</h3>
          <div class="field-grid">
            <div class="field">
              <label>Responses mode <span class="tip" tabindex="0" data-tip="Default: auto (probe the backend for /v1/responses support and cache the result). Use native to force passthrough when you know the backend supports it, or translate to always convert /v1/responses to /v1/chat/completions.">?</span></label>
              <select name="responses_mode">
                <option value="">auto (default)</option>
                <option value="native">native</option>
                <option value="translate">translate</option>
              </select>
            </div>
            <div class="field">
              <label>Messages mode <span class="tip" tabindex="0" data-tip="Default: auto (Anthropic backends passthrough; others translate). Use native to force passthrough, or translate to convert Anthropic /v1/messages to OpenAI chat.">?</span></label>
              <select name="messages_mode">
                <option value="">auto (default)</option>
                <option value="native">native</option>
                <option value="translate">translate</option>
              </select>
            </div>
            <div class="field">
              <label>Timeout (seconds) <span class="tip" tabindex="0" data-tip="Request timeout. Default: 300. Increase for long-running reasoning models (GPT-5, Claude extended thinking, o-series) that may take several minutes to respond.">?</span></label>
              <input type="number" name="timeout" min="0" step="1" placeholder="300">
            </div>
            <div class="field checkbox-row">
              <input type="checkbox" id="force_pipeline" name="force_pipeline">
              <label for="force_pipeline">Force pipeline <span class="tip" tabindex="0" data-tip="Default: off. When on, runs the processing pipeline (vision/OCR/etc.) even when the backend could handle the request natively. Useful for normalizing inputs across mixed backends.">?</span></label>
            </div>
          </div>
        </div>

        <div class="section"><h3>Sampling defaults</h3>
          <div class="field-grid">
            <div class="field"><label>Temperature <span class="tip" tabindex="0" data-tip="Default: unset (backend decides, typically 1.0). 0 = deterministic; higher = more random. Range 0–2.">?</span></label><input type="number" name="temperature" step="0.05" min="0" max="2"></div>
            <div class="field"><label>Top-p <span class="tip" tabindex="0" data-tip="Default: unset (backend decides). Nucleus sampling threshold. Range 0–1.">?</span></label><input type="number" name="top_p" step="0.05" min="0" max="1"></div>
            <div class="field"><label>Top-k <span class="tip" tabindex="0" data-tip="Default: unset. Limit vocabulary to top K tokens. Not supported by all backends.">?</span></label><input type="number" name="top_k" step="1" min="0"></div>
            <div class="field"><label>Max new tokens <span class="tip" tabindex="0" data-tip="Default: unset (backend default, often 4096). Maximum tokens to generate (maps to max_tokens in the upstream request).">?</span></label><input type="number" name="max_new_tokens" step="1" min="0"></div>
            <div class="field"><label>Frequency penalty <span class="tip" tabindex="0" data-tip="Default: unset (0). Penalize tokens by how often they've appeared. Range 0–2.">?</span></label><input type="number" name="frequency_penalty" step="0.05" min="0" max="2"></div>
            <div class="field"><label>Presence penalty <span class="tip" tabindex="0" data-tip="Default: unset (0). Penalize tokens that appeared at all. Range 0–2.">?</span></label><input type="number" name="presence_penalty" step="0.05" min="0" max="2"></div>
            <div class="field">
              <label>Reasoning effort <span class="tip" tabindex="0" data-tip="Default: unset (backend decides). Thinking budget for reasoning models (GPT-5, Claude extended-thinking, o-series).">?</span></label>
              <select name="reasoning_effort">
                <option value="">unset (default)</option>
                <option value="low">low</option>
                <option value="medium">medium</option>
                <option value="high">high</option>
              </select>
            </div>
            <div class="field"><label>Stop sequences <span class="tip" tabindex="0" data-tip="Default: none. Comma-separated strings that end generation when produced by the model.">?</span></label><input type="text" name="stop" placeholder="e.g. ###, END"></div>
          </div>
        </div>

        <div class="section"><h3>Per-model processors</h3>
          <div class="field-grid">
            <div class="field">
              <label>Vision model <span class="tip" tabindex="0" data-tip="Default: global default (processors.vision). Override the vision processor for this model, or set to 'none' to disable vision processing entirely for this model.">?</span></label>
              <input type="text" name="proc_vision" list="allModelsList" placeholder="global default">
            </div>
            <div class="field">
              <label>OCR model <span class="tip" tabindex="0" data-tip="Default: global default (processors.ocr), which itself falls back to the vision model. Override only if a dedicated OCR model works better than vision for PDF page extraction.">?</span></label>
              <input type="text" name="proc_ocr" list="allModelsList" placeholder="global default">
            </div>
            <div class="field field-full">
              <label>Web search key <span class="tip" tabindex="0" data-tip="Default: global default (Tavily or Brave key from top-level config). Override to use a different search key for this model, e.g. for isolated billing. Rotate to replace; Clear to wipe and fall back to global.">?</span></label>
              <div class="secret-row">
                <span class="mono" id="procWebSearchKeyMask">—</span>
                <button type="button" class="btn btn-secondary btn-sm" onclick="rotateSecret('proc_web_search_key', this)">Rotate</button>
                <button type="button" class="btn btn-danger btn-sm" onclick="clearSecret('proc_web_search_key')">Clear</button>
              </div>
              <input type="password" name="proc_web_search_key" id="proc_web_search_key_input" style="display:none;margin-top:6px" placeholder="enter new key — leave blank to keep existing">
            </div>
          </div>
        </div>
        <datalist id="allModelsList"></datalist>

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
    // Populate allModelsList datalist for processor dropdowns.
    var mdl = document.getElementById("allModelsList");
    if(mdl){
      mdl.innerHTML = "";
      for(var i=0;i<mstate.models.length;i++){
        var o = document.createElement("option");
        o.value = mstate.models[i].name;
        mdl.appendChild(o);
      }
      var nopt = document.createElement("option");
      nopt.value = "none";
      mdl.appendChild(nopt);
    }
    renderModels();
  }).catch(function(e){ flash("Load failed: "+e.message, "error"); });
}

function renderModels(){
  var tbody = document.getElementById("modelsBody");
  if(!mstate.models.length){
    tbody.innerHTML = '<tr><td colspan="8" style="text-align:center;color:var(--muted)">No models configured</td></tr>';
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
    var audio = m.supports_audio ? "✓" : "";
    var healthHTML = renderHealth(m.health);
    html += '<tr>' +
      '<td class="cell-name" title="'+esc(m.name)+'"><strong>'+esc(m.name)+'</strong></td>' +
      '<td><code>'+esc(t)+'</code></td>' +
      '<td class="mono" title="'+esc(m.backend||"")+'">'+esc(backend)+'</td>' +
      '<td>'+esc(ctx)+'</td>' +
      '<td style="text-align:center">'+vision+'</td>' +
      '<td style="text-align:center">'+audio+'</td>' +
      '<td>'+healthHTML+'</td>' +
      '<td class="row-actions"><div class="action-group">' +
        '<button class="btn btn-secondary btn-sm" onclick="openModelModal(\''+escAttr(m.name)+'\')">Edit</button>' +
        '<button class="btn btn-danger btn-sm" onclick="deleteModel(\''+escAttr(m.name)+'\')">Delete</button>' +
      '</div></td>' +
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
    form.elements["supports_audio"].checked = !!m.supports_audio;
    form.elements["force_pipeline"].checked = !!m.force_pipeline;
    form.elements["responses_mode"].value = m.responses_mode || "";
    form.elements["messages_mode"].value = m.messages_mode || "";
    form.elements["auth_header_name"].value = m.auth_header_name || "";
    form.elements["auth_scheme"].value = m.auth_scheme || "";
    form.elements["region"].value = m.region || "";
    form.elements["aws_access_key"].value = m.aws_access_key || "";
    form.elements["guardrail_id"].value = m.guardrail_id || "";
    form.elements["guardrail_version"].value = m.guardrail_version || "";
    form.elements["guardrail_trace"].value = m.guardrail_trace || "";
    // Per-model processors
    if(m.processors){
      form.elements["proc_vision"].value = m.processors.vision || "";
      form.elements["proc_ocr"].value = m.processors.ocr || "";
      document.getElementById("procWebSearchKeyMask").textContent =
        m.processors.has_web_search_key ? m.processors.web_search_key_mask : "(not set)";
    } else {
      document.getElementById("procWebSearchKeyMask").textContent = "(not set)";
    }
    document.getElementById("proc_web_search_key_input").style.display = "none";
    document.getElementById("proc_web_search_key_input").value = "";
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
    document.getElementById("procWebSearchKeyMask").textContent = "(not set)";
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
  var mask = {
    api_key:"apiKeyMask",
    aws_secret_key:"awsSecretMask",
    aws_session_token:"awsSessionMask",
    proc_web_search_key:"procWebSearchKeyMask"
  }[field];
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
    supports_audio: form.elements["supports_audio"].checked,
    force_pipeline: form.elements["force_pipeline"].checked,
    responses_mode: form.elements["responses_mode"].value,
    messages_mode: form.elements["messages_mode"].value,
    auth_header_name: form.elements["auth_header_name"].value.trim(),
    auth_scheme: form.elements["auth_scheme"].value,
    region: form.elements["region"].value.trim(),
    aws_access_key: form.elements["aws_access_key"].value.trim(),
    guardrail_id: form.elements["guardrail_id"].value.trim(),
    guardrail_version: form.elements["guardrail_version"].value.trim(),
    guardrail_trace: form.elements["guardrail_trace"].value
  };
  // Secret handling. Semantics:
  //   override "clear"  → send "" (server clears the value)
  //   override "rotate" → send trimmed value only when non-empty
  //                       (empty rotate = no change — use Clear to wipe)
  //   no override       → omit entirely (server preserves existing)
  function secretField(name){
    var override = mstate.secretOverrides[name];
    if(override === "clear") return "";
    if(override === "rotate"){
      var v = form.elements[name].value.trim();
      if(v === "") return null; // blank rotate is a no-op — prevents accidental wipe
      return v;
    }
    return null;
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
  // Per-model processors. Web search key uses the shared secretField
  // helper so it inherits the blank-rotate-is-no-op safety and is never
  // sent plaintext on a no-change save.
  var pv = form.elements["proc_vision"].value.trim();
  var po = form.elements["proc_ocr"].value.trim();
  var pwSecret = secretField("proc_web_search_key"); // null = keep, "" = clear, "value" = set
  if(pv || po || pwSecret !== null){
    body.processors = {vision: pv, ocr: po};
    if(pwSecret !== null) body.processors.web_search_key = pwSecret;
  }
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
