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
	ContextWindow  int    `json:"context_window"`   // max tokens (0 = unknown)
	SupportsVision bool   `json:"supports_vision"`  // model handles images natively
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
		out = append(out, modelInfo{ID: m.Name, Local: local, Protocol: proto, ContextWindow: m.ContextWindow, SupportsVision: m.SupportsVision})
	}
	return out
}

// ConfigPageHandler serves the config generator UI at GET /.
type ConfigPageHandler struct {
	config *config.ConfigStore
	tmpl   *template.Template
}

func NewConfigPageHandler(cs *config.ConfigStore) *ConfigPageHandler {
	return &ConfigPageHandler{
		config: cs,
		tmpl:   template.Must(template.New("page").Parse(configPageHTML)),
	}
}

func (h *ConfigPageHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	cfg := h.config.Get()
	models := modelInfoFromConfig(cfg)

	data, err := json.Marshal(models)
	if err != nil {
		slog.Error("failed to marshal model info", "error", err)
		httputil.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}

	// Pass processors config as separate JS variables.
	type tmplData struct {
		Models       template.JS
		HasVision    bool
		HasWebSearch bool
		HasMCP       bool
	}
	td := tmplData{
		Models:       template.JS(data),
		HasVision:    cfg.Processors.Vision != "",
		HasWebSearch: cfg.Processors.WebSearchKey != "",
		HasMCP:       cfg.Processors.WebSearchKey != "",
	}

	var buf bytes.Buffer
	if err := h.tmpl.Execute(&buf, td); err != nil {
		slog.Error("failed to render config page", "error", err)
		httputil.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}

	httputil.SetSecurityHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; frame-ancestors 'none'")
	if _, err := buf.WriteTo(w); err != nil {
		slog.Error("failed to write config page response", "error", err)
	}
}

const configPageHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Go-LLM-Proxy Config Generator</title>
<style>
*,*::before,*::after{box-sizing:border-box;margin:0;padding:0}
:root{
  --bg:#f4f6f9;--surface:#fff;--border:#d8dde5;
  --text:#1b2033;--muted:#5c6377;
  --blue:#1a56db;--blue-hover:#1648b8;--blue-light:#e8effc;
  --green:#047857;--green-bg:#ecfdf5;
  --amber:#b45309;--amber-bg:#fffbeb;
  --indigo:#4338ca;--indigo-bg:#eef2ff;
  --slate-bg:#0f172a;--slate-text:#e2e8f0;--slate-muted:#94a3b8;
  --radius:8px;
  --shadow:0 1px 3px rgba(0,0,0,.07),0 1px 2px rgba(0,0,0,.05);
}
body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,Helvetica,Arial,sans-serif;background:var(--bg);color:var(--text);line-height:1.6;min-height:100vh}

