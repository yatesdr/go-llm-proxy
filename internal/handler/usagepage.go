package handler

import (
	"encoding/json"
	"html"
	"log/slog"
	"net/http"
	"strconv"

	"go-llm-proxy/internal/config"
	"go-llm-proxy/internal/httputil"
	"go-llm-proxy/internal/ratelimit"
	"go-llm-proxy/internal/usage"
)

const dashboardCookieName = "usage_auth"

type UsageDashboardHandler struct {
	config   *config.ConfigStore
	usage    *usage.UsageLogger
	rl       *ratelimit.RateLimiter
	sessions *sessionStore
}

func NewUsageDashboardHandler(cs *config.ConfigStore, ul *usage.UsageLogger, rl *ratelimit.RateLimiter) *UsageDashboardHandler {
	return &UsageDashboardHandler{
		config:   cs,
		usage:    ul,
		rl:       rl,
		sessions: newSessionStore(),
	}
}

func (h *UsageDashboardHandler) LoginPage(w http.ResponseWriter, r *http.Request) {
	if h.checkCookie(r) {
		h.renderDashboard(w)
		return
	}
	h.renderLogin(w, "")
}

func (h *UsageDashboardHandler) HandleLogin(w http.ResponseWriter, r *http.Request) {
	ip := ratelimit.ClientIP(h.rl, r)
	if !h.rl.Check(ip) {
		httputil.WriteError(w, http.StatusTooManyRequests, "too many attempts")
		return
	}
	if err := r.ParseForm(); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "bad request")
		return
	}
	password := r.FormValue("password")
	cfg := h.config.Get()
	if !constantTimeEqual(password, cfg.UsageDashboardPassword) {
		h.rl.RecordFailure(ip)
		h.renderLogin(w, "Incorrect password")
		return
	}
	token := h.sessions.create()
	if token == "" {
		httputil.WriteError(w, http.StatusInternalServerError, "session creation failed")
		return
	}
	h.setCookie(w, r, token)
	http.Redirect(w, r, "/usage", http.StatusSeeOther)
}

// HandleLogout revokes the caller's session and clears the cookie.
func (h *UsageDashboardHandler) HandleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(dashboardCookieName); err == nil {
		h.sessions.revoke(cookie.Value)
	}
	// Expire the cookie on the client too.
	http.SetCookie(w, &http.Cookie{
		Name:     dashboardCookieName,
		Value:    "",
		Path:     "/usage",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   requestIsTLS(r, h.rl),
		MaxAge:   -1,
	})
	http.Redirect(w, r, "/usage", http.StatusSeeOther)
}

func (h *UsageDashboardHandler) ServeData(w http.ResponseWriter, r *http.Request) {
	if !h.checkCookie(r) {
		httputil.WriteError(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	days := 30
	if d := r.URL.Query().Get("days"); d != "" {
		if v, err := strconv.Atoi(d); err == nil && v > 0 && v <= 365 {
			days = v
		}
	}
	data, err := h.usage.QueryDashboardData(days)
	if err != nil {
		slog.Error("dashboard query failed", "error", err)
		httputil.WriteError(w, http.StatusInternalServerError, "query failed")
		return
	}
	httputil.SetSecurityHeaders(w)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
	json.NewEncoder(w).Encode(data)
}

func (h *UsageDashboardHandler) checkCookie(r *http.Request) bool {
	cookie, err := r.Cookie(dashboardCookieName)
	if err != nil {
		return false
	}
	return h.sessions.validate(cookie.Value)
}

func (h *UsageDashboardHandler) setCookie(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     dashboardCookieName,
		Value:    token,
		Path:     "/usage",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   requestIsTLS(r, h.rl),
		MaxAge:   int(dashboardSessionTTL.Seconds()),
	})
}

