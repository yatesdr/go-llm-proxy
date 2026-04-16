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
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; frame-ancestors 'none'")
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
<style>` + dashboardCSS() + `</style>
</head>
<body>
<div class="header"><div class="header-inner"><h1>Usage Dashboard</h1></div></div>
<div class="container">
<div class="card" style="max-width:420px;margin:40px auto">
<h2>Sign In</h2>
` + errNotice + `
<form method="POST" action="/usage">
<div class="field">
<label for="password">Password</label>
<input type="password" id="password" name="password" autofocus autocomplete="current-password" required>
</div>
<div class="btn-row">
<button class="btn btn-primary" type="submit">Sign In</button>
</div>
</form>
</div>
</div>
</body>
</html>`))
}

func (h *UsageDashboardHandler) renderDashboard(w http.ResponseWriter) {
	httputil.SetSecurityHeaders(w)
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Usage Dashboard</title>
<style>` + dashboardCSS() + `</style>
</head>
<body>
<div class="header"><div class="header-inner"><h1>Usage Dashboard</h1><form method="POST" action="/usage/logout" style="margin-left:auto"><button class="btn-logout" type="submit">Sign out</button></form></div></div>
<div class="container">
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
<h2>Users</h2>
<div class="table-wrap"><table class="data-table">
<thead><tr><th>Name</th><th>Key</th><th>Requests</th><th>Tokens</th><th>Active Days</th><th>Last Seen</th></tr></thead>
<tbody id="usersBody"></tbody>
</table></div>
</div>
<div class="card">
<h2>Models</h2>
<div class="table-wrap"><table class="data-table">
<thead><tr><th>Model</th><th>Requests</th><th>Users</th><th>Tokens</th><th>Avg Latency</th></tr></thead>
<tbody id="modelsBody"></tbody>
</table></div>
</div>
</div>
<script>
var MODEL_COLORS=["#1a56db","#047857","#b45309","#7c3aed","#db2777","#0d9488","#ca8a04","#dc2626","#4f46e5","#059669","#d97706","#9333ea"];
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
		return "<td>"+esc(m.model)+"</td><td>"+fmtNum(m.requests)+"</td>"+
			"<td>"+m.users+"</td><td>"+fmtNum(m.total_tokens)+"</td>"+
			"<td>"+Math.round(m.avg_latency_ms)+" ms</td>";
	});
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
	var models=[];
	var modelSet={};
	if(modelRows){for(var i=0;i<modelRows.length;i++){if(!modelSet[modelRows[i].model]){modelSet[modelRows[i].model]=1;models.push(modelRows[i].model);}}}
	var dateMap={};
	for(var i=0;i<rows.length;i++){dateMap[rows[i].date]={total:rows[i][valKey],models:{}};}
	if(modelRows){for(var i=0;i<modelRows.length;i++){var dm=modelRows[i];if(dateMap[dm.date])dateMap[dm.date].models[dm.model]=dm[valKey];}}
	var max=0;
	for(var i=0;i<rows.length;i++){if(rows[i][valKey]>max)max=rows[i][valKey];}
	var html="";
	if(models.length>1){
		html+="<div class=\"chart-legend\">";
		for(var i=0;i<models.length;i++){
			var c=MODEL_COLORS[i%MODEL_COLORS.length];
			html+="<span class=\"legend-item\"><span class=\"legend-swatch\" style=\"background:"+c+"\"></span>"+esc(models[i])+"</span>";
		}
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
		if(models.length>1&&dm){
			var segments=Object.keys(dm.models).sort(function(a,b){return dm.models[b]-dm.models[a];});
			for(var j=0;j<segments.length;j++){
				var segPct=max>0?(dm.models[segments[j]]/max*100):0;
				var ci=models.indexOf(segments[j]);
				var c=MODEL_COLORS[(ci<0?j:ci)%MODEL_COLORS.length];
				inner+="<div class=\"bar-segment\" style=\"height:"+segPct+"%;background:"+c+"\" title=\""+esc(segments[j])+": "+fmtNum(dm.models[segments[j]])+" "+valLabel+"\"></div>";
			}
		}else{
			inner="<div class=\"bar-segment\" style=\"height:"+pct+"%;background:var(--blue)\"></div>";
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
</script>
</body>
</html>`))
}