/* ---- Header ---- */
.header{background:var(--slate-bg);color:#f1f5f9;padding:28px 0;text-align:center}
.header-inner{display:flex;align-items:center;justify-content:center;gap:16px;flex-wrap:wrap}
.header-logo{height:56px;width:auto;filter:brightness(0) invert(1);opacity:.92}
.header-text h1{font-size:1.6rem;font-weight:700;letter-spacing:-.02em}
.header-text p{color:var(--slate-muted);font-size:.9rem;margin-top:2px}

/* ---- Layout ---- */
.container{max-width:840px;margin:0 auto;padding:28px 20px 60px}
.card{background:var(--surface);border:1px solid var(--border);border-radius:var(--radius);padding:24px;margin-bottom:22px;box-shadow:var(--shadow)}
.card h2{font-size:1.05rem;font-weight:600;margin-bottom:14px;padding-bottom:10px;border-bottom:1px solid var(--border)}

/* ---- Form ---- */
label{display:block;font-size:.78rem;font-weight:600;color:var(--muted);margin-bottom:3px;text-transform:uppercase;letter-spacing:.04em}
select,input[type="text"],input[type="password"]{width:100%;padding:9px 11px;font-size:.92rem;border:1px solid var(--border);border-radius:6px;background:var(--surface);color:var(--text);transition:border-color .15s;font-family:inherit}
select:focus,input:focus{outline:none;border-color:var(--blue);box-shadow:0 0 0 3px rgba(26,86,219,.1)}
.field{margin-bottom:14px}
.field-row{display:flex;gap:14px}
.field-row .field{flex:1}
.hint{font-size:.78rem;color:var(--muted);margin-top:2px}

/* ---- Table ---- */
.model-table{width:100%;border-collapse:collapse;font-size:.88rem;margin-top:6px}
.model-table th{text-align:left;font-size:.72rem;font-weight:600;text-transform:uppercase;letter-spacing:.05em;color:var(--muted);padding:7px 10px;border-bottom:2px solid var(--border)}
.model-table td{padding:9px 10px;border-bottom:1px solid var(--border);vertical-align:middle}
.model-table tr:last-child td{border-bottom:none}
.model-table tr:hover td{background:#f9fafb}

/* ---- Badges ---- */
.badge{display:inline-block;font-size:.72rem;font-weight:600;padding:2px 8px;border-radius:99px;white-space:nowrap}
.badge-safe{background:var(--green-bg);color:var(--green)}
.badge-warn{background:var(--amber-bg);color:var(--amber)}
.badge-proto-oai{background:#f0fdf4;color:#166534}
.badge-proto-ant{background:var(--indigo-bg);color:var(--indigo)}
.badge-vision{background:#dbeafe;color:#1e40af}

/* ---- Checkboxes ---- */
.checkbox-group{margin-top:6px}
.checkbox-group label{display:flex;align-items:center;gap:8px;font-size:.88rem;font-weight:400;text-transform:none;letter-spacing:0;color:var(--text);padding:5px 0;cursor:pointer}
.checkbox-group input[type="checkbox"]{width:16px;height:16px;accent-color:var(--blue)}

/* ---- Buttons ---- */
.btn{display:inline-flex;align-items:center;gap:6px;padding:10px 22px;font-size:.92rem;font-weight:600;border:none;border-radius:6px;cursor:pointer;transition:background .15s,transform .1s;font-family:inherit}
.btn:active{transform:scale(.98)}
.btn-primary{background:var(--blue);color:#fff}
.btn-primary:hover{background:var(--blue-hover)}
.btn-primary:disabled{opacity:.5;cursor:not-allowed}
.btn-row{display:flex;gap:12px;align-items:center;margin-top:6px}

/* ---- Output ---- */
.output-area{display:none}
.output-area.visible{display:block}
.code-block{position:relative;background:var(--slate-bg);color:var(--slate-text);border-radius:6px;padding:16px 16px 16px 16px;font-family:"SF Mono","Cascadia Code","Fira Code",Consolas,monospace;font-size:.8rem;line-height:1.55;overflow-x:auto;white-space:pre;margin-top:8px}
.copy-btn{position:absolute;top:8px;right:8px;background:rgba(255,255,255,.1);color:var(--slate-muted);border:none;border-radius:4px;padding:4px 10px;font-size:.72rem;cursor:pointer;font-family:inherit;transition:background .15s}
.copy-btn:hover{background:rgba(255,255,255,.2);color:#e2e8f0}
.file-path{display:inline-block;background:#f1f5f9;padding:2px 10px;border-radius:4px;font-family:"SF Mono",Consolas,monospace;font-size:.8rem;color:var(--text);margin:4px 0}

/* ---- Tabs ---- */
.tabs{display:flex;border-bottom:2px solid var(--border);margin-bottom:14px;gap:0}
.tab{padding:7px 18px;font-size:.88rem;font-weight:500;color:var(--muted);cursor:pointer;border:none;background:none;border-bottom:2px solid transparent;margin-bottom:-2px;transition:color .15s,border-color .15s;font-family:inherit}
.tab:hover{color:var(--text)}
.tab.active{color:var(--blue);border-bottom-color:var(--blue);font-weight:600}
.tab-content{display:none}
.tab-content.active{display:block}
.cmd-tab{display:none}
.cmd-tab.active{display:block}

/* ---- Install steps ---- */
.install-steps{font-size:.88rem;line-height:1.7}
.install-steps ol{padding-left:20px}
.install-steps li{margin-bottom:8px}
.install-steps code{background:#f1f5f9;padding:1px 6px;border-radius:3px;font-family:"SF Mono",Consolas,monospace;font-size:.8rem}

.dl-btn{display:inline-flex;align-items:center;gap:5px;padding:6px 14px;font-size:.82rem;font-weight:600;border:1px solid var(--border);border-radius:5px;background:var(--surface);color:var(--blue);cursor:pointer;font-family:inherit;transition:background .15s;text-decoration:none;margin-top:8px;margin-right:6px}
.dl-btn:hover{background:#f1f5f9}
.dl-btn svg{width:14px;height:14px;fill:currentColor}
.hidden{display:none}
@media(max-width:600px){.field-row{flex-direction:column;gap:0}.container{padding:14px 12px 48px}.card{padding:16px}.header-inner{flex-direction:column;gap:8px}}
</style>
</head>
<body>

<div class="header">
  <div class="header-inner">
    <svg class="header-logo" viewBox="0 0 240 160" xmlns="http://www.w3.org/2000/svg">
      <defs>
        <linearGradient id="sky" x1="0%" y1="0%" x2="0%" y2="100%">
          <stop offset="0%" stop-color="#f59e0b"/>
          <stop offset="60%" stop-color="#ea580c"/>
          <stop offset="100%" stop-color="#7c2d12"/>
        </linearGradient>
        <linearGradient id="mtn" x1="0%" y1="0%" x2="0%" y2="100%">
          <stop offset="0%" stop-color="#94a3b8"/>
          <stop offset="100%" stop-color="#64748b"/>
        </linearGradient>
      </defs>
      <circle cx="120" cy="90" r="24" fill="url(#sky)" opacity=".9"/>
      <path d="M0 150 L60 40 L105 105 L120 90" fill="url(#mtn)" opacity=".85"/>
      <path d="M120 90 L135 105 L180 40 L240 150" fill="url(#mtn)" opacity=".85"/>
      <line x1="0" y1="150" x2="240" y2="150" stroke="#475569" stroke-width="2"/>
    </svg>
    <div class="header-text">
      <h1>Go-LLM-Proxy Config Generator</h1>
      <p>Generate configuration files for your coding assistant</p>
    </div>
  </div>
</div>

<div class="container">

  <!-- Models overview -->
  <div class="card">
    <h2>Available Models</h2>
    <table class="model-table">
      <thead><tr><th>Model</th><th>Protocol</th><th>Data Safety</th><th>Context</th></tr></thead>
      <tbody id="modelTableBody"></tbody>
    </table>
  </div>

  <!-- Configuration form -->
  <div class="card">
    <h2>Generate Configuration</h2>

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
        <label for="tavilyKey">Tavily API Key <span style="font-weight:400;text-transform:none">(optional &mdash; web search)</span></label>
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
          <label style="display:inline-flex;align-items:center;gap:5px;margin-top:5px;font-size:.82rem;font-weight:400;text-transform:none;letter-spacing:0;cursor:pointer"><input type="checkbox" id="sonnetThinking" checked style="width:14px;height:14px;accent-color:var(--blue)"> Thinking</label>
        </div>
        <div class="field">
          <label for="opusModel">Opus <span style="font-weight:400;text-transform:none">(large model)</span></label>
          <select id="opusModel"></select>
          <label style="display:inline-flex;align-items:center;gap:5px;margin-top:5px;font-size:.82rem;font-weight:400;text-transform:none;letter-spacing:0;cursor:pointer"><input type="checkbox" id="opusThinking" checked style="width:14px;height:14px;accent-color:var(--blue)"> Thinking</label>
        </div>
        <div class="field">
          <label for="haikuModel">Haiku <span style="font-weight:400;text-transform:none">(fast model)</span></label>
          <select id="haikuModel"></select>
          <label style="display:inline-flex;align-items:center;gap:5px;margin-top:5px;font-size:.82rem;font-weight:400;text-transform:none;letter-spacing:0;cursor:pointer"><input type="checkbox" id="haikuThinking" style="width:14px;height:14px;accent-color:var(--blue)"> Thinking</label>
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
            <input type="radio" name="outputFormat" value="config" checked style="width:16px;height:16px;accent-color:var(--blue)">
            <span style="font-size:.9rem">Configuration file</span>
          </label>
          <label class="checkbox-group" style="padding:0;display:inline-flex;cursor:pointer;margin-left:12px">
            <input type="radio" name="outputFormat" value="command" style="width:16px;height:16px;accent-color:var(--blue)">
            <span style="font-size:.9rem">Start command (shell script)</span>
          </label>
        </div>
      </div>
    </div>

    <div class="btn-row">
      <button class="btn btn-primary" id="generateBtn" disabled onclick="generate()">Generate Config</button>
    </div>
  </div>

  <!-- Output -->
  <div class="output-area" id="outputArea">
    <div class="card">
      <h2 id="configTitle">Configuration File</h2>
      <div id="configOutput"></div>
    </div>

    <div class="card">
      <h2>Installation Instructions</h2>
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

<script>
// Model data and proxy capabilities injected server-side.
var MODELS = {{.Models}};
var HAS_VISION = {{.HasVision}};
var HAS_WEB_SEARCH = {{.HasWebSearch}};
var HAS_MCP = {{.HasMCP}};
var PROXY_ORIGIN = location.origin;
var PROXY_URL = PROXY_ORIGIN + "/v1";

// ---- Populate model overview table ----
(function(){
  var tbody = document.getElementById("modelTableBody");
  for (var i = 0; i < MODELS.length; i++) {
    var m = MODELS[i];
    var row = document.createElement("tr");
    var badges = "";
    if (m.supports_vision) badges += ' <span class="badge badge-vision">vision</span>';
    var safety = m.local
      ? '<span class="badge badge-safe">Safe for data</span>'
      : '<span class="badge badge-warn">Warning &mdash; 3rd party</span>';
    row.innerHTML = '<td><strong>' + esc(m.id) + '</strong>' + badges + '</td>' +
      '<td><span class="badge ' + (m.protocol === "anthropic" ? "badge-proto-ant" : "badge-proto-oai") + '">' + m.protocol + '</span></td>' +
      '<td>' + safety + '</td>' +
      '<td>' + (m.context_window > 0 ? m.context_window.toLocaleString() : "unknown") + '</td>';
    tbody.appendChild(row);
  }
})();

function esc(s){ var d=document.createElement("div"); d.textContent=s; return d.innerHTML; }
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
  // Update Tavily hint based on proxy-side search availability and selected harness
  var hint = document.getElementById("tavilyHint");
  if(h==="qwen-code"){
    hint.textContent = "Qwen Code uses client-side search. Enter your Tavily key to enable web search.";
    hint.style.color = "var(--muted)";
  } else if(HAS_WEB_SEARCH){
    hint.textContent = "Proxy already has web search configured. Client-side Tavily MCP is optional.";
    hint.style.color = "var(--green)";
  } else {
    hint.textContent = "";
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
    hint.style.color = "var(--amber)";
    sel.style.borderColor = "var(--amber)";
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
    var safety = m.local
      ? ' <span class="badge badge-safe" style="margin-left:4px">Safe</span>'
      : ' <span class="badge badge-warn" style="margin-left:4px">3rd party</span>';
    var proto = m.protocol==="anthropic"
      ? ' <span class="badge badge-proto-ant" style="margin-left:4px">Anthropic</span>' : '';
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
  // Only add client-side Tavily MCP if proxy doesn't have search
  var useTavily = tavily && !HAS_WEB_SEARCH;
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

  // Only add client-side Tavily MCP if proxy doesn't have search
  var useTavily = tavily && !HAS_WEB_SEARCH;
  var tavilyMcpJSON = useTavily ? JSON.stringify({type:"http",url:"https://mcp.tavily.com/mcp",headers:{"Authorization":"Bearer "+tavily}}) : "";

  var shLines = ["#!/usr/bin/env bash", "# go-llm-proxy: Claude Code start script", ""];
  if(useTavily){
    shLines.push("# Configure Tavily web search");
    shLines.push("claude mcp remove tavily -s user 2>/dev/null");
    shLines.push("claude mcp add-json tavily '" + tavilyMcpJSON + "' -s user");
    shLines.push("");
  }
  shLines.push("exec env \\");
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
    ps1Lines.push("", "# Configure Tavily web search");
    ps1Lines.push("claude mcp remove tavily -s user 2>$null");
    ps1Lines.push("claude mcp add-json tavily '" + tavilyMcpJSON + "' -s user");
  }
  ps1Lines.push("", "try {", "  claude --settings '" + settingsJSON + "' @args", "} finally {");
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
  // Only disable web_search when proxy doesn't handle it
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

  // Only add client-side Tavily MCP if proxy doesn't have search
  if(tavily && !HAS_WEB_SEARCH){
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
    'The API key' + (tavily && !HAS_WEB_SEARCH ? 's are' : ' is') + ' embedded in the config. To use environment variables instead, ' +
      'edit the file and swap the commented lines, then set:<br>' +
      '<code>export OPENAI_API_KEY=' + esc(apiKey) + '</code>' +
      (tavily && !HAS_WEB_SEARCH ? '<br><code>export TAVILY_API_KEY=' + esc(tavily) + '</code>' : ''),
    'Restart Codex for changes to take effect.' + searchNote
  ];

  var winSteps = [
    'Create the config directory:<br><code>mkdir %USERPROFILE%\\.codex</code>',
    'Save the generated file as:<br><code>%USERPROFILE%\\.codex\\config.toml</code>',
    'The API key' + (tavily && !HAS_WEB_SEARCH ? 's are' : ' is') + ' embedded in the config. To use environment variables instead, ' +
      'edit the file and swap the commented lines, then set:<br>' +
      '<code>setx OPENAI_API_KEY ' + esc(apiKey) + '</code>' +
      (tavily && !HAS_WEB_SEARCH ? '<br><code>setx TAVILY_API_KEY ' + esc(tavily) + '</code>' : ''),
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
  var useTavily = tavily && !HAS_WEB_SEARCH;

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
  shLines.push("exec codex \\");
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

  // Qwen Code always needs client-side search config — it doesn't use the proxy for search.
  if(tavily){
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

  // Prefer proxy MCP endpoint for search if available, otherwise client-side Tavily
  if(HAS_MCP){
    obj.mcp = {
      "proxy-search": {
        type: "remote",
        url: PROXY_ORIGIN + "/mcp/sse",
        headers: { "Authorization": "Bearer " + apiKey },
        enabled: true
      }
    };
  } else if(tavily){
    obj.mcp = {
      tavily: {
        type: "remote",
        url: "https://mcp.tavily.com/mcp",
        headers: { "Authorization": "Bearer " + tavily },
        enabled: true
      }
    };
  }

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
        'Save <code>opencode.json</code> to your project root, or globally to:<br><code>%APPDATA%\\opencode\\opencode.json</code>',
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

// ---- MCP card (shown when proxy has web search) ----
if (HAS_MCP) {
  var mcpCard = document.createElement("div");
  mcpCard.className = "card";
  mcpCard.innerHTML = '<h2>Web Search (MCP)</h2>' +
    '<p style="margin-bottom:12px">This proxy provides web search via MCP. Add to your client config:</p>' +
    '<pre style="background:var(--slate-bg);color:var(--slate-text);padding:14px;border-radius:6px;font-size:.82rem;overflow-x:auto;line-height:1.5">' +
    '// OpenCode (~/.config/opencode/opencode.json)\n' +
    '"mcp": {\n' +
    '  "proxy-search": {\n' +
    '    "type": "remote",\n' +
    '    "url": "' + PROXY_ORIGIN + '/mcp/sse",\n' +
    '    "headers": { "Authorization": "Bearer &lt;your-proxy-key&gt;" },\n' +
    '    "enabled": true\n' +
    '  }\n' +
    '}\n\n' +
    '// Codex (~/.codex/config.toml)\n' +
    '[mcp_servers.proxy-search]\n' +
    'url = "' + PROXY_ORIGIN + '/mcp/sse"\n' +
    'headers = { Authorization = "Bearer &lt;your-proxy-key&gt;" }' +
    '</pre>' +
    '<p class="hint" style="margin-top:8px">Replace &lt;your-proxy-key&gt; with your API key. Claude Code and Codex get web search automatically via the proxy &mdash; MCP is for clients like OpenCode that don\'t use the Responses or Messages API search tools.</p>';
  document.querySelector(".container").appendChild(mcpCard);
}
</script>
</body>
</html>`