func (h *UsageDashboardHandler) renderLogin(w http.ResponseWriter, errMsg string) {
	httputil.SetSecurityHeaders(w)
	w.Header().Set("Content-Security-Policy", cspWithFonts)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	errNotice := ""
	if errMsg != "" {
		errNotice = `<div class="error-notice">` + html.EscapeString(errMsg) + `</div>`
	}
	w.Write([]byte(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Usage Dashboard</title>
`+fontLinks+`
<style>` + dashboardCSS() + `</style>
</head>
<body class="auth-body">
<div class="auth-card">
` + eidonixMark("auth-mark") + `
<div class="auth-title">EIDONIX</div>
<div class="auth-sub">LLM Proxy · Usage</div>
` + errNotice + `
<form method="POST" action="/usage">
<div class="field">
<label for="password">Password</label>
<input type="password" id="password" name="password" autofocus autocomplete="current-password" required>
</div>
<div class="btn-row">
<button class="btn btn-primary" type="submit" style="width:100%;justify-content:center">Sign In</button>
</div>
</form>
</div>
</body>
</html>`))
}

func (h *UsageDashboardHandler) renderDashboard(w http.ResponseWriter) {
	httputil.SetSecurityHeaders(w)
	w.Header().Set("Content-Security-Policy", cspWithFonts)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Usage Dashboard</title>
`+fontLinks+`
<style>` + dashboardCSS() + `</style>
</head>
<body>
<div class="shell">
<div class="topbar"><div class="topbar-inner">
` + topbarBrand("/usage", "Usage") + `
<div class="topbar-right">
<a class="nav-ext" href="/admin">Admin</a>
<a class="nav-ext" href="/">Setup</a>
<form method="POST" action="/usage/logout"><button class="btn-logout" type="submit">Sign out</button></form>
</div>
</div></div>
<div class="content"><div class="container">
<div class="summary-cards" id="summaryCards"></div>
<div class="card">
<div class="card-header">
<h2 id="chartTitle">Daily Tokens</h2>
<div style="display:flex;gap:8px;align-items:center">
<div class="toggle-group">
<button class="toggle-btn" id="toggleRequests" onclick="setChartMode('requests')">Requests</button>
<button class="toggle-btn active" id="toggleTokens" onclick="setChartMode('tokens')">Tokens</button>
</div>
<select id="periodSelect" onchange="loadData()">
<option value="7">Last 7 days</option>
<option value="30" selected>Last 30 days</option>
<option value="90">Last 90 days</option>
</select>
</div>
</div>
<div id="dailyChart"></div>
</div>
<div class="card">
<div class="card-header"><h2>Users</h2><input id="userTableFilter" class="filter-input" type="search" placeholder="Filter users&hellip;" autocomplete="off"></div>
<div class="table-wrap"><table class="data-table">
<thead><tr><th>Name</th><th>Key</th><th>Requests</th><th>Tokens</th><th>Active Days</th><th>Last Seen</th></tr></thead>
<tbody id="usersBody"></tbody>
</table></div>
</div>
<div class="card">
<h2>Models</h2>
<div class="table-wrap"><table class="data-table">
<thead><tr><th>Model</th><th>Requests</th><th>Users</th><th>Tokens</th><th>Avg Latency</th><th>Errors</th></tr></thead>
<tbody id="modelsBody"></tbody>
</table></div>
</div>
<div class="card" id="backendsCard" style="display:none">
<div class="card-header"><h2>Backends</h2><input id="backendTableFilter" class="filter-input" type="search" placeholder="Filter backends&hellip;" autocomplete="off"></div>
<div class="table-wrap"><table class="data-table">
<thead><tr><th>Model</th><th>Backend</th><th>Requests</th><th>Share</th><th>Tokens</th><th>Avg Latency</th><th>Errors</th></tr></thead>
<tbody id="backendsBody"></tbody>
</table></div>
</div>
</div></div>
</div>
<script>
var MODEL_COLORS=["#2a78d6","#eb6834","#1baf7a","#eda100","#e87ba4","#008300","#4a3aa7","#e34948"];
var OTHER_COLOR="#9a9a97";
var chartMode="tokens";
var lastData=null;
(function(){loadData()})();
function setChartMode(mode){
	chartMode=mode;
	document.getElementById("toggleRequests").classList.toggle("active",mode==="requests");
	document.getElementById("toggleTokens").classList.toggle("active",mode==="tokens");
	document.getElementById("chartTitle").textContent=mode==="tokens"?"Daily Tokens":"Daily Requests";
	if(lastData)renderChart(lastData.daily,lastData.daily_models);
}
function loadData(){
	var days=document.getElementById("periodSelect").value;
	fetch("/usage/data?days="+days)
		.then(function(r){return r.json()})
		.then(function(d){renderData(d)})
		.catch(function(e){console.error(e)});
}
function renderData(d){
	lastData=d;
	var sc=document.getElementById("summaryCards");
	sc.innerHTML=
		summaryCard("Total Requests",fmtNum(d.totals.requests))+
		summaryCard("Total Tokens",fmtNum(d.totals.total_tokens))+
		summaryCard("Active Users",d.totals.users)+
		summaryCard("Error Rate",d.totals.error_rate.toFixed(1)+"%");
	renderChart(d.daily,d.daily_models);
	renderTable("usersBody",d.users,function(u){
		return "<td>"+esc(u.name)+"</td><td><code>"+esc(u.key_hash)+"</code></td>"+
			"<td>"+fmtNum(u.requests)+"</td><td>"+fmtNum(u.total_tokens)+"</td>"+
			"<td>"+u.active_days+"</td><td>"+esc(u.last_seen)+"</td>";
	});
	renderTable("modelsBody",d.models,function(m){
		var errCell=m.errors>0
			?"<td style=\"color:var(--danger);font-weight:700\">"+fmtNum(m.errors)+"</td>"
			:"<td style=\"color:var(--faint)\">0</td>";
		return "<td>"+esc(m.model)+"</td><td>"+fmtNum(m.requests)+"</td>"+
			"<td>"+m.users+"</td><td>"+fmtNum(m.total_tokens)+"</td>"+
			"<td>"+Math.round(m.avg_latency_ms)+" ms</td>"+errCell;
	});
	// Backends table: shown only when pooled routing has produced rows.
	var bc=document.getElementById("backendsCard");
	if(d.backends&&d.backends.length){
		bc.style.display="";
		var perModel={};
		for(var i=0;i<d.backends.length;i++){
			perModel[d.backends[i].model]=(perModel[d.backends[i].model]||0)+d.backends[i].requests;
		}
		renderTable("backendsBody",d.backends,function(b){
			var share=perModel[b.model]>0?(100*b.requests/perModel[b.model]).toFixed(0)+"%":"—";
			return "<td>"+esc(b.model)+"</td><td><code>"+esc(b.backend)+"</code></td>"+
				"<td>"+fmtNum(b.requests)+"</td><td>"+share+"</td>"+
				"<td>"+fmtNum(b.total_tokens)+"</td>"+
				"<td>"+Math.round(b.avg_latency_ms)+" ms</td><td>"+fmtNum(b.errors)+"</td>";
		});
	}else{
		bc.style.display="none";
	}
}
function summaryCard(label,value){
	return "<div class=\"summary-card\"><div class=\"summary-value\">"+value+"</div><div class=\"summary-label\">"+label+"</div></div>";
}
function renderChart(rows,modelRows){
	var el=document.getElementById("dailyChart");
	if(!rows||rows.length===0){el.innerHTML="<p style=\"color:var(--muted);padding:20px 0\">No data for this period.</p>";return;}
	var useTokens=chartMode==="tokens";
	var valKey=useTokens?"total_tokens":"requests";
	var valLabel=useTokens?"tokens":"requests";
	// Series get palette slots in fixed order by total volume; past 8, fold into "Other".
	var totals={};
	if(modelRows){for(var i=0;i<modelRows.length;i++){var mr=modelRows[i];totals[mr.model]=(totals[mr.model]||0)+(mr[valKey]||0);}}
	var allModels=Object.keys(totals).sort(function(a,b){return totals[b]-totals[a];});
	var top=allModels.slice(0,8);
	var hasOther=allModels.length>8;
	function colorFor(m){var i=top.indexOf(m);return i>=0?MODEL_COLORS[i]:OTHER_COLOR;}
	var dateMap={};
	for(var i=0;i<rows.length;i++){dateMap[rows[i].date]={total:rows[i][valKey],models:{}};}
	if(modelRows){for(var i=0;i<modelRows.length;i++){
		var dm=modelRows[i];
		if(!dateMap[dm.date])continue;
		var key=top.indexOf(dm.model)>=0?dm.model:"Other";
		dateMap[dm.date].models[key]=(dateMap[dm.date].models[key]||0)+(dm[valKey]||0);
	}}
	var max=0;
	for(var i=0;i<rows.length;i++){if(rows[i][valKey]>max)max=rows[i][valKey];}
	var html="";
	if(allModels.length>1){
		html+="<div class=\"chart-legend\">";
		for(var i=0;i<top.length;i++){
			html+="<span class=\"legend-item\"><span class=\"legend-swatch\" style=\"background:"+MODEL_COLORS[i]+"\"></span>"+esc(top[i])+"</span>";
		}
		if(hasOther)html+="<span class=\"legend-item\"><span class=\"legend-swatch\" style=\"background:"+OTHER_COLOR+"\"></span>Other</span>";
		html+="</div>";
	}
	html+="<div class=\"bars\">";
	for(var i=0;i<rows.length;i++){
		var r=rows[i];
		var val=r[valKey];
		var pct=max>0?(val/max*100):0;
		var dateLabel=r.date.substring(5);
		var dm=dateMap[r.date];
		var inner="";
		if(allModels.length>1&&dm){
			var segOrder=top.slice();
			if(hasOther)segOrder.push("Other");
			// Stack in fixed series order so each model keeps its position day to day.
			for(var j=0;j<segOrder.length;j++){
				var name=segOrder[j];
				var v=dm.models[name];
				if(!v)continue;
				var segPct=max>0?(v/max*100):0;
				inner+="<div class=\"bar-segment\" style=\"height:"+segPct+"%;background:"+colorFor(name==="Other"?"__other__":name)+"\" title=\""+esc(name)+": "+fmtNum(v)+" "+valLabel+"\"></div>";
			}
		}else{
			inner="<div class=\"bar-segment\" style=\"height:"+pct+"%;background:"+MODEL_COLORS[0]+"\"></div>";
		}
		html+="<div class=\"bar-group\" title=\""+esc(r.date)+": "+fmtNum(r.requests)+" requests, "+fmtNum(r.total_tokens)+" tokens\">"+
			"<div class=\"bar-stack\">"+inner+"</div>"+
			"<div class=\"bar-label\">"+esc(dateLabel)+"</div></div>";
	}
	html+="</div>";
	el.innerHTML=html;
}
function renderTable(id,rows,cellFn){
	var tbody=document.getElementById(id);
	var html="";
	for(var i=0;i<rows.length;i++){html+="<tr>"+cellFn(rows[i])+"</tr>";}
	if(!rows.length)html="<tr><td colspan=\"99\" style=\"text-align:center;color:var(--muted);padding:16px\">No data</td></tr>";
	tbody.innerHTML=html;
}
function fmtNum(n){
	if(typeof n!=="number")return String(n);
	return n.toLocaleString();
}
function esc(s){var d=document.createElement("div");d.textContent=s;return d.innerHTML;}
function tableFilter(inputId,bodyId){
	var inp=document.getElementById(inputId);
	if(!inp)return;
	inp.addEventListener("input",function(){
		var q=inp.value.toLowerCase();
		var rows=document.getElementById(bodyId).rows;
		for(var i=0;i<rows.length;i++){
			rows[i].style.display=(!q||rows[i].textContent.toLowerCase().indexOf(q)>=0)?"":"none";
		}
	});
}
tableFilter("userTableFilter","usersBody");
tableFilter("backendTableFilter","backendsBody");
</script>
</body>
</html>`))
}

