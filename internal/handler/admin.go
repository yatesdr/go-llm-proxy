package handler

import (
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/url"

	"go-llm-proxy/internal/config"
	"go-llm-proxy/internal/httputil"
	"go-llm-proxy/internal/ratelimit"
	"go-llm-proxy/internal/usage"
)

const adminCookieName = "admin_auth"

// AdminHandler serves the /admin/* pages. It shares the usage-dashboard
// password for authentication and uses its own session store scoped to
// Path=/admin.
type AdminHandler struct {
	cs       *config.ConfigStore
	rl       *ratelimit.RateLimiter
	health   *config.HealthStore
	ul       *usage.UsageLogger // may be nil when metrics logging is off
	sessions *sessionStore
}

func NewAdminHandler(cs *config.ConfigStore, rl *ratelimit.RateLimiter, hs *config.HealthStore, ul *usage.UsageLogger) *AdminHandler {
	return &AdminHandler{cs: cs, rl: rl, health: hs, ul: ul, sessions: newSessionStore()}
}

// enabled reports whether the admin UI is switched on in the current config.
// Routes are always mounted; a 404 is served until a password exists, so the
// UI can be enabled by config reload without restarting.
func (h *AdminHandler) enabled() bool {
	return h.cs.Get().EffectiveAdminPassword() != ""
}

// Root redirects /admin → /admin/users.
func (h *AdminHandler) Root(w http.ResponseWriter, r *http.Request) {
	if !h.enabled() {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}

// LoginPage renders the login form (or redirects to /admin/users when the
// caller already has a valid session cookie).
func (h *AdminHandler) LoginPage(w http.ResponseWriter, r *http.Request) {
	if !h.enabled() {
		http.NotFound(w, r)
		return
	}
	if h.authed(r) {
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
		return
	}
	h.renderLogin(w, "")
}

// HandleLogin verifies the submitted password and issues a session cookie.
func (h *AdminHandler) HandleLogin(w http.ResponseWriter, r *http.Request) {
	if !h.enabled() {
		http.NotFound(w, r)
		return
	}
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
	cfg := h.cs.Get()
	adminPassword := cfg.EffectiveAdminPassword()
	if adminPassword == "" || !constantTimeEqual(password, adminPassword) {
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
	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}

// HandleLogout revokes the caller's session and clears the cookie.
func (h *AdminHandler) HandleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(adminCookieName); err == nil {
		h.sessions.revoke(cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     adminCookieName,
		Value:    "",
		Path:     "/admin",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   requestIsTLS(r, h.rl),
		MaxAge:   -1,
	})
	http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
}

func (h *AdminHandler) authed(r *http.Request) bool {
	cookie, err := r.Cookie(adminCookieName)
	if err != nil {
		return false
	}
	return h.sessions.validate(cookie.Value)
}

func (h *AdminHandler) setCookie(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     adminCookieName,
		Value:    token,
		Path:     "/admin",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   requestIsTLS(r, h.rl),
		MaxAge:   int(dashboardSessionTTL.Seconds()),
	})
}

// RequirePage wraps a handler that renders an HTML page. Unauthenticated
// requests are redirected to /admin/login.
func (h *AdminHandler) RequirePage(inner http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.enabled() {
			http.NotFound(w, r)
			return
		}
		if !h.authed(r) {
			http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
			return
		}
		inner(w, r)
	}
}

// RequireAPI wraps a JSON endpoint. Unauthenticated requests get a 401 JSON
// error. For POST mutations it also verifies the Origin/Referer header
// matches the server's own host — defense in depth beside SameSite=Strict.
func (h *AdminHandler) RequireAPI(inner http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.enabled() {
			http.NotFound(w, r)
			return
		}
		if !h.authed(r) {
			httputil.WriteError(w, http.StatusUnauthorized, "not authenticated")
			return
		}
		if r.Method == http.MethodPost && !originMatches(r) {
			httputil.WriteError(w, http.StatusForbidden, "cross-origin request rejected")
			return
		}
		inner(w, r)
	}
}