func dashboardCSS() string {
	return `*,*::before,*::after{box-sizing:border-box;margin:0;padding:0}
:root{
  --bg:#f4f6f9;--surface:#fff;--border:#d8dde5;
  --text:#1b2033;--muted:#5c6377;
  --blue:#1a56db;--blue-hover:#1648b8;
  --green:#047857;--green-bg:#ecfdf5;
  --amber:#b45309;--amber-bg:#fffbeb;
  --slate-bg:#0f172a;--slate-text:#e2e8f0;--slate-muted:#94a3b8;
  --radius:8px;
  --shadow:0 1px 3px rgba(0,0,0,.07),0 1px 2px rgba(0,0,0,.05);
}
body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,Helvetica,Arial,sans-serif;background:var(--bg);color:var(--text);line-height:1.6;min-height:100vh}
.header{background:var(--slate-bg);color:#f1f5f9;padding:28px 0;text-align:center}
.header-inner{display:flex;align-items:center;justify-content:center;gap:16px;max-width:960px;margin:0 auto;padding:0 20px;position:relative}
.btn-logout{background:transparent;border:1px solid rgba(226,232,240,0.35);color:#e2e8f0;padding:6px 14px;font-size:.82rem;font-weight:500;border-radius:6px;cursor:pointer;font-family:inherit}
.btn-logout:hover{background:rgba(226,232,240,0.1)}
.header h1{font-size:1.6rem;font-weight:700;letter-spacing:-.02em}
.container{max-width:960px;margin:0 auto;padding:28px 20px 60px}
.card{background:var(--surface);border:1px solid var(--border);border-radius:var(--radius);padding:24px;margin-bottom:22px;box-shadow:var(--shadow)}
.card h2{font-size:1.05rem;font-weight:600;margin-bottom:14px;padding-bottom:10px;border-bottom:1px solid var(--border)}
.card-header{display:flex;align-items:center;justify-content:space-between;margin-bottom:14px;padding-bottom:10px;border-bottom:1px solid var(--border)}
.card-header h2{border:none;padding:0;margin:0}
.summary-cards{display:grid;grid-template-columns:repeat(auto-fit,minmax(180px,1fr));gap:16px;margin-bottom:22px}
.summary-card{background:var(--surface);border:1px solid var(--border);border-radius:var(--radius);padding:20px;box-shadow:var(--shadow);text-align:center}
.summary-value{font-size:1.8rem;font-weight:700;color:var(--blue)}
.summary-label{font-size:.82rem;color:var(--muted);margin-top:4px;text-transform:uppercase;letter-spacing:.03em;font-weight:600}
.field{margin-bottom:14px}
label{display:block;font-size:.78rem;font-weight:600;color:var(--muted);margin-bottom:3px;text-transform:uppercase;letter-spacing:.04em}
input[type="password"]{width:100%;padding:9px 11px;font-size:.92rem;border:1px solid var(--border);border-radius:6px;background:var(--surface);color:var(--text);transition:border-color .15s;font-family:inherit}
input:focus{outline:none;border-color:var(--blue);box-shadow:0 0 0 3px rgba(26,86,219,.1)}
.btn-row{margin-top:6px}
.btn{display:inline-flex;align-items:center;gap:6px;padding:10px 22px;font-size:.92rem;font-weight:600;border:none;border-radius:6px;cursor:pointer;transition:background .15s,transform .1s;font-family:inherit}
.btn:active{transform:scale(.98)}
.btn-primary{background:var(--blue);color:#fff}
.btn-primary:hover{background:var(--blue-hover)}
.error-notice{background:var(--amber-bg);color:var(--amber);padding:10px 14px;border-radius:6px;margin-bottom:14px;font-size:.88rem;font-weight:500}
select{padding:6px 10px;font-size:.88rem;border:1px solid var(--border);border-radius:6px;background:var(--surface);color:var(--text);font-family:inherit}
.toggle-group{display:inline-flex;border:1px solid var(--border);border-radius:6px;overflow:hidden}
.toggle-btn{padding:5px 12px;font-size:.82rem;font-weight:500;border:none;background:var(--surface);color:var(--muted);cursor:pointer;font-family:inherit;transition:all .15s}
.toggle-btn:not(:last-child){border-right:1px solid var(--border)}
.toggle-btn:hover{background:#f1f5f9}
.toggle-btn.active{background:var(--blue);color:#fff}
.table-wrap{overflow-x:auto}
.data-table{width:100%;border-collapse:collapse;font-size:.88rem}
.data-table th{text-align:left;font-size:.72rem;font-weight:600;text-transform:uppercase;letter-spacing:.05em;color:var(--muted);padding:7px 10px;border-bottom:2px solid var(--border)}
.data-table td{padding:9px 10px;border-bottom:1px solid var(--border);vertical-align:middle}
.data-table tr:last-child td{border-bottom:none}
.data-table tr:hover td{background:#f9fafb}
.data-table code{font-family:"SF Mono","Cascadia Code","Fira Code",Consolas,monospace;font-size:.8rem;background:#f1f5f9;padding:1px 5px;border-radius:3px}
.chart-legend{display:flex;flex-wrap:wrap;gap:8px;margin-bottom:8px}
.legend-item{display:inline-flex;align-items:center;gap:6px;font-size:.78rem;color:var(--text)}
.legend-swatch{display:inline-block;width:12px;height:12px;border-radius:3px;flex-shrink:0}
.bars{display:flex;align-items:flex-end;gap:4px;height:200px;padding-top:8px}
.bar-group{flex:1;display:flex;flex-direction:column;align-items:center;height:100%;justify-content:flex-end;min-width:0}
.bar-stack{display:flex;flex-direction:column;justify-content:flex-end;width:100%;max-width:48px;height:100%}
.bar-segment{width:100%;min-height:1px;transition:height .3s}
.bar-segment:first-child{border-radius:3px 3px 0 0}
.bar-label{font-size:.65rem;color:var(--muted);margin-top:4px;white-space:nowrap;overflow:hidden;text-overflow:ellipsis;max-width:100%;text-align:center}
@media(max-width:600px){.container{padding:14px 12px 48px}.card{padding:16px}.summary-cards{grid-template-columns:1fr 1fr}.bars{height:150px}}`
}
