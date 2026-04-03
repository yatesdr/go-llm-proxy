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
	}
	td := tmplData{
		Models:       template.JS(data),
		HasVision:    cfg.Processors.Vision != "",
		HasWebSearch: cfg.Processors.WebSearchKey != "",
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
</style>
</head>
<body>
<div class="header"><div class="header-inner"><div class="header-text"><h1>Go-LLM-Proxy</h1><p>Config Generator</p></div></div></div>
<div class="container">
<div class="card">
<h2>Available Models</h2>
<table class="model-table">
<thead><tr><th>Model</th><th>Protocol</th><th>Location</th><th>Context</th></tr></thead>
<tbody id="modelsBody"></tbody>
</table>
</div>
</div>
<script>
var MODELS = {{.Models}};
var HAS_VISION = {{.HasVision}};
var HAS_WEB_SEARCH = {{.HasWebSearch}};
(function(){
var tbody = document.getElementById("modelsBody");
for (var i = 0; i < MODELS.length; i++) {
  var m = MODELS[i];
  var row = document.createElement("tr");
  var badges = "";
  if (m.supports_vision) badges += ' <span class="badge badge-vision">vision</span>';
  row.innerHTML = '<td>' + m.id + badges + '</td>' +
    '<td><span class="badge ' + (m.protocol === "anthropic" ? "badge-proto-ant" : "badge-proto-oai") + '">' + m.protocol + '</span></td>' +
    '<td><span class="badge ' + (m.local ? "badge-safe" : "badge-warn") + '">' + (m.local ? "local" : "remote") + '</span></td>' +
    '<td>' + (m.context_window > 0 ? m.context_window.toLocaleString() : "unknown") + '</td>';
  tbody.appendChild(row);
}
})();
</script>
</body>
</html>`
