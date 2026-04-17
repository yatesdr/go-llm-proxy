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
)

const adminCookieName = "admin_auth"

// AdminHandler serves the /admin/* pages. It shares the usage-dashboard
// password for authentication and uses its own session store scoped to
// Path=/admin.
type AdminHandler struct {
	cs       *config.ConfigStore
	rl       *ratelimit.RateLimiter
	health   *config.HealthStore
	sessions *sessionStore
}

func NewAdminHandler(cs *config.ConfigStore, rl *ratelimit.RateLimiter, hs *config.HealthStore) *AdminHandler {
	return &AdminHandler{cs: cs, rl: rl, health: hs, sessions: newSessionStore()}
}

// Root redirects /admin → /admin/users.
func (h *AdminHandler) Root(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}

// LoginPage renders the login form (or redirects to /admin/users when the
// caller already has a valid session cookie).
func (h *AdminHandler) LoginPage(w http.ResponseWriter, r *http.Request) {
	if h.authed(r) {
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
		return
	}
	h.renderLogin(w, "")
}

// HandleLogin verifies the submitted password and issues a session cookie.
func (h *AdminHandler) HandleLogin(w http.ResponseWriter, r *http.Request) {
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
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; frame-ancestors 'none'")
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
<style>` + dashboardCSS() + adminCSS() + `</style>
</head>
<body>
<div class="header"><div class="header-inner"><h1>Admin</h1></div></div>
<div class="container">
<div class="card" style="max-width:420px;margin:40px auto">
<h2>Sign In</h2>
` + errNotice + `
<form method="POST" action="/admin/login">
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

// renderShell emits the HTML for an /admin tab: header + nav + body + inline JS.
// bodyHTML is inserted into a <div class="container">; scriptJS runs after the DOM is ready.
func (h *AdminHandler) renderShell(w http.ResponseWriter, activeTab, title, bodyHTML, scriptJS string) {
	httputil.SetSecurityHeaders(w)
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'")
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
<style>`+dashboardCSS()+adminCSS()+`</style>
</head>
<body>
<div class="header">
  <div class="header-inner">
    <h1>Admin</h1>
    <nav class="admin-nav">`+tab("users", "Users", "/admin/users")+tab("models", "Models", "/admin/models")+tab("processors", "Processors", "/admin/processors")+`</nav>
    <form method="POST" action="/admin/logout" style="margin-left:auto">
      <button class="btn-logout" type="submit">Sign out</button>
    </form>
  </div>
</div>
<div class="container">
`+bodyHTML+`
</div>
<script>`+adminClientJS()+scriptJS+`</script>
</body>
</html>`)
}

// adminClientJS contains shared client helpers used by every tab.
func adminClientJS() string {
	return `
function esc(s){var d=document.createElement("div");d.textContent=s==null?"":String(s);return d.innerHTML;}
function escAttr(s){ return String(s).replace(/'/g, "\\'"); }
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
.admin-nav{display:flex;gap:4px;margin-left:24px}
.admin-tab{padding:6px 14px;font-size:.88rem;font-weight:500;color:#cbd5e1;text-decoration:none;border-radius:6px;transition:all .15s}
.admin-tab:hover{background:rgba(226,232,240,0.1);color:#fff}
.admin-tab.active{background:var(--blue);color:#fff}
.toolbar{display:flex;align-items:center;gap:10px;margin-bottom:14px}
.toolbar h2{margin:0;flex:1}
.btn-link{background:transparent;border:none;color:var(--blue);cursor:pointer;padding:0;font-size:inherit;font-weight:600;font-family:inherit;text-decoration:none}
.btn-link:hover{text-decoration:underline}
.btn-secondary{background:var(--surface);color:var(--text);border:1px solid var(--border)}
.btn-secondary:hover{background:#f1f5f9}
.btn-danger{background:#dc2626;color:#fff}
.btn-danger:hover{background:#b91c1c}
.btn-sm{padding:4px 10px;font-size:.82rem}
.pill{display:inline-flex;align-items:center;gap:4px;background:#e0e7ff;color:#3730a3;padding:2px 4px 2px 8px;border-radius:12px;font-size:.78rem;margin:2px 2px 2px 0;font-weight:500;white-space:nowrap}
.pill-unrestricted{background:transparent;border:1px dashed var(--muted);color:var(--muted)}
.pill-x{background:transparent;border:none;padding:0 4px 0 2px;color:#3730a3;cursor:pointer;font-size:.9rem;line-height:1;border-radius:10px;opacity:.6}
.pill-x:hover{opacity:1;background:rgba(55,48,163,.15)}
.pill-add{display:inline-block;padding:2px 8px;font-size:.78rem;border:1px dashed #94a3b8;color:#475569;background:transparent;border-radius:12px;cursor:pointer;margin-left:4px}
.pill-add:hover{border-color:var(--blue);color:var(--blue)}
.model-input{padding:2px 6px;font-size:.8rem;border:1px solid var(--border);border-radius:10px;width:140px;margin-left:4px;font-family:inherit}
.row-actions{text-align:right;white-space:nowrap}
.row-actions .action-group{display:inline-flex;gap:6px;align-items:center;vertical-align:middle}
.flash-bar{padding:10px 14px;border-radius:6px;margin-bottom:14px;font-size:.9rem;font-weight:500;display:none}
.flash-success{background:var(--green-bg);color:var(--green)}
.flash-error{background:#fef2f2;color:#b91c1c}
.flash-info{background:#eff6ff;color:#1e40af}
.persist-bar{padding:14px;border-radius:6px;margin-bottom:14px;background:#ecfdf5;border:1px solid #34d399;display:none}
.persist-bar code{display:block;background:#0f172a;color:#e2e8f0;padding:8px 12px;margin:6px 0;font-family:"SF Mono","Cascadia Code","Fira Code",Consolas,monospace;font-size:.85rem;border-radius:4px;word-break:break-all}
.persist-bar .persist-actions{display:flex;gap:8px;margin-top:6px}
.mono{font-family:"SF Mono","Cascadia Code","Fira Code",Consolas,monospace;font-size:.85rem;color:var(--muted)}
.health-dot{display:inline-block;width:10px;height:10px;border-radius:50%;background:#9ca3af;vertical-align:middle;margin-right:6px}
.health-online{background:#10b981}
.health-offline{background:#ef4444}
.health-unknown{background:#9ca3af}
.modal-backdrop{position:fixed;inset:0;background:rgba(15,23,42,.55);display:none;align-items:flex-start;justify-content:center;z-index:50;overflow-y:auto;padding:40px 16px}
.modal-backdrop.open{display:flex}
.modal{background:var(--surface);border-radius:10px;box-shadow:0 20px 48px rgba(0,0,0,.25);width:100%;max-width:640px;max-height:calc(100vh - 80px);overflow-y:auto}
.modal-header{padding:18px 22px;border-bottom:1px solid var(--border);display:flex;align-items:center;gap:12px}
.modal-header h2{flex:1;margin:0;font-size:1.1rem}
.modal-body{padding:18px 22px}
.modal-footer{padding:14px 22px;border-top:1px solid var(--border);display:flex;justify-content:flex-end;gap:10px;background:#fafbfc;border-radius:0 0 10px 10px}
.modal-close{background:transparent;border:none;font-size:1.6rem;line-height:1;color:var(--muted);cursor:pointer;padding:0 4px}
.modal-close:hover{color:var(--text)}
.section{margin-bottom:18px}
.section h3{font-size:.78rem;font-weight:700;text-transform:uppercase;letter-spacing:.06em;color:var(--muted);margin-bottom:8px;padding-bottom:4px;border-bottom:1px solid var(--border)}
.section.section-required h3{color:var(--blue)}
.section-divider{border:none;border-top:2px dashed var(--border);margin:22px 0 18px;position:relative}
.section-divider::before{content:"Optional";position:absolute;top:-10px;left:50%;transform:translateX(-50%);background:var(--surface);color:var(--muted);font-size:.68rem;font-weight:700;letter-spacing:.1em;padding:0 12px;text-transform:uppercase}
.field-grid{display:grid;grid-template-columns:1fr 1fr;gap:12px}
.field-grid .field-full{grid-column:1/-1}
.field label{display:flex;align-items:center;gap:5px}
.tip{display:inline-flex;align-items:center;justify-content:center;width:14px;height:14px;border-radius:50%;background:var(--border);color:var(--muted);font-size:.65rem;font-weight:700;cursor:help;position:relative}
.tip:hover::after,.tip:focus::after{content:attr(data-tip);position:absolute;bottom:calc(100% + 6px);left:50%;transform:translateX(-50%);background:#0f172a;color:#e2e8f0;padding:6px 10px;border-radius:6px;font-size:.76rem;font-weight:400;white-space:normal;width:240px;z-index:60;line-height:1.45;box-shadow:0 4px 12px rgba(0,0,0,.25);text-transform:none;letter-spacing:0}
input[type="text"],input[type="number"],input[type="url"],input[type="password"]{width:100%;padding:7px 10px;font-size:.88rem;border:1px solid var(--border);border-radius:6px;background:var(--surface);color:var(--text);font-family:inherit}
select{width:100%}
.checkbox-row{display:flex;align-items:center;gap:8px;padding-top:20px}
.checkbox-row input[type=checkbox]{width:auto;margin:0}
.checkbox-row label{display:inline;font-size:.88rem;color:var(--text);font-weight:400;letter-spacing:0;text-transform:none;margin:0}
.secret-row{display:flex;gap:6px;align-items:center}
.secret-row .mono{flex:1}
.inline-err{color:#b91c1c;font-size:.78rem;margin-top:3px}
`
}