// originMatches returns true when the request's Origin or Referer header's
// host matches the request Host. A request with neither header set (e.g.
// curl) is rejected.
func originMatches(r *http.Request) bool {
	raw := r.Header.Get("Origin")
	if raw == "" {
		raw = r.Header.Get("Referer")
	}
	if raw == "" {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if u.Host == "" {
		return false
	}
	return u.Host == r.Host
}

// decodeJSONBody decodes a small JSON body into dst. Bodies larger than
// limit bytes are rejected to prevent memory exhaustion.
func decodeJSONBody(r *http.Request, dst any, limit int64) error {
	r.Body = http.MaxBytesReader(nil, r.Body, limit)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	// Ensure only one JSON object in the body.
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return err
	}
	return nil
}

// writeJSON emits a JSON response with security headers applied.
func writeJSON(w http.ResponseWriter, status int, payload any) {
	httputil.SetSecurityHeaders(w)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// ─── HTML rendering ──────────────────────────────────────────────────────────

func (h *AdminHandler) renderLogin(w http.ResponseWriter, errMsg string) {
	httputil.SetSecurityHeaders(w)
	w.Header().Set("Content-Security-Policy", cspWithFonts)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	errNotice := ""
	if errMsg != "" {
		errNotice = `<div class="error-notice">` + html.EscapeString(errMsg) + `</div>`
	}
	_, _ = w.Write([]byte(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Admin</title>
` + fontLinks + `
<style>` + dashboardCSS() + adminCSS() + `</style>
</head>
<body class="auth-body">
<div class="auth-card">
` + eidonixMark("auth-mark") + `
<div class="auth-title">EIDONIX</div>
<div class="auth-sub">LLM Proxy · Admin</div>
` + errNotice + `
<form method="POST" action="/admin/login">
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

// renderShell emits the HTML for an /admin tab: header + nav + body + inline JS.
// bodyHTML is inserted into a <div class="container">; scriptJS runs after the DOM is ready.
func (h *AdminHandler) renderShell(w http.ResponseWriter, activeTab, title, bodyHTML, scriptJS string) {
	httputil.SetSecurityHeaders(w)
	w.Header().Set("Content-Security-Policy", cspWithFonts)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	tab := func(id, label, href string) string {
		cls := "admin-tab"
		if id == activeTab {
			cls += " active"
		}
		return `<a class="` + cls + `" href="` + href + `">` + html.EscapeString(label) + `</a>`
	}

	_, _ = io.WriteString(w, `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>`+html.EscapeString(title)+`</title>
`+fontLinks+`
<style>`+dashboardCSS()+adminCSS()+`</style>
</head>
<body>
<div class="shell">
<div class="topbar">
  <div class="topbar-inner">
    `+topbarBrandProduct("/admin", "LLM Proxy")+`
    <div class="topbar-right">
      <a class="nav-ext" href="/usage">Usage</a>
      <a class="nav-ext" href="/">Setup</a>
      <form method="POST" action="/admin/logout">
        <button class="btn-logout" type="submit">Sign out</button>
      </form>
    </div>
  </div>
</div>
<nav class="tabs-bar">`+tab("users", "API Keys", "/admin/users")+tab("chat", "LLM", "/admin/chat")+tab("audio", "Audio", "/admin/audio")+tab("documents", "Documents", "/admin/documents")+`</nav>
<div class="content"><div class="container">
`+bodyHTML+adminConfirmModalHTML+`
</div></div>
</div>
<script>`+adminClientJS()+scriptJS+`</script>
</body>
</html>`)
}

// adminConfirmModalHTML is a shared confirmation dialog available on every
// admin tab via openConfirm()/closeConfirm() in adminClientJS.
const adminConfirmModalHTML = `<div id="confirmModal" class="modal-backdrop" onclick="if(event.target.id==='confirmModal')closeConfirm()">
  <div class="modal" style="max-width:440px" role="alertdialog">
    <div class="modal-header"><h2 id="confirmTitle"></h2><button class="modal-close" type="button" onclick="closeConfirm()">&times;</button></div>
    <div class="modal-body">
      <p id="confirmBody" style="font-size:13px;line-height:1.6"></p>
      <p class="hint" id="confirmActivity" style="margin-top:8px"></p>
    </div>
    <div class="modal-footer"><button type="button" class="btn btn-secondary" onclick="closeConfirm()">Cancel</button><button id="confirmBtn" class="btn btn-danger" type="button"></button></div>
  </div>
</div>`

// adminClientJS contains shared client helpers used by every tab.
func adminClientJS() string {
	return `
function esc(s){var d=document.createElement("div");d.textContent=s==null?"":String(s);return d.innerHTML;}
function escAttr(s){ return esc(s).replace(/\\/g, "\\\\").replace(/'/g, "\\'"); }
function flash(msg, kind){
  var bar = document.getElementById("flashBar");
  if(!bar){
    bar = document.createElement("div");
    bar.id = "flashBar";
    bar.className = "flash-bar";
    document.querySelector(".container").prepend(bar);
  }
  bar.textContent = msg;
  bar.className = "flash-bar flash-" + (kind||"info");
  bar.style.display = "block";
  if(kind !== "persistent"){
    setTimeout(function(){ bar.style.display = "none"; }, 3500);
  }
}
function showPersistentBanner(html){
  var bar = document.getElementById("persistBar");
  if(!bar){
    bar = document.createElement("div");
    bar.id = "persistBar";
    bar.className = "persist-bar";
    document.querySelector(".container").prepend(bar);
  }
  bar.innerHTML = html;
  bar.style.display = "block";
}
function dismissPersistent(){
  var bar = document.getElementById("persistBar");
  if(bar) bar.style.display = "none";
}
function apiPost(url, body){
  return fetch(url, {
    method: "POST",
    headers: {"Content-Type":"application/json"},
    credentials: "same-origin",
    body: JSON.stringify(body)
  }).then(function(r){
    return r.json().then(function(j){ return {ok: r.ok, status: r.status, json: j}; });
  });
}
function apiGet(url){
  return fetch(url, {credentials:"same-origin"}).then(function(r){
    if(!r.ok) throw new Error("HTTP "+r.status);
    return r.json();
  });
}
// attachFilter wires a text input to hide non-matching rows of a tbody.
// Rows marked data-detail="1" follow the visibility of the row above them
// (used by the chat page's expandable backend rows; data-open tracks state).
function attachFilter(inputId, tbodyId){
  var inp = document.getElementById(inputId);
  if(!inp) return;
  inp.addEventListener("input", function(){ applyFilter(inputId, tbodyId); });
}
function applyFilter(inputId, tbodyId){
  var inp = document.getElementById(inputId);
  var q = (inp.value||"").toLowerCase();
  var rows = document.getElementById(tbodyId).rows;
  for(var i=0;i<rows.length;i++){
    var r = rows[i];
    if(r.dataset.detail !== undefined) continue;
    var match = !q || r.textContent.toLowerCase().indexOf(q) >= 0;
    r.style.display = match ? "" : "none";
    var next = rows[i+1];
    if(next && next.dataset.detail !== undefined){
      next.style.display = (match && next.dataset.open === "1") ? "" : "none";
    }
  }
}
function reapplyFilter(inputId, tbodyId){
  var inp = document.getElementById(inputId);
  if(inp && inp.value) applyFilter(inputId, tbodyId);
}
var __confirmFn = null;
function openConfirm(title, bodyHTML, hintText, btnLabel, fn){
  document.getElementById("confirmTitle").textContent = title;
  document.getElementById("confirmBody").innerHTML = bodyHTML;
  document.getElementById("confirmActivity").textContent = hintText || "";
  var btn = document.getElementById("confirmBtn");
  btn.textContent = btnLabel;
  __confirmFn = fn;
  document.getElementById("confirmModal").classList.add("open");
  btn.focus();
}
function closeConfirm(){
  document.getElementById("confirmModal").classList.remove("open");
  __confirmFn = null;
}
document.addEventListener("DOMContentLoaded", function(){
  var btn = document.getElementById("confirmBtn");
  if(btn) btn.addEventListener("click", function(){ var fn = __confirmFn; closeConfirm(); if(fn) fn(); });
});
document.addEventListener("keydown", function(e){ if(e.key === "Escape") closeConfirm(); });
var __tipEl=null;
function showTip(t){
  hideTip();
  var txt=t.getAttribute("data-tip"); if(!txt) return;
  __tipEl=document.createElement("div"); __tipEl.className="tooltip-pop"; __tipEl.textContent=txt;
  document.body.appendChild(__tipEl);
  var r=t.getBoundingClientRect();
  var top=r.top-8-__tipEl.offsetHeight, left=r.left+r.width/2-__tipEl.offsetWidth/2;
  if(top<4) top=r.bottom+8;
  if(left<4) left=4;
  var maxLeft=window.innerWidth-__tipEl.offsetWidth-4;
  if(left>maxLeft) left=maxLeft;
  __tipEl.style.top=top+"px"; __tipEl.style.left=left+"px";
}
function hideTip(){ if(__tipEl){ __tipEl.remove(); __tipEl=null; } }
document.addEventListener("mouseover", function(e){ var t=e.target.closest(".tip"); if(t) showTip(t); });
document.addEventListener("mouseout", function(e){ var t=e.target.closest(".tip"); if(t) hideTip(); });
document.addEventListener("focusin", function(e){ var t=e.target.closest(".tip"); if(t) showTip(t); });
document.addEventListener("focusout", function(e){ var t=e.target.closest(".tip"); if(t) hideTip(); });
function copyToClipboard(txt){
  if(navigator.clipboard && navigator.clipboard.writeText){
    return navigator.clipboard.writeText(txt);
  }
  var ta = document.createElement("textarea");
  ta.value = txt; document.body.appendChild(ta); ta.select();
  try { document.execCommand("copy"); } finally { document.body.removeChild(ta); }
  return Promise.resolve();
}
`
}

// adminCSS is appended to dashboardCSS() for admin-specific widgets.
func adminCSS() string {
	return `
.cascade-list{margin-bottom:10px}
.cascade-row{display:flex;align-items:center;gap:10px;padding:6px 10px;border:1px solid var(--hairline);border-radius:4px;background:var(--panel);margin-bottom:4px}
.cascade-rank{font-family:var(--font-mono);font-size:12px;color:var(--steel);width:16px;text-align:center;flex-shrink:0}
.cascade-label{flex:1;font-size:13px;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.cascade-actions{display:flex;align-items:center;gap:10px;flex-shrink:0}
.cascade-spacer{display:inline-block;width:11px}
.cascade-add{display:flex;gap:8px;align-items:center}
.cascade-add select{max-width:260px}
.subtabs{display:flex;gap:4px;margin-bottom:12px;border-bottom:1px solid var(--hairline)}
.subtab{display:inline-flex;align-items:center;height:32px;padding:0 12px;font-size:12px;font-weight:500;letter-spacing:.06em;text-transform:uppercase;color:var(--steel);background:none;border:none;cursor:pointer;border-bottom:2px solid transparent;margin-bottom:-1px;font-family:var(--font-ui)}
.subtab:hover{color:var(--black)}
.subtab.active{color:var(--black);border-bottom-color:var(--black);font-weight:600}
.toolbar{display:flex;align-items:center;gap:8px;margin-bottom:12px}
.toolbar h2{margin:0;flex:1;font-size:13px;font-weight:700;text-transform:uppercase;letter-spacing:.06em}
.toolbar h4{font-size:11px;font-weight:600;text-transform:uppercase;letter-spacing:.08em;color:var(--black);flex:1}
.card .toolbar{margin-bottom:8px}
.btn-link{background:transparent;border:none;color:var(--blue);cursor:pointer;padding:0;font-size:inherit;font-weight:500;font-family:inherit;text-decoration:none;text-align:left}
.btn-link:hover{text-decoration:underline}
.editable{color:var(--black);border-bottom:1px dotted #999;cursor:text}
.editable:hover{border-bottom-color:var(--black)}
.editable-input{font:inherit;font-size:13px;color:var(--black);border:none;border-bottom:1px solid var(--blue);background:transparent;padding:0;outline:none;width:150px}
.btn-secondary{background:#fff;color:#333;border-color:#CCC}
.btn-secondary:hover{background:#E6E6E6;border-color:#ADADAD}
.btn-danger{background:#D9534F;color:#fff;border-color:#D43F3A}
.btn-danger:hover{background:#C9302C;border-color:#AC2925}
/* row actions: neutral gray, destructive stays red */
.row-actions .btn-secondary{background:#EFEFEF;border-color:#C8C8C8;color:#111}
.row-actions .btn-secondary:hover{background:#DEDEDE;border-color:#ADADAD}
.btn-sm{height:24px;padding:0 10px;font-size:12px;border-radius:3px}
.btn-icon{padding:0 7px}
.copy-icon{background:none;border:none;cursor:pointer;color:var(--steel);padding:2px;vertical-align:middle;line-height:1}
.copy-icon:hover{color:var(--black)}
.btn-icon svg{display:block}
.pill{display:inline-flex;align-items:center;gap:4px;background:var(--paper);border:1px solid var(--hairline);color:var(--steel);padding:1px 3px 1px 7px;font-size:11px;font-family:var(--font-mono);white-space:nowrap}
.pill-unrestricted{border-color:var(--hairline);color:var(--quiet);font-family:var(--font-ui);padding-right:7px}
.pill-x{background:transparent;border:none;padding:0 4px 0 2px;color:var(--steel);cursor:pointer;font-size:12px;line-height:1;opacity:.7}
.pill-x:hover{opacity:1;color:var(--red)}
.pill-add{display:inline-block;padding:1px 7px;font-size:11px;border:1px dashed var(--border);color:var(--steel);background:transparent;cursor:pointer}
.pill-add:hover{border-color:var(--blue);color:var(--blue)}
.model-input{height:24px;padding:0 8px;font-size:12px;border:1px solid var(--border);border-radius:4px;width:180px;font-family:var(--font-mono)}
.picker{position:relative;display:inline-block;margin-left:4px}
.picker-menu{position:absolute;top:calc(100% + 3px);left:0;min-width:220px;max-height:240px;overflow-y:auto;background:#fff;border:1px solid #C8C8C8;border-radius:4px;box-shadow:var(--shadow-pop);z-index:60}
.picker-item{padding:6px 10px;font-family:var(--font-mono);font-size:12px;cursor:pointer;white-space:nowrap;color:var(--black)}
.picker-item:hover{background:var(--panel)}
.picker-empty{padding:6px 10px;color:var(--quiet);font-size:12px}
.row-actions{text-align:right;white-space:nowrap}
.row-actions .action-group{display:inline-flex;gap:4px;align-items:center;vertical-align:middle;flex-wrap:nowrap;justify-content:flex-end}
/* Admin tables: rows may grow with wrapped pill lists, but stay dense. */
.data-table td{height:auto;min-height:36px;padding:6px 10px;overflow:visible}
.data-table .cell-key code{white-space:nowrap}
.pills-wrap{display:flex;flex-wrap:wrap;align-items:center;gap:4px;row-gap:4px}
.data-table .cell-pills{max-width:none}
.filter-row{display:flex;justify-content:flex-end;margin-bottom:8px}
.model-row{cursor:pointer}
.chevron{color:var(--quiet);font-size:10px;user-select:none}
.sub-table{width:100%;border-collapse:collapse;font-size:12px;background:var(--panel);border:1px solid var(--hairline)}
.sub-table td{padding:5px 10px;border-bottom:1px solid var(--hairline);vertical-align:middle;white-space:nowrap;font-family:var(--font-mono)}
.sub-table td:first-child{width:40%;white-space:normal}
.sub-table tr:last-child td{border-bottom:none}
/* toasts (flash) — bottom-right per style guide */
.flash-bar{position:fixed;right:16px;bottom:16px;z-index:200;background:#000;color:#fff;padding:10px 16px;font-size:13px;font-weight:500;display:none;box-shadow:var(--shadow-pop);max-width:420px}
.flash-success{background:#000;color:#fff}
.flash-error{background:var(--red);color:#fff}
.flash-info{background:#000;color:#fff}
.persist-bar{padding:12px 14px;margin-bottom:12px;background:var(--paper);border:1px solid var(--black);display:none;font-size:13px}
.persist-bar code{display:block;background:#000;color:#fff;padding:8px 12px;margin:6px 0;font-family:var(--font-mono);font-size:12px;word-break:break-all}
.persist-bar .persist-actions{display:flex;gap:8px;margin-top:6px}
.mono{font-family:var(--font-mono);font-size:12px;color:var(--steel)}
/* status glyphs: round dot — green ok, orange degraded, red down */
.health-dot{display:inline-block;width:9px;height:9px;border-radius:50%;background:var(--quiet);vertical-align:middle;margin-right:6px;box-sizing:border-box}
.health-online{background:var(--ok)}
.health-degraded{background:var(--warn)}
.health-offline{background:var(--red)}
.health-unknown{background:transparent;border:1px solid var(--quiet)}
.st-word{font-size:11px;font-weight:600}
.modal-backdrop{position:fixed;inset:0;background:rgba(0,0,0,.5);display:none;align-items:flex-start;justify-content:center;z-index:50;overflow-y:auto;padding:48px 16px}
.modal-backdrop.open{display:flex}
.modal{background:var(--paper);border:1px solid var(--black);border-radius:6px;box-shadow:var(--shadow-pop);width:100%;max-width:640px;max-height:calc(100vh - 96px);overflow-y:auto}
.modal::before{content:"";display:block;height:4px;background:var(--accent)}
.modal-header{padding:12px 16px;border-bottom:1px solid var(--hairline);display:flex;align-items:center;gap:12px}
.modal-header h2{flex:1;margin:0;font-size:13px;font-weight:700;text-transform:uppercase;letter-spacing:.06em}
.modal-body{padding:16px}
.modal-footer{padding:12px 16px;border-top:1px solid var(--hairline);display:flex;justify-content:flex-end;gap:8px;background:var(--panel)}
.modal-close{background:transparent;border:none;font-size:20px;line-height:1;color:var(--steel);cursor:pointer;padding:0 4px}
.modal-close:hover{color:var(--black)}
.section{margin-bottom:24px}
.section h3{font-size:13px;font-weight:700;text-transform:uppercase;letter-spacing:.06em;color:var(--black);margin-bottom:8px;padding-bottom:4px;border-bottom:1px solid var(--hairline)}
.section.section-required h3{border-bottom-color:var(--black)}
.section-divider{border:none;border-top:1px dashed var(--hairline);margin:24px 0 16px;position:relative}
.section-divider::before{content:"Optional";position:absolute;top:-10px;left:50%;transform:translateX(-50%);background:var(--paper);color:var(--steel);font-size:11px;font-weight:600;letter-spacing:.08em;padding:0 10px;text-transform:uppercase}
.field-grid{display:grid;grid-template-columns:1fr 1fr;gap:12px 16px}
.grid-4{display:grid;grid-template-columns:repeat(4,1fr);gap:12px 16px}
.grid-4 .field{margin-bottom:0}
.grid-3{display:grid;grid-template-columns:repeat(3,1fr);gap:6px;margin-top:4px}
.grid-3 .field{margin-bottom:0}
.grid-3 select{padding:0 4px;font-size:12px}
.grid-3 label{font-size:10px;margin-bottom:1px}
.be-head,.be-row{display:grid;grid-template-columns:16px minmax(200px,2fr) minmax(160px,1.2fr) 60px 64px 36px 96px;gap:8px;align-items:center}
.be-head{padding:0 9px;margin-bottom:4px}
.be-head span{font-size:11px;font-weight:600;text-transform:uppercase;letter-spacing:.08em;color:var(--steel)}
.be-row-wrap{border:1px solid var(--hairline);border-radius:4px;background:var(--panel);padding:6px 8px;margin-bottom:6px}
.be-row input{height:28px;background:#fff}
.be-row input[type=number]{padding:0 6px}
.be-row input[type=checkbox]{height:auto;width:14px;margin:0;accent-color:var(--black)}
.be-keywrap{display:flex;align-items:center;gap:2px}
.be-keywrap .be-key{flex:1;min-width:0}
.be-actions{display:flex;gap:4px;justify-content:flex-end}
.be-note{margin-top:4px;font-size:12px;padding-left:24px}
.field-grid .field{margin-bottom:0}
.field-grid .field-full{grid-column:1/-1}
.field label{display:flex;align-items:center;gap:5px}
.tip{display:inline-flex;align-items:center;justify-content:center;width:14px;height:14px;background:var(--hairline);color:var(--steel);font-size:10px;font-weight:600;cursor:help;position:relative}
.tooltip-pop{position:fixed;background:#000;color:#fff;padding:6px 10px;font-size:12px;font-weight:400;white-space:normal;max-width:260px;z-index:9999;line-height:1.45;box-shadow:var(--shadow-pop);border-radius:4px;text-transform:none;letter-spacing:0;pointer-events:none}
.checkbox-row{display:flex;align-items:center;gap:8px;padding-top:22px}
.checkbox-row input[type=checkbox]{width:14px;height:14px;margin:0;accent-color:var(--black)}
.checkbox-row label{display:inline;font-size:13px;color:var(--black);font-weight:400;letter-spacing:0;text-transform:none;margin:0}
.secret-row{display:flex;gap:6px;align-items:center}
.helper-text{color:var(--steel);font-size:12px;line-height:16px;margin:0;font-weight:400;text-transform:none;letter-spacing:0}
.toolbar .helper-text{margin-top:1px}
.empty-cell{text-align:center;color:var(--steel)}
.workload-card+.workload-card{margin-top:16px}
.backend-disabled{opacity:.55}
.drain-badge{display:inline-block;padding:0 6px;border:1px solid var(--hairline);background:var(--paper);color:var(--steel);font-size:11px;font-weight:600;text-transform:uppercase;letter-spacing:.05em}
.secret-row .mono{flex:1}
.inline-err{color:var(--red);font-size:12px;margin-top:3px}
.hint{font-size:12px;line-height:16px;color:var(--quiet)}
@media(max-width:900px){
  .tabs-bar{overflow-x:auto}
  .admin-tab{white-space:nowrap}
  .toolbar{align-items:flex-start;flex-wrap:wrap}
  .field-grid{grid-template-columns:1fr}
  .field-grid .field-full{grid-column:1}
  .modal-backdrop{padding:12px}
  .modal{max-height:calc(100vh - 24px)}
  .checkbox-row{padding-top:8px}
}
`
}