// eidonixMark returns the official Eidonix mark (from eidos.svg) as inline
// SVG. It fills with currentColor so chrome can color it (signal red in the
// top bar, black on white auth cards).
func eidonixMark(class string) string {
	return `<svg class="` + class + `" viewBox="40 70 440 350" xmlns="http://www.w3.org/2000/svg" aria-hidden="true"><g transform="translate(0,512) scale(0.1,-0.1)" fill="currentColor" stroke="none"><path d="M2384 4078 c-81 -128 -254 -397 -384 -598 -130 -201 -258 -400 -285 -442 -28 -43 -207 -323 -400 -623 -192 -300 -423 -656 -512 -791 -114 -174 -157 -247 -148 -251 21 -7 2492 1 2500 9 4 3 -17 38 -45 77 -29 40 -89 131 -135 204 l-83 132 -93 7 c-52 4 -348 7 -659 6 -437 -2 -564 0 -563 10 6 26 903 1426 955 1489 19 24 45 -6 154 -174 59 -93 176 -271 260 -398 83 -126 199 -306 256 -400 122 -198 244 -395 411 -659 65 -104 131 -212 145 -241 l26 -53 66 -6 c83 -8 456 -6 491 3 l26 6 -69 110 c-37 61 -97 155 -131 210 -35 55 -109 174 -166 265 -101 163 -138 222 -400 640 -74 118 -177 285 -229 370 -52 85 -197 319 -322 520 -125 201 -282 455 -350 565 -67 110 -129 210 -137 223 -7 12 -17 22 -22 22 -5 0 -76 -105 -157 -232z"/><path d="M2491 2673 c13 -21 44 -69 69 -108 26 -38 103 -160 172 -270 196 -312 312 -495 453 -713 l130 -202 163 0 c90 0 162 3 160 8 -2 4 -38 61 -81 127 -43 66 -142 221 -219 345 -78 124 -181 288 -229 365 -48 77 -138 218 -200 313 l-112 172 -164 0 -165 0 23 -37z"/></g></svg>`
}

