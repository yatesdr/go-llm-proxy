package handler

import (
	"bytes"
	"encoding/json"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"net/url"

	"go-llm-proxy/internal/config"
	"go-llm-proxy/internal/httputil"
)

// modelInfo is the public metadata exposed to the config page.
// It intentionally omits backend URLs, API keys, and other sensitive fields.
type modelInfo struct {
	ID             string `json:"id"`
	Local          bool   `json:"local"`
	Protocol       string `json:"protocol"`        // "openai" or "anthropic"
	Type           string `json:"type"`            // backend type: "openai", "anthropic", "bedrock"
	ContextWindow  int    `json:"context_window"`  // max tokens (0 = unknown)
	SupportsVision bool   `json:"supports_vision"` // model handles images natively
	SupportsAudio  bool   `json:"supports_audio"`  // model handles audio (transcription or audio input)
}

var privateRanges = []struct{ start, end net.IP }{
	{net.ParseIP("10.0.0.0").To4(), net.ParseIP("10.255.255.255").To4()},
	{net.ParseIP("172.16.0.0").To4(), net.ParseIP("172.31.255.255").To4()},
	{net.ParseIP("192.168.0.0").To4(), net.ParseIP("192.168.255.255").To4()},
	{net.ParseIP("127.0.0.0").To4(), net.ParseIP("127.255.255.255").To4()},
	{net.ParseIP("0.0.0.0").To4(), net.ParseIP("0.0.0.0").To4()},
}

func isPrivateIP(host string) bool {
	h, _, err := net.SplitHostPort(host)
	if err != nil {
		h = host
	}
	ip := net.ParseIP(h)
	if ip == nil {
		return false
	}

	// Check IPv6 loopback and private ranges.
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() {
		return true
	}

	ip4 := ip.To4()
	if ip4 == nil {
		return false
	}
	for _, r := range privateRanges {
		if ipInRange(ip4, r.start, r.end) {
			return true
		}
	}
	return false
}

func ipInRange(ip, lo, hi net.IP) bool {
	return bytes.Compare(ip, lo) >= 0 && bytes.Compare(ip, hi) <= 0
}

func modelInfoFromConfig(cfg *config.Config) []modelInfo {
	out := make([]modelInfo, 0, len(cfg.Models))
	for _, m := range cfg.Models {
		u, _ := url.Parse(m.Backend)
		local := false
		if u != nil {
			host := u.Hostname()
			if host == "localhost" {
				local = true
			} else {
				local = isPrivateIP(u.Host)
			}
		}
		proto := "openai"
		if m.Type == config.BackendAnthropic {
			proto = "anthropic"
		}
		out = append(out, modelInfo{
			ID:             m.Name,
			Local:          local,
			Protocol:       proto,
			Type:           m.Type,
			ContextWindow:  m.ContextWindow,
			SupportsVision: m.SupportsVision,
			SupportsAudio:  m.SupportsAudio,
		})
	}
	return out
}

// ConfigPageHandler serves the config generator UI at GET /.
type ConfigPageHandler struct {
	config *config.ConfigStore
	health *config.HealthStore
	tmpl   *template.Template
}

func NewConfigPageHandler(cs *config.ConfigStore, health *config.HealthStore) *ConfigPageHandler {
	return &ConfigPageHandler{
		config: cs,
		health: health,
		tmpl:   template.Must(template.New("page").Parse(configPageHTML)),
	}
}

func (h *ConfigPageHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	cfg := h.config.Get()
	models := modelInfoFromConfig(cfg)
	health := h.health.GetStatus()

	// Create model status map for quick lookup.
	modelStatus := make(map[string]map[string]any, len(health))
	for name, s := range health {
		modelStatus[name] = map[string]any{
			"online":     s.Online,
			"last_check": s.LastCheck.Unix(),
			"error":      s.Error,
		}
	}

	data, err := json.Marshal(models)
	if err != nil {
		slog.Error("failed to marshal model info", "error", err)
		httputil.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}

	healthData, err := json.Marshal(modelStatus)
	if err != nil {
		slog.Error("failed to marshal health status", "error", err)
		httputil.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}

	// Pass processors config as separate JS variables.
	type tmplData struct {
		Mark         template.HTML
		BaseCSS      template.CSS
		Models       template.JS
		Health       template.JS
		HasVision    bool
		HasWebSearch bool
		HasMCP       bool
	}
	td := tmplData{
		Mark:         template.HTML(eidonixMark("brand-mark")),
		BaseCSS:      template.CSS(dashboardCSS() + adminCSS()),
		Models:       template.JS(data),
		Health:       template.JS(healthData),
		HasVision:    len(cfg.Processors.EffectiveVisionModels()) > 0,
		HasWebSearch: len(cfg.Processors.EffectiveSearchKeys()) > 0,
		HasMCP:       len(cfg.Processors.EffectiveSearchKeys()) > 0,
	}

	var buf bytes.Buffer
	if err := h.tmpl.Execute(&buf, td); err != nil {
		slog.Error("failed to render config page", "error", err)
		httputil.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}

	httputil.SetSecurityHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", cspWithFonts)
	if _, err := buf.WriteTo(w); err != nil {
		slog.Error("failed to write config page response", "error", err)
	}
}

const configPageHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Eidonix · Client Setup</title>
<link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=IBM+Plex+Sans:wght@400;500;600;700&family=IBM+Plex+Mono:wght@400;500&display=swap">
<style>{{.BaseCSS}}
body{overflow:auto}
/* ---- Setup page ---- */
.gen-section{margin-top:20px;border-top:1px solid var(--hairline);padding-top:14px}
.gen-section h3{font-size:13px;font-weight:700;text-transform:uppercase;letter-spacing:.06em;margin-bottom:8px}
.endpoints{margin-top:12px;display:flex;gap:8px;align-items:center;flex-wrap:wrap}
.endpoints code{background:var(--panel);border:1px solid var(--hairline);padding:2px 8px;font-family:var(--mono);font-size:12px;white-space:nowrap}
.login-icon{display:inline-flex;align-items:center;color:#8a8a8e;padding:4px}
.login-icon:hover{color:#fff;text-decoration:none}
.container{max-width:1280px;margin:0 auto;padding:16px}
.model-table{width:100%;border-collapse:collapse;font-size:13px;margin-top:6px}
.model-table th{text-align:left;font-size:11px;font-weight:600;text-transform:uppercase;letter-spacing:.08em;color:var(--black);padding:6px 10px;border-bottom:2px solid var(--black)}
.model-table td{padding:8px 10px;border-bottom:1px solid var(--hairline);vertical-align:middle;font-variant-numeric:tabular-nums}
.model-table tr:last-child td{border-bottom:none}
.model-table tr:hover td{background:var(--panel)}
.tag{display:inline-block;font-size:11px;font-weight:600;letter-spacing:.05em;text-transform:uppercase;padding:0 6px;border:1px solid var(--hairline);color:var(--steel);background:var(--paper);white-space:nowrap;vertical-align:1px}
.tag-warn{border-color:var(--red);color:var(--red);background:var(--red-wash)}
.tag-local{background:#E9F5EC;border-color:#BFDFC9;color:#1E6B3C}
.tag-bedrock{background:#1E8E5A;border-color:#1E8E5A;color:#fff}
.tag-3p{background:#FCE9CB;border-color:#E3A94E;color:#8A5400}
.copy-icon{background:none;border:none;cursor:pointer;color:var(--steel);padding:2px;vertical-align:middle;line-height:1}
.copy-icon:hover{color:var(--black)}
.st-word{font-size:12px}
.model-table code{font-family:var(--mono);font-size:12px;background:var(--panel);padding:1px 6px}
.tag-cap{border-style:dashed;background:transparent}
.field-row{display:flex;gap:12px}
.field-row .field{flex:1}
select,input[type="text"]{width:100%;font-family:inherit}
.checkbox-group{margin-top:4px}
.checkbox-group label{display:flex;align-items:center;gap:8px;font-size:.82rem;font-weight:400;text-transform:none;letter-spacing:0;color:var(--text);padding:3px 0;cursor:pointer}
.checkbox-group input[type="checkbox"]{width:14px;height:14px;accent-color:var(--ink)}
.btn-primary:disabled{opacity:.4;cursor:not-allowed}
.btn-row{display:flex;gap:12px;align-items:center;margin-top:6px}
.output-area{display:none}
.output-area.visible{display:block}
.code-block{position:relative;background:var(--ink);color:#e8e8e8;border-radius:2px;padding:14px;font-family:var(--mono);font-size:.76rem;line-height:1.55;overflow-x:auto;white-space:pre;margin-top:8px}
.copy-btn{position:absolute;top:8px;right:8px;background:rgba(255,255,255,.12);color:#bdbdbd;border:none;border-radius:2px;padding:3px 10px;font-size:.64rem;font-weight:700;letter-spacing:.06em;text-transform:uppercase;cursor:pointer;font-family:inherit;transition:background .15s}
.copy-btn:hover{background:rgba(255,255,255,.25);color:#fff}
.file-path{display:inline-block;background:#f2f2f2;border:1px solid var(--border);padding:2px 8px;border-radius:2px;font-family:var(--mono);font-size:.76rem;color:var(--text);margin:4px 0}
.tabs{display:flex;border-bottom:1px solid var(--hairline);margin-bottom:12px;gap:4px}
.tab{padding:6px 12px;font-size:12px;font-weight:500;letter-spacing:.06em;text-transform:uppercase;color:var(--steel);cursor:pointer;border:none;background:none;font-family:inherit;border-bottom:2px solid transparent;margin-bottom:-1px}
.tab:hover{color:var(--black)}
.tab.active{color:var(--black);border-bottom-color:var(--black)}
.tab-content{display:none}
.tab-content.active{display:block}
.cmd-tab{display:none}
.cmd-tab.active{display:block}
.install-steps{font-size:.82rem;line-height:1.7}
.install-steps ol{padding-left:20px}
.install-steps li{margin-bottom:8px}
.install-steps code{background:#f2f2f2;border:1px solid var(--hairline);padding:1px 5px;border-radius:2px;font-family:var(--mono);font-size:.74rem}
.dl-btn{display:inline-flex;align-items:center;gap:5px;padding:5px 12px;font-size:.7rem;font-weight:700;letter-spacing:.04em;text-transform:uppercase;border:1px solid #c8c8c8;border-radius:2px;background:var(--surface);color:var(--text);cursor:pointer;font-family:inherit;transition:border-color .15s;text-decoration:none;margin-top:8px;margin-right:6px}
.dl-btn:hover{border-color:var(--ink)}
.dl-btn svg{width:13px;height:13px;fill:currentColor}
.hidden{display:none}
@media(max-width:600px){.field-row{flex-direction:column;gap:0}}
</style>
</head>
<body>

<div class="topbar">
  <div class="topbar-inner">
    <a class="brand" href="/">
      {{.Mark}}
      <span class="brand-name">EIDONIX</span><span class="brand-sub">LLM Proxy · Client Setup</span>
    </a>
    <div class="topbar-right">
      <a class="login-icon" href="/admin" title="Sign in" aria-label="Sign in"><svg viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><circle cx="12" cy="8" r="4"/><path d="M4 21c0-4 3.6-6.5 8-6.5s8 2.5 8 6.5"/></svg></a>
    </div>
  </div>
</div>

<div class="container">

  <!-- Models reference -->
  <div class="card">
    <div class="card-header"><h2>Available Models</h2><div class="card-tools"><input id="modelTableFilter" class="filter-input" type="search" placeholder="Filter models&hellip;" autocomplete="off"><button class="btn btn-primary" type="button" onclick="openGen()">Generate Config</button></div></div>
    <table class="model-table">
      <thead><tr><th>Model</th><th style="width:110px">Status</th><th style="width:100px">Protocol</th><th style="width:180px">URL</th><th style="width:110px">Context</th><th style="width:130px">Data Safety</th></tr></thead>
      <tbody id="modelTableBody"></tbody>
    </table>
    <div class="endpoints"><code>OpenAI&nbsp;&middot;&nbsp;<span id="epOai"></span></code> <code>Anthropic&nbsp;&middot;&nbsp;<span id="epAnt"></span></code></div>
  </div>


  <!-- Config generator (modal) -->
  <div id="genModal" class="modal-backdrop" onclick="if(event.target.id==='genModal')closeGen()">
    <div class="modal" style="max-width:900px" role="dialog">
      <div class="modal-header"><h2>Generate Configuration</h2><button class="modal-close" type="button" onclick="closeGen()">&times;</button></div>
      <div class="modal-body">

    <div class="field">
      <label for="harness">Coding Assistant</label>
      <select id="harness">
        <option value="">Select a coding assistant&hellip;</option>
        <option value="claude-code">Claude Code</option>
        <option value="codex">Codex</option>
        <option value="qwen-code">Qwen Code</option>
        <option value="opencode">OpenCode</option>
      </select>
    </div>

    <div class="field-row">
      <div class="field">
        <label for="apiKey">Proxy API Key</label>
        <input type="password" id="apiKey" placeholder="your-proxy-api-key" autocomplete="off">
      </div>
      <div class="field hidden" id="tavilyField">
        <label for="tavilyKey" id="tavilyLabel">Tavily API Key <span style="font-weight:400;text-transform:none">(optional &mdash; web search)</span></label>
        <input type="password" id="tavilyKey" placeholder="tvly-..." autocomplete="off">
        <div class="hint" id="tavilyHint"></div>
      </div>
    </div>

    <!-- Claude Code model selectors (Sonnet/Opus/Haiku) -->
    <div id="claudeSelectors" class="hidden">
      <div class="field-row">
        <div class="field">
          <label for="sonnetModel">Sonnet <span style="font-weight:400;text-transform:none">(default model)</span></label>
          <select id="sonnetModel"></select>
          <label style="display:inline-flex;align-items:center;gap:5px;margin-top:5px;font-size:.82rem;font-weight:400;text-transform:none;letter-spacing:0;cursor:pointer"><input type="checkbox" id="sonnetThinking" checked style="width:14px;height:14px;accent-color:var(--ink)"> Thinking</label>
        </div>
        <div class="field">
          <label for="opusModel">Opus <span style="font-weight:400;text-transform:none">(large model)</span></label>
          <select id="opusModel"></select>
          <label style="display:inline-flex;align-items:center;gap:5px;margin-top:5px;font-size:.82rem;font-weight:400;text-transform:none;letter-spacing:0;cursor:pointer"><input type="checkbox" id="opusThinking" checked style="width:14px;height:14px;accent-color:var(--ink)"> Thinking</label>
        </div>
        <div class="field">
          <label for="haikuModel">Haiku <span style="font-weight:400;text-transform:none">(fast model)</span></label>
          <select id="haikuModel"></select>
          <label style="display:inline-flex;align-items:center;gap:5px;margin-top:5px;font-size:.82rem;font-weight:400;text-transform:none;letter-spacing:0;cursor:pointer"><input type="checkbox" id="haikuThinking" style="width:14px;height:14px;accent-color:var(--ink)"> Thinking</label>
        </div>
      </div>
    </div>

    <!-- OpenCode model selectors (build/plan + model list) -->
    <div id="openCodeSelectors" class="hidden">
      <div class="field-row">
        <div class="field">
          <label for="buildModel">Build Agent <span style="font-weight:400;text-transform:none">(coding)</span></label>
          <select id="buildModel"></select>
        </div>
        <div class="field">
          <label for="planModel">Plan Agent <span style="font-weight:400;text-transform:none">(reasoning)</span></label>
          <select id="planModel"></select>
        </div>
      </div>
      <div class="field" style="margin-top:12px">
        <label>Available Models <span style="font-weight:400;text-transform:none">(included in config)</span></label>
        <div class="checkbox-group" id="ocAdditionalModels"></div>
      </div>
    </div>

    <!-- Multi-select (qwen-code) -->
    <div id="multiSelectors" class="hidden">
      <div class="field">
        <label>Default Model</label>
        <select id="defaultModel"></select>
      </div>
      <div class="field" style="margin-top:12px">
        <label>Additional Models <span style="font-weight:400;text-transform:none">(available via /model)</span></label>
        <div class="checkbox-group" id="additionalModels"></div>
      </div>
    </div>

    <!-- Codex model selectors -->
    <div id="codexSelectors" class="hidden">
      <div class="field-row">
        <div class="field">
          <label for="codexModel">Model</label>
          <select id="codexModel"></select>
        </div>
        <div class="field">
          <label for="codexEffort">Reasoning Effort</label>
          <select id="codexEffort">
            <option value="high">High</option>
            <option value="medium" selected>Medium</option>
            <option value="low">Low</option>
          </select>
        </div>
        <div class="field">
          <label for="codexCtxWindow">Context Window</label>
          <select id="codexCtxWindow">
            <option value="auto" selected>Auto-detect</option>
            <option value="8192">8K</option>
            <option value="16384">16K</option>
            <option value="32768">32K</option>
            <option value="65536">64K</option>
            <option value="131072">128K</option>
            <option value="196608">192K</option>
            <option value="262144">256K</option>
            <option value="524288">512K</option>
            <option value="1048576">1M</option>
          </select>
          <div class="hint" id="codexCtxHint"></div>
        </div>
      </div>
    </div>

    <!-- Output format (claude-code + codex) -->
    <div id="outputFormatField" class="hidden">
      <div class="field">
        <label>Output Format</label>
        <div style="display:flex;gap:8px;margin-top:4px">
          <label class="checkbox-group" style="padding:0;display:inline-flex;cursor:pointer">
            <input type="radio" name="outputFormat" value="config" checked style="width:16px;height:16px;accent-color:var(--ink)">
            <span style="font-size:.9rem">Configuration file</span>
          </label>
          <label class="checkbox-group" style="padding:0;display:inline-flex;cursor:pointer;margin-left:12px">
            <input type="radio" name="outputFormat" value="command" style="width:16px;height:16px;accent-color:var(--ink)">
            <span style="font-size:.9rem">Start command (shell script)</span>
          </label>
        </div>
      </div>
    </div>

    <div class="btn-row">
      <button class="btn btn-primary" id="generateBtn" disabled onclick="generate()">Generate Config</button>
    </div>

  <!-- Output -->
  <div class="output-area" id="outputArea">
    <div class="gen-section">
      <h3 id="configTitle">Configuration File</h3>
      <div id="configOutput"></div>
    </div>

    <div class="gen-section">
      <h3>Installation Instructions</h3>
      <div class="tabs" id="osTabs">
        <button class="tab active" data-os="macos" onclick="switchOS('macos')">macOS</button>
        <button class="tab" data-os="linux" onclick="switchOS('linux')">Linux</button>
        <button class="tab" data-os="windows" onclick="switchOS('windows')">Windows</button>
      </div>
      <div class="tab-content active" id="os-macos"></div>
      <div class="tab-content" id="os-linux"></div>
      <div class="tab-content" id="os-windows"></div>
    </div>
  </div>

      </div>
    </div>
  </div>

</div>

<script>
// Model data and proxy capabilities injected server-side.
var MODELS = {{.Models}};
var HAS_VISION = {{.HasVision}};
var HAS_WEB_SEARCH = {{.HasWebSearch}};
var HAS_MCP = {{.HasMCP}};
var HEALTH = {{.Health}};
var PROXY_ORIGIN = location.origin;
var PROXY_URL = PROXY_ORIGIN + "/v1";

// ---- Populate model overview table ----
(function(){
  var tbody = document.getElementById("modelTableBody");
  for (var i = 0; i < MODELS.length; i++) {
    var m = MODELS[i];
    var row = document.createElement("tr");
    var badges = "";
    if (m.supports_vision) badges += ' <span class="tag tag-cap">vision</span>';
    if (m.supports_audio) badges += ' <span class="tag tag-cap">audio</span>';
    var healthStatus = HEALTH[m.id] || { online: true, error: '' };
    var status = healthStatus.online
      ? '<span class="health-dot health-online"></span><span class="st-word">online</span>'
      : '<span class="health-dot health-offline"></span><span class="st-word" style="color:var(--red)" title="' + esc(healthStatus.error || '') + '">offline</span>';
    var path = m.protocol === "anthropic" ? "/anthropic" : "/v1";
    var fullURL = PROXY_ORIGIN + path;
    var urlCell = '<code>' + path + '</code> <button class="copy-icon" type="button" title="Copy ' + esc(fullURL) + '" onclick="copyText(\'' + esc(fullURL) + '\', this)"><svg viewBox="0 0 16 16" width="13" height="13" fill="none" stroke="currentColor" stroke-width="1.5"><rect x="5" y="5" width="9" height="9"/><path d="M11 5V2H2v9h3"/></svg></button>';
    var safety;
    if (m.local) {
      safety = '<span class="tag tag-local" title="Runs on your own infrastructure. No third party sees your data.">Local &middot; safe</span>';
    } else if (m.type === 'bedrock') {
      safety = '<span class="tag tag-bedrock" title="Hosted by AWS Bedrock. Model provider has no access to your data. AWS does not log or train on prompts.">Bedrock &middot; safe</span>';
    } else {
      safety = '<span class="tag tag-3p" title="Sent directly to the model provider. Subject to their terms of service.">3rd party</span>';
    }
    row.innerHTML = '<td><strong>' + esc(m.id) + '</strong>' + badges + '</td>' +
      '<td>' + status + '</td>' +
      '<td><span class="mono">' + m.protocol + '</span></td>' +
      '<td>' + urlCell + '</td>' +
      '<td>' + (m.context_window > 0 ? m.context_window.toLocaleString() : '<span class="mono">&mdash;</span>') + '</td>' +
      '<td>' + safety + '</td>';
    tbody.appendChild(row);
  }
})();

function esc(s){ var d=document.createElement("div"); d.textContent=s; return d.innerHTML; }
function copyText(txt, btn){
  function done(){ if(btn){ var t = btn.innerHTML; btn.innerHTML = "&check;"; setTimeout(function(){ btn.innerHTML = t; }, 1200); } }
  if(navigator.clipboard && navigator.clipboard.writeText){ navigator.clipboard.writeText(txt).then(done); return; }
  var ta = document.createElement("textarea");
  ta.value = txt; ta.style.position = "fixed"; ta.style.opacity = "0";
  document.body.appendChild(ta); ta.select();
  try { document.execCommand("copy"); done(); } finally { document.body.removeChild(ta); }
}
function openGen(){ document.getElementById("genModal").classList.add("open"); }
function closeGen(){ document.getElementById("genModal").classList.remove("open"); }
document.addEventListener("keydown", function(e){ if(e.key === "Escape") closeGen(); });
document.getElementById("epOai").textContent = PROXY_URL;
document.getElementById("epAnt").textContent = PROXY_ORIGIN + "/anthropic";
(function(){
  var inp = document.getElementById("modelTableFilter");
  inp.addEventListener("input", function(){
    var q = inp.value.toLowerCase();
    var rows = document.getElementById("modelTableBody").rows;
    for(var i=0;i<rows.length;i++){
      rows[i].style.display = (!q || rows[i].textContent.toLowerCase().indexOf(q) >= 0) ? "" : "none";
    }
  });
})();
function getModel(id){ for(var i=0;i<MODELS.length;i++) if(MODELS[i].id===id) return MODELS[i]; return null; }

// ---- Harness change ----
var harnessEl    = document.getElementById("harness");
var claudeSel    = document.getElementById("claudeSelectors");
var openCodeSel  = document.getElementById("openCodeSelectors");
var multiSel     = document.getElementById("multiSelectors");
var codexSel     = document.getElementById("codexSelectors");
var tavilyField  = document.getElementById("tavilyField");
var generateBtn  = document.getElementById("generateBtn");

harnessEl.addEventListener("change", function(){
  var h = this.value;
  claudeSel.classList.toggle("hidden", h!=="claude-code");
  openCodeSel.classList.toggle("hidden", h!=="opencode");
  multiSel.classList.toggle("hidden", h!=="qwen-code");
  codexSel.classList.toggle("hidden", h!=="codex");
  tavilyField.classList.toggle("hidden", !h);
  document.getElementById("outputFormatField").classList.toggle("hidden", h!=="claude-code" && h!=="codex");
  generateBtn.disabled = !h;
  document.getElementById("outputArea").classList.remove("visible");
  // Update search key field label, placeholder, and hint per client
  var label = document.getElementById("tavilyLabel");
  var input = document.getElementById("tavilyKey");
  var hint = document.getElementById("tavilyHint");
  if(h==="qwen-code"){
    label.innerHTML = 'Tavily API Key <span style="font-weight:400;text-transform:none">(optional &mdash; client-side web search)</span>';
    input.placeholder = "tvly-...";
    if(HAS_MCP){
      hint.textContent = "Proxy search will be added via MCP. Enter a Tavily key to also enable client-side search.";
      hint.style.color = "var(--text)";
    } else {
      hint.textContent = "Enter a Tavily key for client-side search, or configure web_search_key on the proxy.";
      hint.style.color = "var(--muted)";
    }
  } else if(h==="opencode"){
    label.innerHTML = 'Tavily API Key <span style="font-weight:400;text-transform:none">(optional &mdash; client-side web search)</span>';
    input.placeholder = "tvly-...";
    if(HAS_MCP){
      hint.textContent = "Proxy has web search configured. Optionally enter a Tavily key to use client-side search and disable proxy MCP search.";
      hint.style.color = "var(--text)";
    } else {
      hint.textContent = "Enter a Tavily key for client-side search, or configure web_search_key on the proxy (supports Tavily and Brave).";
      hint.style.color = "var(--muted)";
    }
  } else {
    label.innerHTML = 'Tavily API Key <span style="font-weight:400;text-transform:none">(optional &mdash; client-side web search)</span>';
    input.placeholder = "tvly-...";
    if(HAS_WEB_SEARCH){
      hint.textContent = "Proxy has web search configured. Enter a Tavily key to also enable client-side search with your own key.";
      hint.style.color = "var(--text)";
    } else {
      hint.textContent = "Enter a Tavily key for client-side search, or configure web_search_key on the proxy (supports Tavily and Brave).";
      hint.style.color = "var(--muted)";
    }
  }
  if(h==="claude-code") populateClaudeSelects();
  if(h==="codex") populateCodexSelects();
  if(h==="opencode") populateOpenCodeSelects();
  if(h==="qwen-code") populateMultiSelects();
});

function chatModels(){ return MODELS.filter(function(m){ return !m.id.toLowerCase().match(/embed/); }); }

function optionText(m){
  var tags=[];
  if(m.protocol==="anthropic") tags.push("Anthropic API");
  if(!m.local) tags.push("3rd party");
  var h=HEALTH[m.id];
  if(h && !h.online) tags.push("OFFLINE");
  return m.id + (tags.length ? "  ["+tags.join(", ")+"]" : "");
}


function populateSelects(ids, defaults){
  var cms = chatModels();
  ids.forEach(function(id){
    var sel=document.getElementById(id);
    sel.innerHTML="";
    cms.forEach(function(m){
      var o=document.createElement("option"); o.value=m.id; o.textContent=optionText(m);
      sel.appendChild(o);
    });
  });
  Object.keys(defaults).forEach(function(id){ setDefault(id,defaults[id]); });
}

function populateClaudeSelects(){
  populateSelects(["sonnetModel","opusModel","haikuModel"], {});
}

function populateCodexSelects(){
  var cms = chatModels().filter(function(m){ return m.protocol !== "anthropic"; });
  var sel = document.getElementById("codexModel");
  sel.innerHTML="";
  cms.forEach(function(m){
    var o=document.createElement("option"); o.value=m.id; o.textContent=optionText(m);
    sel.appendChild(o);
  });
  sel.addEventListener("change", updateCodexCtxHint);
  updateCodexCtxHint();
}

function updateCodexCtxHint(){
  var m = getModel(document.getElementById("codexModel").value);
  var hint = document.getElementById("codexCtxHint");
  var sel = document.getElementById("codexCtxWindow");
  var detected = m && m.context_window > 0;
  if(detected){
    var label = m.context_window >= 1024 ? Math.round(m.context_window/1024)+"K" : m.context_window;
    hint.textContent = "Detected: " + label + " tokens";
    hint.style.color = "";
    sel.style.borderColor = "";
  } else {
    hint.textContent = "Not detected \u2014 set manually for best results";
    hint.style.color = "var(--danger)";
    sel.style.borderColor = "var(--danger)";
  }
}

function getCodexCtxWindow(){
  var sel = document.getElementById("codexCtxWindow");
  if(sel.value === "auto"){
    var m = getModel(document.getElementById("codexModel").value);
    return (m && m.context_window > 0) ? m.context_window : 0;
  }
  return parseInt(sel.value, 10);
}

// Sync checkboxes: check+disable models selected in dropdowns
function syncCheckboxes(containerId, selectIds){
  var selected = {};
  selectIds.forEach(function(id){ selected[document.getElementById(id).value]=true; });
  var cbs = document.querySelectorAll("#"+containerId+" input[type=checkbox]");
  for(var i=0;i<cbs.length;i++){
    if(selected[cbs[i].value]){
      cbs[i].checked=true;
      cbs[i].disabled=true;
    } else {
      cbs[i].disabled=false;
    }
  }
}

function buildCheckboxGroup(containerId, selectIds){
  var container = document.getElementById(containerId);
  container.innerHTML="";
  chatModels().forEach(function(m){
    var label=document.createElement("label");
    var cb=document.createElement("input"); cb.type="checkbox"; cb.value=m.id; cb.checked=true;
    label.appendChild(cb);
    var safety;
    if (m.local) {
      safety = ' <span class="tag" style="margin-left:4px">Local</span>';
    } else if (m.type === 'bedrock') {
      safety = ' <span class="tag" style="margin-left:4px">Bedrock</span>';
    } else {
      safety = ' <span class="tag tag-warn" style="margin-left:4px">3rd party</span>';
    }
    var proto = m.protocol==="anthropic"
      ? ' <span class="tag tag-cap" style="margin-left:4px">Anthropic</span>' : '';
    var span=document.createElement("span");
    span.innerHTML = esc(m.id) + safety + proto;
    label.appendChild(span);
    container.appendChild(label);
  });
  var sync = function(){ syncCheckboxes(containerId, selectIds); };
  sync();
  selectIds.forEach(function(id){ document.getElementById(id).onchange = sync; });
}

function populateOpenCodeSelects(){
  populateSelects(["buildModel","planModel"], {});
  buildCheckboxGroup("ocAdditionalModels", ["buildModel","planModel"]);
}

function populateMultiSelects(){
  var cms = chatModels();
  var defSel = document.getElementById("defaultModel");
  defSel.innerHTML="";
  cms.forEach(function(m){
    var o=document.createElement("option"); o.value=m.id; o.textContent=optionText(m);
    defSel.appendChild(o);
  });
  buildCheckboxGroup("additionalModels", ["defaultModel"]);
}

function setDefault(id,val){ var s=document.getElementById(id); for(var i=0;i<s.options.length;i++) if(s.options[i].value===val){s.value=val;return;} }
function selectedAdditional(){ var r=[]; var cbs=document.querySelectorAll("#additionalModels input:checked"); for(var i=0;i<cbs.length;i++) r.push(cbs[i].value); return r; }

// ---- Generate ----
function getOutputFormat(){
  var r = document.querySelector('input[name="outputFormat"]:checked');
  return r ? r.value : "config";
}

function generate(){
  var harness = harnessEl.value;
  var apiKey = document.getElementById("apiKey").value.trim() || "<your-proxy-api-key>";
  var tavily = document.getElementById("tavilyKey").value.trim();
  var fmt = getOutputFormat();
  var result;
  switch(harness){
    case "claude-code":
      result = fmt==="command" ? genClaudeCodeCommand(apiKey,tavily) : genClaudeCode(apiKey,tavily);
      break;
    case "codex":
      result = fmt==="command" ? genCodexCommand(apiKey,tavily) : genCodex(apiKey,tavily);
      break;
    case "qwen-code":   result = genQwenCode(apiKey,tavily); break;
    case "opencode":    result = genOpenCode(apiKey,tavily); break;
  }
  if(result) renderOutput(result);
}

function renderOutput(r){
  var area = document.getElementById("outputArea");
  area.classList.add("visible");

  document.getElementById("configTitle").textContent = r.title || "Configuration File";
  var co = document.getElementById("configOutput");

  if(r.configTabs){
    // Tabbed code blocks (e.g. start command with macOS/Linux + PowerShell)
    var tabKeys = Object.keys(r.configTabs);
    var tabsHtml = '<div class="tabs" style="margin-bottom:0">';
    tabKeys.forEach(function(k,i){
      tabsHtml += '<button class="tab' + (i===0?' active':'') + '" onclick="switchCmdTab(this,\'' + i + '\')">' + esc(k) + '</button>';
    });
    tabsHtml += '</div>';
    tabKeys.forEach(function(k,i){
      tabsHtml += '<div class="cmd-tab' + (i===0?' active':'') + '" data-idx="' + i + '">' +
        '<div class="code-block"><button class="copy-btn" onclick="copyCode(this)">Copy</button>' + esc(r.configTabs[k]) + '</div></div>';
    });
    co.innerHTML = tabsHtml;
  } else {
    co.innerHTML = '<div style="margin-bottom:6px"><span class="file-path">' + esc(r.filename) + '</span></div>' +
      '<div class="code-block"><button class="copy-btn" onclick="copyCode(this)">Copy</button>' + esc(r.config) + '</div>';
  }

  // Download buttons (per-OS scripts)
  if(r.downloads){
    var dlHtml = '<div style="margin-top:10px">';
    r.downloads.forEach(function(d){
      dlHtml += '<a class="dl-btn" href="#" onclick="downloadFile(\'' + esc(d.name) + '\',this);return false" data-content="' +
        btoa(d.content) + '"><svg viewBox="0 0 16 16"><path d="M8 1v9m0 0L5 7m3 3l3-3M2 12v1a2 2 0 002 2h8a2 2 0 002-2v-1" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"/></svg>' +
        esc(d.label) + '</a>';
    });
    dlHtml += '</div>';
    co.innerHTML += dlHtml;
  }

  ["macos","linux","windows"].forEach(function(os){
    document.getElementById("os-"+os).innerHTML = '<div class="install-steps">' + r.install[os] + '</div>';
  });
  area.scrollIntoView({behavior:"smooth",block:"start"});
}

// ---- Claude Code ----
function thinkingCaps(checkboxId){
  return document.getElementById(checkboxId).checked ? "thinking,interleaved_thinking" : "";
}

// Shared env var builder for both config-file and start-command modes.
function claudeEnvVars(apiKey){
  var sonnetId = document.getElementById("sonnetModel").value;
  var opusId = document.getElementById("opusModel").value;
  var haikuId = document.getElementById("haikuModel").value;
  return [
    ["ANTHROPIC_BASE_URL", PROXY_ORIGIN],
    ["ANTHROPIC_API_KEY", apiKey],
    ["ANTHROPIC_DEFAULT_SONNET_MODEL", sonnetId],
    ["ANTHROPIC_DEFAULT_SONNET_MODEL_NAME", displayName(sonnetId)],
    ["ANTHROPIC_DEFAULT_SONNET_MODEL_SUPPORTED_CAPABILITIES", thinkingCaps("sonnetThinking")],
    ["ANTHROPIC_DEFAULT_OPUS_MODEL", opusId],
    ["ANTHROPIC_DEFAULT_OPUS_MODEL_NAME", displayName(opusId)],
    ["ANTHROPIC_DEFAULT_OPUS_MODEL_SUPPORTED_CAPABILITIES", thinkingCaps("opusThinking")],
    ["ANTHROPIC_DEFAULT_HAIKU_MODEL", haikuId],
    ["ANTHROPIC_DEFAULT_HAIKU_MODEL_NAME", displayName(haikuId)],
    ["ANTHROPIC_DEFAULT_HAIKU_MODEL_SUPPORTED_CAPABILITIES", thinkingCaps("haikuThinking")],
    ["DISABLE_PROMPT_CACHING", "1"],
    ["CLAUDE_CODE_DISABLE_1M_CONTEXT", "1"],
    ["CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "1"],
    ["API_TIMEOUT_MS", "900000"]
  ];
}

function genClaudeCode(apiKey, tavily){
  var env = {};
  claudeEnvVars(apiKey).forEach(function(kv){ env[kv[0]]=kv[1]; });

  var settings = {
    attribution: { commit: "", pr: "" },
    env: env
  };

  var fn = "settings.json";
  // Add client-side Tavily MCP when user enters a key.
  var useTavily = !!tavily;
  var tavilyJSON = useTavily ? JSON.stringify({type:"http",url:"https://mcp.tavily.com/mcp",headers:{"Authorization":"Bearer "+tavily}}) : "";
  var tavilyStep = useTavily
    ? 'Install Tavily web search:<br><code>claude mcp remove tavily -s user 2&gt;/dev/null; claude mcp add-json tavily \'' + esc(tavilyJSON) + '\' -s user</code>'
    : "";
  var tavilyStepWin = useTavily
    ? 'Install Tavily web search:<br><code>claude mcp remove tavily -s user 2>nul & claude mcp add-json tavily "' + esc(tavilyJSON.replace(/"/g,'\\"')) + '" -s user</code>'
    : "";

  var macLinuxSteps = [
    'Create the config directory:<br><code>mkdir -p ~/.claude</code>',
    'Save the generated file as:<br><code>~/.claude/settings.json</code>'
  ];
  if(tavilyStep) macLinuxSteps.push(tavilyStep);
  if(HAS_WEB_SEARCH) macLinuxSteps.push('Web search is handled by the proxy &mdash; no client-side MCP needed.');
  macLinuxSteps.push('Restart Claude Code for changes to take effect.');

  var winSteps = [
    'Create the config directory:<br><code>mkdir %USERPROFILE%\\.claude</code>',
    'Save the generated file as:<br><code>%USERPROFILE%\\.claude\\settings.json</code>'
  ];
  if(tavilyStepWin) winSteps.push(tavilyStepWin);
  if(HAS_WEB_SEARCH) winSteps.push('Web search is handled by the proxy &mdash; no client-side MCP needed.');
  winSteps.push('Restart Claude Code for changes to take effect.');

  return {
    config: JSON.stringify(settings, null, 2),
    filename: fn,
    install: {
      macos: ol(macLinuxSteps),
      linux: ol(macLinuxSteps),
      windows: ol(winSteps)
    }
  };
}

// ---- Claude Code (start command) ----
function genClaudeCodeCommand(apiKey, tavily){
  var vars = claudeEnvVars(apiKey);
  var settingsJSON = JSON.stringify({attribution:{commit:"",pr:""}});

  // Add client-side Tavily MCP when user enters a key.
  var useTavily = !!tavily;
  var tavilyMcpJSON = useTavily ? JSON.stringify({type:"http",url:"https://mcp.tavily.com/mcp",headers:{"Authorization":"Bearer "+tavily}}) : "";

  var shLines = ["#!/usr/bin/env bash", "# go-llm-proxy: Claude Code start script", ""];
  if(useTavily){
    shLines.push("# Configure Tavily web search");
    shLines.push("claude mcp remove tavily -s user 2>/dev/null");
    shLines.push("claude mcp add-json tavily '" + tavilyMcpJSON + "' -s user");
    shLines.push("");
  }
  shLines.push("env \\");
  vars.forEach(function(kv){ shLines.push('  ' + kv[0] + '="' + kv[1] + '" \\'); });
  shLines.push("  claude --settings '" + settingsJSON + "' \"$@\"");
  var shContent = shLines.join("\n") + "\n";

  var batLines = ["@echo off", "setlocal", "REM go-llm-proxy: Claude Code start script", ""];
  vars.forEach(function(kv){ batLines.push("set " + kv[0] + "=" + kv[1]); });
  if(useTavily){
    batLines.push("", "REM Configure Tavily web search");
    batLines.push('claude mcp remove tavily -s user 2>nul');
    batLines.push('claude mcp add-json tavily "' + tavilyMcpJSON.replace(/"/g,'\\"') + '" -s user');
  }
  batLines.push("", 'claude --settings "' + settingsJSON.replace(/"/g, '\\"') + '" %*');
  var batContent = batLines.join("\r\n") + "\r\n";

  var ps1Lines = ["# go-llm-proxy: Claude Code start script", ""];
  vars.forEach(function(kv){ ps1Lines.push('$env:' + kv[0] + ' = "' + kv[1] + '"'); });
  if(useTavily){
    ps1Lines.push("", "# Configure Tavily web search in .claude.json");
    ps1Lines.push('$configPath = "$env:USERPROFILE\\.claude.json"');
    ps1Lines.push("if (Test-Path $configPath) {");
    ps1Lines.push("  $config = Get-Content $configPath -Raw | ConvertFrom-Json");
    ps1Lines.push("} else {");
    ps1Lines.push("  $config = '{}' | ConvertFrom-Json");
    ps1Lines.push("}");
    ps1Lines.push("$tavilyObj = @{");
    ps1Lines.push('  type = "http"');
    ps1Lines.push('  url = "https://mcp.tavily.com/mcp"');
    ps1Lines.push("  headers = @{");
    ps1Lines.push('    Authorization = "Bearer ' + tavily + '"');
    ps1Lines.push("  }");
    ps1Lines.push("}");
    ps1Lines.push('if (-not $config.mcpServers) {');
    ps1Lines.push('  $config | Add-Member -NotePropertyName "mcpServers" -NotePropertyValue @{} -Force');
    ps1Lines.push("}");
    ps1Lines.push('$config.mcpServers | Add-Member -NotePropertyName "tavily" -NotePropertyValue $tavilyObj -Force');
    ps1Lines.push('$config | ConvertTo-Json -Depth 10 | Set-Content $configPath -Encoding UTF8');
    ps1Lines.push('Write-Host "Tavily MCP configured in .claude.json"');
  }
  // Write settings JSON to a temp file to avoid PowerShell quoting issues
  // with inline JSON containing curly braces and colons.
  ps1Lines.push("", "$settingsFile = [System.IO.Path]::GetTempFileName()");
  ps1Lines.push("'" + settingsJSON + "' | Set-Content -Path $settingsFile -Encoding UTF8");
  ps1Lines.push("try {");
  ps1Lines.push("  claude --settings $settingsFile @args");
  ps1Lines.push("} finally {");
  ps1Lines.push("  Remove-Item $settingsFile -ErrorAction SilentlyContinue");
  vars.forEach(function(kv){ ps1Lines.push('  Remove-Item Env:' + kv[0] + ' -ErrorAction SilentlyContinue'); });
  ps1Lines.push("}");
  var ps1Content = ps1Lines.join("\r\n") + "\r\n";

  var shDisplay = shLines.slice(2).join("\n").trim();
  var ps1Display = ps1Lines.slice(2).join("\n").trim();

  return {
    title: "Start Command",
    configTabs: {
      "macOS / Linux": shDisplay,
      "PowerShell": ps1Display
    },
    downloads: [
      { name: "claude-proxy.sh", label: "Download .sh (macOS/Linux)", content: shContent },
      { name: "claude-proxy.bat", label: "Download .bat (Windows)", content: batContent },
      { name: "claude-proxy.ps1", label: "Download .ps1 (PowerShell)", content: ps1Content }
    ],
    install: {
      macos: ol([
        'Download <code>claude-proxy.sh</code> using the button above.',
        'Make it executable:<br><code>chmod +x claude-proxy.sh</code>',
        'Run it:<br><code>./claude-proxy.sh</code>',
        'Optional: move it to your PATH for easy access:<br><code>mv claude-proxy.sh /usr/local/bin/claude-proxy</code>',
        'Then launch from anywhere:<br><code>claude-proxy</code>'
      ]),
      linux: ol([
        'Download <code>claude-proxy.sh</code> using the button above.',
        'Make it executable:<br><code>chmod +x claude-proxy.sh</code>',
        'Run it:<br><code>./claude-proxy.sh</code>',
        'Optional: move it to your PATH for easy access:<br><code>mv claude-proxy.sh ~/.local/bin/claude-proxy</code>',
        'Then launch from anywhere:<br><code>claude-proxy</code>'
      ]),
      windows: ol([
        'Download <code>claude-proxy.bat</code> or <code>claude-proxy.ps1</code> using the buttons above.',
        'For <strong>.bat</strong>: Double-click the file, or run from Command Prompt:<br><code>claude-proxy.bat</code>',
        'For <strong>.ps1</strong>: Run from PowerShell:<br><code>.\\claude-proxy.ps1</code>',
        'Optional: move the script to a folder in your PATH for easy access.'
      ])
    }
  };
}

// ---- Codex ----
function codexToml(modelId, effort, apiKey, tavily){
  var ctxWindow = getCodexCtxWindow();
  var toml = 'model = "' + modelId + '"\n' +
    'model_provider = "go-llm-proxy"\n' +
    'model_reasoning_effort = "' + effort + '"\n';
  // Disable built-in web_search when proxy doesn't handle it (client MCP or no search).
  // When proxy has search, leave it enabled so proxy-side search works.
  if(!HAS_WEB_SEARCH){
    toml += 'web_search = "disabled"\n';
  }
  if(ctxWindow > 0){
    toml += 'model_context_window = ' + ctxWindow + '\n';
  }
  toml += '\n[model_providers.go-llm-proxy]\n' +
    'name = "Go-LLM-Proxy"\n' +
    'base_url = "' + PROXY_URL + '"\n' +
    'wire_api = "responses"\n' +
    '# API key embedded directly (no env var needed):\n' +
    'experimental_bearer_token = "' + apiKey + '"\n' +
    '# Or use an environment variable instead:\n' +
    '# env_key = "OPENAI_API_KEY"\n';

  // Add client-side Tavily MCP when user enters a key.
  if(tavily){
    toml += '\n[mcp_servers.tavily]\n' +
      'url = "https://mcp.tavily.com/mcp"\n' +
      '# Tavily key embedded directly:\n' +
      'http_headers = { Authorization = "Bearer ' + tavily + '" }\n' +
      '# Or use an environment variable instead:\n' +
      '# bearer_token_env_var = "TAVILY_API_KEY"\n';
  }
  return toml;
}

function genCodex(apiKey, tavily){
  var modelId = document.getElementById("codexModel").value;
  var effort = document.getElementById("codexEffort").value;
  var toml = codexToml(modelId, effort, apiKey, tavily);

  var searchNote = HAS_WEB_SEARCH ? ' Web search is handled by the proxy automatically.' : '';

  var unixSteps = [
    'Create the config directory:<br><code>mkdir -p ~/.codex</code>',
    'Save the generated file as:<br><code>~/.codex/config.toml</code>',
    'The API key' + (tavily ? 's are' : ' is') + ' embedded in the config. To use environment variables instead, ' +
      'edit the file and swap the commented lines, then set:<br>' +
      '<code>export OPENAI_API_KEY=' + esc(apiKey) + '</code>' +
      (tavily ? '<br><code>export TAVILY_API_KEY=' + esc(tavily) + '</code>' : ''),
    'Restart Codex for changes to take effect.' + searchNote
  ];

  var winSteps = [
    'Create the config directory:<br><code>mkdir %USERPROFILE%\\.codex</code>',
    'Save the generated file as:<br><code>%USERPROFILE%\\.codex\\config.toml</code>',
    'The API key' + (tavily ? 's are' : ' is') + ' embedded in the config. To use environment variables instead, ' +
      'edit the file and swap the commented lines, then set:<br>' +
      '<code>setx OPENAI_API_KEY ' + esc(apiKey) + '</code>' +
      (tavily ? '<br><code>setx TAVILY_API_KEY ' + esc(tavily) + '</code>' : ''),
    'Restart Codex for changes to take effect.' + searchNote
  ];

  return {
    config: toml,
    filename: "config.toml",
    install: {
      macos: ol(unixSteps),
      linux: ol(unixSteps),
      windows: ol(winSteps)
    }
  };
}

function genCodexCommand(apiKey, tavily){
  var modelId = document.getElementById("codexModel").value;
  var effort = document.getElementById("codexEffort").value;
  var useTavily = !!tavily;

  var ctxWindow = getCodexCtxWindow();
  var cfgFlags = [
    '-c \'model="' + modelId + '"\'',
    '-c \'model_provider="go-llm-proxy"\'',
    '-c \'model_reasoning_effort="' + effort + '"\''
  ];
  if(!HAS_WEB_SEARCH){
    cfgFlags.push('-c \'web_search="disabled"\'');
  }
  if(ctxWindow > 0){
    cfgFlags.push('-c \'model_context_window=' + ctxWindow + '\'');
  }
  cfgFlags.push(
    '-c \'model_providers.go-llm-proxy.name="Go-LLM-Proxy"\'',
    '-c \'model_providers.go-llm-proxy.base_url="' + PROXY_URL + '"\'',
    '-c \'model_providers.go-llm-proxy.env_key="OPENAI_API_KEY"\'',
    '-c \'model_providers.go-llm-proxy.wire_api="responses"\''
  );
  if(useTavily){
    cfgFlags.push('-c \'mcp_servers.tavily.url="https://mcp.tavily.com/mcp"\'');
    cfgFlags.push('-c \'mcp_servers.tavily.bearer_token_env_var="TAVILY_API_KEY"\'');
  }

  // Build PowerShell array entries
  var ps1FlagPairs = cfgFlags.map(function(f){
    var val = f.replace(/^-c '/,"").replace(/'$/,"");
    return "  '-c', '" + val + "'";
  });

  var shLines = [
    "#!/usr/bin/env bash",
    "# go-llm-proxy: Codex start script",
    "",
    'export OPENAI_API_KEY="' + apiKey + '"'
  ];
  if(useTavily) shLines.push('export TAVILY_API_KEY="' + tavily + '"');
  shLines.push("");
  shLines.push("codex \\");
  cfgFlags.forEach(function(f){
    shLines.push("  " + f + " \\");
  });
  shLines.push('  "$@"');
  var shContent = shLines.join("\n") + "\n";

  var ps1Lines = [
    "# go-llm-proxy: Codex start script",
    "",
    '$env:OPENAI_API_KEY = "' + apiKey + '"'
  ];
  if(useTavily) ps1Lines.push('$env:TAVILY_API_KEY = "' + tavily + '"');
  ps1Lines.push("");
  ps1Lines.push("# Build argument list.");
  ps1Lines.push("$codexArgs = @(");
  ps1FlagPairs.forEach(function(p){ ps1Lines.push(p); });
  ps1Lines.push(") + $args");
  ps1Lines.push("");
  ps1Lines.push("try {");
  ps1Lines.push("  codex @codexArgs");
  ps1Lines.push("} finally {");
  ps1Lines.push("  Remove-Item Env:OPENAI_API_KEY -ErrorAction SilentlyContinue");
  if(useTavily) ps1Lines.push("  Remove-Item Env:TAVILY_API_KEY -ErrorAction SilentlyContinue");
  ps1Lines.push("}");
  var ps1Content = ps1Lines.join("\r\n") + "\r\n";

  var shDisplay = shLines.slice(2).join("\n").trim();
  var ps1Display = ps1Lines.slice(2).join("\n").trim();

  return {
    title: "Start Command",
    configTabs: {
      "macOS / Linux": shDisplay,
      "PowerShell": ps1Display
    },
    downloads: [
      { name: "codex-proxy.sh", label: "Download .sh (macOS/Linux)", content: shContent },
      { name: "codex-proxy.ps1", label: "Download .ps1 (PowerShell)", content: ps1Content }
    ],
    install: {
      macos: ol([
        'Download <code>codex-proxy.sh</code> using the button above.',
        'Make it executable:<br><code>chmod +x codex-proxy.sh</code>',
        'Run it:<br><code>./codex-proxy.sh</code>',
        'Optional: move it to your PATH:<br><code>mv codex-proxy.sh /usr/local/bin/codex-proxy</code>'
      ]),
      linux: ol([
        'Download <code>codex-proxy.sh</code> using the button above.',
        'Make it executable:<br><code>chmod +x codex-proxy.sh</code>',
        'Run it:<br><code>./codex-proxy.sh</code>',
        'Optional: move it to your PATH:<br><code>mv codex-proxy.sh ~/.local/bin/codex-proxy</code>'
      ]),
      windows: ol([
        'Download <code>codex-proxy.ps1</code> using the button above.',
        'Run from PowerShell:<br><code>.\\codex-proxy.ps1</code>',
        'Optional: move the script to a folder in your PATH.'
      ])
    }
  };
}

// ---- Qwen Code ----
function genQwenCode(apiKey, tavily){
  var defModel = document.getElementById("defaultModel").value;
  var additional = selectedAdditional();
  var defInfo = getModel(defModel);

  var envKeyName = "PROXY_API_KEY";
  var oaiModels = [];
  var antModels = [];
  additional.forEach(function(id){
    var m = getModel(id);
    if(!m) return;
    var entry = {
      id: id,
      name: displayName(id),
      envKey: envKeyName,
      baseUrl: m.protocol==="anthropic" ? (PROXY_ORIGIN+"/anthropic") : PROXY_URL,
      generationConfig: { timeout: 300000, maxRetries: 1 }
    };
    if(m.protocol==="anthropic") antModels.push(entry);
    else oaiModels.push(entry);
  });

  var mp = {};
  if(oaiModels.length) mp.openai = oaiModels;
  if(antModels.length) mp.anthropic = antModels;

  var authType = defInfo && defInfo.protocol==="anthropic" ? "anthropic" : "openai";

  var obj = {
    "$version": 3,
    model: { name: defModel },
    security: { auth: { selectedType: authType } },
    modelProviders: mp,
    env: {}
  };

  obj.env[envKeyName] = apiKey;

  // Search: prefer proxy MCP endpoint, fall back to client-side Tavily.
  if(HAS_MCP){
    obj.mcpServers = {
      "proxy-search": {
        url: PROXY_ORIGIN + "/mcp/sse",
        headers: { "Authorization": "Bearer " + apiKey }
      }
    };
    // Also add client-side Tavily if user entered a key (gives model two search options).
    if(tavily){
      obj.webSearch = {
        provider: [{ type: "tavily", apiKey: tavily }],
        "default": "tavily"
      };
    }
  } else if(tavily){
    obj.webSearch = {
      provider: [{ type: "tavily", apiKey: tavily }],
      "default": "tavily"
    };
  }

  var unixSteps = ol([
    'Create the config directory:<br><code>mkdir -p ~/.qwen</code>',
    'Save the generated file as:<br><code>~/.qwen/settings.json</code>',
    'Launch Qwen Code. Use <code>/model</code> to switch between models.'
  ]);

  return {
    config: JSON.stringify(obj, null, 2),
    filename: "settings.json",
    install: {
      macos: unixSteps,
      linux: unixSteps,
      windows: ol([
        'Create the config directory:<br><code>mkdir %USERPROFILE%\\.qwen</code>',
        'Save the generated file as:<br><code>%USERPROFILE%\\.qwen\\settings.json</code>',
        'Launch Qwen Code. Use <code>/model</code> to switch between models.'
      ])
    }
  };
}

// ---- OpenCode ----
function genOpenCode(apiKey, tavily){
  var agentId = document.getElementById("buildModel").value;
  var plannerId = document.getElementById("planModel").value;

  var selectedOC = {};
  var cbs = document.querySelectorAll("#ocAdditionalModels input:checked");
  for(var i=0;i<cbs.length;i++) selectedOC[cbs[i].value]=true;
  selectedOC[agentId]=true;
  selectedOC[plannerId]=true;

  var oaiModels = chatModels().filter(function(m){ return selectedOC[m.id] && m.protocol!=="anthropic"; });
  var antModels = chatModels().filter(function(m){ return selectedOC[m.id] && m.protocol==="anthropic"; });

  var oaiModelsObj = {};
  oaiModels.forEach(function(m){ oaiModelsObj[m.id] = { name: displayName(m.id) }; });

  var antModelsObj = {};
  antModels.forEach(function(m){ antModelsObj[m.id] = { name: displayName(m.id) }; });

  var providers = {};
  if(oaiModels.length){
    providers["go-llm-proxy"] = {
      npm: "@ai-sdk/openai-compatible",
      name: "go-llm-proxy (OpenAI)",
      options: { baseURL: PROXY_URL, apiKey: apiKey },
      models: oaiModelsObj
    };
  }
  if(antModels.length){
    providers["go-llm-proxy-ant"] = {
      npm: "@ai-sdk/anthropic",
      name: "go-llm-proxy (Anthropic)",
      options: { baseURL: PROXY_ORIGIN + "/anthropic/v1", apiKey: apiKey },
      models: antModelsObj
    };
  }

  function ocModel(id){
    var m = getModel(id);
    if(m && m.protocol==="anthropic") return "go-llm-proxy-ant/" + id;
    return "go-llm-proxy/" + id;
  }

  var obj = {
    "$schema": "https://opencode.ai/config.json",
    provider: providers,
    model: ocModel(agentId),
    small_model: ocModel(agentId),
    agent: {
      build: { model: ocModel(agentId), description: "Coding agent" },
      plan:  { model: ocModel(plannerId), description: "Planning agent" }
    }
  };

  // Add proxy MCP and/or client-side Tavily for search
  var mcpObj = {};
  if(HAS_MCP){
    mcpObj["proxy-search"] = {
      type: "remote",
      url: PROXY_ORIGIN + "/mcp/sse",
      headers: { "Authorization": "Bearer " + apiKey },
      enabled: tavily ? false : true
    };
  }
  if(tavily){
    mcpObj["tavily"] = {
      type: "remote",
      url: "https://mcp.tavily.com/mcp",
      headers: { "Authorization": "Bearer " + tavily },
      enabled: true
    };
  }
  if(Object.keys(mcpObj).length) obj.mcp = mcpObj;

  var unixSteps = ol([
    'Save <code>opencode.json</code> to your project root, or globally:<br><code>mkdir -p ~/.config/opencode &amp;&amp; cp opencode.json ~/.config/opencode/opencode.json</code>',
    'Launch OpenCode from your project directory.'
  ]);

  return {
    config: JSON.stringify(obj, null, 2),
    filename: "opencode.json",
    install: {
      macos: unixSteps,
      linux: unixSteps,
      windows: ol([
        'Save <code>opencode.json</code> to your project root, or globally. The correct location depends on which OpenCode you run:',
        '<b>TUI (terminal):</b> <code>%APPDATA%\\opencode\\opencode.json</code>',
        '<b>GUI (desktop app):</b> <code>%USERPROFILE%\\.config\\opencode\\opencode.json</code>',
        'Launch OpenCode from your project directory.'
      ])
    }
  };
}

// ---- Helpers ----
function displayName(id){
  return {"MiniMax-M2.5":"MiniMax M2.5","MiniMax-M2.7":"MiniMax M2.7","qwen-3.5":"Qwen 3.5",
    "glm-5.1":"GLM 5.1","glm-4.7":"GLM 4.7","Nemotron-3-Super":"Nemotron 3 Super","nomic-embed":"Nomic Embed"}[id]||id;
}

function ol(items){ return "<ol>"+items.map(function(i){ return "<li>"+i+"</li>"; }).join("")+"</ol>"; }

function downloadFile(name, el){
  var raw = atob(el.dataset.content);
  var bytes = new Uint8Array(raw.length);
  for(var i=0;i<raw.length;i++) bytes[i]=raw.charCodeAt(i);
  var blob = new Blob([bytes], {type:"application/octet-stream"});
  var url = URL.createObjectURL(blob);
  var a = document.createElement("a");
  a.href = url; a.download = name; a.click();
  URL.revokeObjectURL(url);
}

function copyCode(btn){
  var text = btn.parentElement.textContent.replace(/^Copy/,"").trim();
  if(navigator.clipboard){
    navigator.clipboard.writeText(text).then(function(){
      btn.textContent="Copied!"; setTimeout(function(){btn.textContent="Copy";},1500);
    });
  } else {
    var ta=document.createElement("textarea");
    ta.value=text; ta.style.position="fixed"; ta.style.opacity="0";
    document.body.appendChild(ta); ta.select();
    document.execCommand("copy");
    document.body.removeChild(ta);
    btn.textContent="Copied!"; setTimeout(function(){btn.textContent="Copy";},1500);
  }
}

function switchCmdTab(btn, idx){
  var parent = btn.parentElement.parentElement;
  var tabs = parent.querySelectorAll(".tabs .tab");
  for(var i=0;i<tabs.length;i++) tabs[i].classList.remove("active");
  btn.classList.add("active");
  var ct = parent.querySelectorAll(".cmd-tab");
  for(var i=0;i<ct.length;i++) ct[i].classList.toggle("active",ct[i].dataset.idx===idx);
}

function switchOS(os){
  var tabs = document.querySelectorAll("#osTabs .tab");
  for(var i=0;i<tabs.length;i++) tabs[i].classList.toggle("active",tabs[i].dataset.os===os);
  ["macos","linux","windows"].forEach(function(o){ document.getElementById("os-"+o).classList.toggle("active",o===os); });
}

</script>
</body>
</html>`