// topbarBrand renders the Eidonix brand block used in every page's top bar:
// the mark in signal red, the wordmark, and the app name.
func topbarBrand(href, sub string) string {
	return `<a class="brand" href="` + href + `">` + eidonixMark("brand-mark") +
		`<span class="brand-name">EIDONIX</span><span class="brand-app">· ` + sub + `</span></a>`
}

// fontLinks loads IBM Plex from Google Fonts (the style guide's UI faces).
// Pages fall back to system-ui when the host has no internet access.
const fontLinks = `<link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=IBM+Plex+Sans:wght@400;500;600;700&family=IBM+Plex+Mono:wght@400;500&display=swap">`

// cspWithFonts is the Content-Security-Policy for HTML pages, permitting
// inline styles/scripts plus the Google Fonts hosts and same-origin fetches.
const cspWithFonts = "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline' https://fonts.googleapis.com; font-src https://fonts.gstatic.com; connect-src 'self'; frame-ancestors 'none'"

func dashboardCSS() string {
	return `*,*::before,*::after{box-sizing:border-box;margin:0;padding:0}
:root{
  --black:#000000;--steel:#6E6E73;--quiet:#A6A6AB;--paper:#FFFFFF;--panel:#F4F4F5;
  --hairline:#E8E8E8;--border:#9C9CA1;
  --red:#E10600;--red-down:#B00500;--red-wash:#FFF3F2;
  --blue:#1D4ED8;--blue-down:#163FAE;
  --accent:#00C2C7;
  --ok:#1E8E5A;--warn:#B87700;
  --font-ui:"IBM Plex Sans",system-ui,-apple-system,"Segoe UI",sans-serif;
  --font-mono:"IBM Plex Mono",ui-monospace,"SF Mono",Consolas,monospace;
  --shadow-pop:0 6px 24px rgba(0,0,0,.16);
  /* aliases kept for page scripts */
  --ink:#000000;--text:#000000;--surface:#FFFFFF;--muted:#6E6E73;--faint:#A6A6AB;
  --danger:#E10600;--danger-bg:#FFF3F2;--success:#1E8E5A;--mono:var(--font-mono);
}
html,body{height:100%}
body{font-family:var(--font-ui);background:var(--paper);color:var(--black);font-size:13px;line-height:20px}
::selection{background:#000;color:#fff}
a{color:var(--blue);text-decoration:none}
a:hover{text-decoration:underline}
/* ---- Shell ---- */
.shell{display:flex;flex-direction:column;height:100vh}
.topbar{background:#000;color:#fff;flex:none}
.topbar::after{content:"";display:block;height:4px;background:var(--accent)}
.topbar-inner{display:flex;align-items:center;gap:12px;height:62px;padding:0 20px}
.brand{display:inline-flex;align-items:center;gap:10px;color:#fff;text-decoration:none}
.brand:hover{text-decoration:none}
.brand-mark{height:32px;width:auto;display:block;color:#fff}
.brand-name{font-weight:700;font-size:13px;letter-spacing:.14em}
.brand-app{color:var(--quiet);font-weight:600;font-size:12px;letter-spacing:.08em;text-transform:uppercase;white-space:nowrap}
.topbar-right{margin-left:auto;display:flex;align-items:center;gap:16px}
.nav-ext{color:#c9c9cd;font-size:12px;font-weight:600;letter-spacing:.08em;text-transform:uppercase;text-decoration:none}
.nav-ext:hover{color:#fff;text-decoration:none}
.btn-logout{background:transparent;border:1px solid #3a3a3a;color:#ccc;height:28px;padding:0 12px;font-family:var(--font-ui);font-size:11px;font-weight:600;letter-spacing:.06em;text-transform:uppercase;cursor:pointer;border-radius:0}
.btn-logout:hover{border-color:#fff;color:#fff}
.tabs-bar{height:40px;background:var(--paper);border-bottom:1px solid var(--hairline);display:flex;align-items:stretch;padding:0 16px;gap:4px;flex:none}
.admin-tab{display:inline-flex;align-items:center;padding:0 14px;font-size:12px;font-weight:600;letter-spacing:.08em;text-transform:uppercase;color:var(--steel);text-decoration:none;border-bottom:3px solid transparent}
.admin-tab:hover{color:var(--black);text-decoration:none;border-bottom-color:var(--border)}
.admin-tab.active{color:var(--black);border-bottom-color:var(--black)}
.content{flex:1;overflow:auto;padding:16px;min-height:0;background:linear-gradient(180deg,#f8f8f9 0%,#e9e9ec 100%)}
.container{max-width:none;margin:0;padding:0}
/* ---- Auth ---- */
.auth-body{display:flex;align-items:center;justify-content:center;min-height:100vh;background:#000;padding:24px}
.auth-card{background:var(--paper);border-radius:6px;overflow:hidden;padding:0 32px 28px;width:100%;max-width:360px}
.auth-card::before{content:"";display:block;height:4px;margin:0 -32px 28px;background:var(--accent)}
.auth-card .auth-mark{height:60px;width:auto;display:block;color:var(--black);margin-bottom:12px}
.auth-title{font-weight:700;font-size:16px;letter-spacing:.14em}
.auth-sub{color:var(--steel);font-size:12px;font-weight:600;letter-spacing:.08em;text-transform:uppercase;margin:2px 0 20px}
/* ---- Cards & headings ---- */
.card{background:var(--paper);border:1px solid var(--black);border-radius:6px;padding:16px;margin-bottom:16px;box-shadow:4px 4px 0 rgba(0,0,0,.12)}
.card .card{border-color:var(--hairline);background:var(--panel);border-radius:4px;padding:12px;box-shadow:none}
.card h2{font-size:13px;font-weight:700;text-transform:uppercase;letter-spacing:.06em;margin-bottom:8px}
.card h3{font-size:13px;font-weight:700;text-transform:uppercase;letter-spacing:.06em}
.card-header{display:flex;align-items:center;gap:8px;margin-bottom:8px}
.card-header h2{flex:1;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.card-tools{display:flex;align-items:center;gap:8px;flex-shrink:1;flex-wrap:nowrap;min-width:0}
.card-tools .filter-input{min-width:80px;flex-shrink:1}
.card-header h2{margin:0}
.card-tools{display:flex;align-items:center;gap:8px;margin-left:auto}
/* ---- Summary strip ---- */
.summary-cards{display:grid;grid-template-columns:repeat(auto-fit,minmax(150px,1fr));gap:0;border:1px solid var(--black);border-radius:6px;background:var(--paper);margin-bottom:16px;box-shadow:4px 4px 0 rgba(0,0,0,.12);overflow:hidden}
.summary-card{padding:8px 14px;border-right:1px solid var(--hairline)}
.summary-card:last-child{border-right:none}
.summary-value{font-family:var(--font-mono);font-size:20px;line-height:28px;font-weight:600;font-variant-numeric:tabular-nums}
.summary-label{font-size:11px;line-height:16px;color:var(--steel);text-transform:uppercase;letter-spacing:.08em;font-weight:600;margin-top:1px}
/* ---- Forms ---- */
.field{margin-bottom:16px}
label{display:block;font-size:12px;line-height:20px;font-weight:500;color:var(--black);margin-bottom:2px}
input[type="password"],input[type="text"],input[type="number"],input[type="url"],input[type="search"]{width:100%;height:32px;padding:0 8px;font-size:13px;border:1px solid var(--border);border-radius:4px;background:var(--paper);color:var(--black);font-family:var(--font-ui)}
input:focus,select:focus{outline:none;border-color:var(--black);box-shadow:0 0 0 2px rgba(29,78,216,.25)}
.btn-row{margin-top:8px}
.btn{display:inline-flex;align-items:center;justify-content:center;gap:6px;height:32px;padding:0 14px;font-size:13px;font-weight:500;letter-spacing:0;text-transform:none;border:1px solid transparent;border-radius:4px;cursor:pointer;font-family:var(--font-ui);line-height:1;white-space:nowrap;flex-shrink:0}
.btn:active{filter:brightness(.92)}
.btn-primary{background:var(--blue);border-color:var(--blue-down);color:#fff}
.btn-primary:hover{background:var(--blue-down);border-color:var(--blue-down)}
.btn-primary:disabled{opacity:.45;cursor:not-allowed}
.error-notice{background:var(--red-wash);color:var(--red);border:1px solid var(--red);padding:8px 12px;margin-bottom:14px;font-size:12px;font-weight:500}
select{height:32px;padding:0 8px;font-size:13px;border:1px solid var(--border);border-radius:4px;background:var(--paper);color:var(--black);font-family:var(--font-ui)}
.filter-input{width:220px;height:28px;padding:0 8px;font-size:12px;border:1px solid var(--border);border-radius:0;background:var(--paper);color:var(--black);font-family:var(--font-ui)}
.filter-input:focus{outline:none;border-color:var(--black);box-shadow:0 0 0 2px rgba(29,78,216,.25)}
.toggle-group{display:inline-flex;border:1px solid var(--black)}
.toggle-btn{height:26px;padding:0 12px;font-size:11px;font-weight:600;letter-spacing:.06em;text-transform:uppercase;border:none;background:var(--paper);color:var(--steel);cursor:pointer;font-family:var(--font-ui)}
.toggle-btn:not(:last-child){border-right:1px solid var(--hairline)}
.toggle-btn:hover{color:var(--black)}
.toggle-btn.active{background:var(--black);color:#fff}
/* ---- Tables ---- */
.table-wrap{overflow-x:auto}
.data-table{width:100%;border-collapse:collapse;font-size:13px;table-layout:fixed}
.data-table th{text-align:left;font-size:11px;line-height:16px;font-weight:600;text-transform:uppercase;letter-spacing:.08em;color:var(--black);padding:6px 10px;border-bottom:2px solid var(--black);vertical-align:middle;position:sticky;top:-16px;background:var(--panel);z-index:2}
.data-table td{padding:0 10px;border-bottom:1px solid var(--hairline);vertical-align:middle;height:36px;box-sizing:border-box;overflow:hidden;font-variant-numeric:tabular-nums}
.data-table tr:last-child td{border-bottom:none}
.data-table tr:hover td{background:var(--panel)}
.data-table code{font-family:var(--font-mono);font-size:12px;background:var(--panel);padding:1px 5px;white-space:nowrap}
.data-table .mono{white-space:nowrap}
.data-table .cell-name{overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.data-table .cell-pills{max-width:520px}
.data-table .pills-scroll{display:flex;flex-wrap:nowrap;align-items:center;gap:4px;overflow-x:auto;overflow-y:hidden;scrollbar-width:thin}
.num{text-align:right;font-family:var(--font-mono)}
/* ---- Chart ---- */
.chart-legend{display:flex;flex-wrap:wrap;gap:10px;margin-bottom:8px}
.legend-item{display:inline-flex;align-items:center;gap:6px;font-size:12px;color:var(--black);font-family:var(--font-mono)}
.legend-swatch{display:inline-block;width:10px;height:10px;flex-shrink:0;border:1px solid rgba(0,0,0,.2)}
.bars{display:flex;align-items:flex-end;gap:3px;height:160px;padding-top:6px}
.bar-group{flex:1;display:flex;flex-direction:column;align-items:center;height:100%;justify-content:flex-end;min-width:0}
.bar-stack{display:flex;flex-direction:column;justify-content:flex-end;width:100%;max-width:40px;height:100%}
.bar-segment{width:100%;min-height:1px;transition:height .15s}
.bar-segment+.bar-segment{border-top:2px solid var(--paper)}
.bar-label{font-size:11px;color:var(--quiet);margin-top:4px;white-space:nowrap;overflow:hidden;text-overflow:ellipsis;max-width:100%;text-align:center;font-family:var(--font-mono)}
@media(prefers-reduced-motion:reduce){*{transition:none!important}}
@media(max-width:700px){.summary-cards{grid-template-columns:1fr 1fr}.bars{height:120px}.brand-app,.nav-ext{display:none}}`
}
