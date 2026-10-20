package handler

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"go-llm-proxy/internal/config"
	"go-llm-proxy/internal/httputil"
	"go-llm-proxy/internal/mailer"
	"go-llm-proxy/internal/ratelimit"
)

// Self-service enrollment (/join): a person opens a join link, enters a work
// email in one of the join key's domains, opens the one-time link emailed to
// them and is shown a key of their own — or a new one for the key they
// already have. Every submission gets the same answer, so the pages can't be
// used to learn which addresses or domains can get a key.

// confirmTTL is how long an emailed confirmation link works.
const confirmTTL = 30 * time.Minute

// Rate limits for the public pages. Submissions are limited hard because
// each one can send an email.
const (
	enrollLookupsPerIP  = 120 // page views per IP per hour
	enrollSubmitsPerIP  = 10  // join submissions per IP per hour
	enrollMailsPerEmail = 3   // confirmation emails per address per hour
	enrollSubmitsPerJK  = 200 // join submissions per join key per hour
	enrollConfirmsPerIP = 20  // key creations per IP per hour
	maxPendingConfirms  = 10000
)

// EnrollHandler serves the public /join pages.
type EnrollHandler struct {
	cs     *config.ConfigStore
	rl     *ratelimit.RateLimiter // client IP resolution behind trusted proxies
	mailer mailer.Sender
	tokens *confirmStore

	lookups, submits, mails, perJoinKey, confirms *ratelimit.Window

	// async runs mail delivery off the request path so the answer takes
	// the same time whatever the address; tests run it inline.
	async func(func())
}

// NewEnrollHandler returns the /join handler. rl supplies the trusted-proxy
// list used to find the client IP.
func NewEnrollHandler(cs *config.ConfigStore, rl *ratelimit.RateLimiter, m mailer.Sender) *EnrollHandler {
	return &EnrollHandler{
		cs:         cs,
		rl:         rl,
		mailer:     m,
		tokens:     newConfirmStore(),
		lookups:    ratelimit.NewWindow(enrollLookupsPerIP, time.Hour),
		submits:    ratelimit.NewWindow(enrollSubmitsPerIP, time.Hour),
		mails:      ratelimit.NewWindow(enrollMailsPerEmail, time.Hour),
		perJoinKey: ratelimit.NewWindow(enrollSubmitsPerJK, time.Hour),
		confirms:   ratelimit.NewWindow(enrollConfirmsPerIP, time.Hour),
		async:      func(fn func()) { go fn() },
	}
}

// Mount registers the public, unauthenticated /join routes.
func (h *EnrollHandler) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /join", h.PublicJoinPage)
	mux.HandleFunc("POST /join", h.PublicJoinSubmit)
	mux.HandleFunc("GET /join/{key}", h.JoinPage)
	mux.HandleFunc("POST /join/{key}", h.JoinSubmit)
	mux.HandleFunc("GET /join/confirm/{token}", h.ConfirmPage)
	mux.HandleFunc("POST /join/confirm/{token}", h.ConfirmSubmit)
}

// ─── Pending confirmations ───────────────────────────────────────────────────

// confirmStore holds the emailed one-time links in memory, keyed by the
// token's hash. A restart invalidates them, which only means asking again.
type confirmStore struct {
	mu  sync.Mutex
	m   map[string]pendingConfirm
	now func() time.Time
}

type pendingConfirm struct {
	email     string
	joinKeyID string
	expires   time.Time
}

func newConfirmStore() *confirmStore {
	return &confirmStore{m: make(map[string]pendingConfirm), now: time.Now}
}

func tokenID(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// create returns a new token for email, or "" when the store is full.
func (s *confirmStore) create(email, joinKeyID string) string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	token := base64.RawURLEncoding.EncodeToString(buf)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if len(s.m) >= maxPendingConfirms {
		for k, p := range s.m {
			if !now.Before(p.expires) {
				delete(s.m, k)
			}
		}
		if len(s.m) >= maxPendingConfirms {
			return ""
		}
	}
	s.m[tokenID(token)] = pendingConfirm{email: email, joinKeyID: joinKeyID, expires: now.Add(confirmTTL)}
	return token
}

// peek returns the pending confirmation without using it up.
func (s *confirmStore) peek(token string) (pendingConfirm, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.m[tokenID(token)]
	if !ok || !s.now().Before(p.expires) {
		return pendingConfirm{}, false
	}
	return p, true
}

// take uses the token up and returns its confirmation.
func (s *confirmStore) take(token string) (pendingConfirm, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := tokenID(token)
	p, ok := s.m[id]
	delete(s.m, id)
	if !ok || !s.now().Before(p.expires) {
		return pendingConfirm{}, false
	}
	return p, true
}

// restore puts back a token whose use failed, so the person can retry.
func (s *confirmStore) restore(token string, p pendingConfirm) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[tokenID(token)] = p
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

// activeJoinKey returns the join key for a link secret when enrollment is
// ready, not paused, and the join key itself is not paused.
func activeJoinKey(cfg *config.Config, key string) *config.JoinKeyConfig {
	if !enrollmentOpen(cfg) {
		return nil
	}
	jk := cfg.Enrollment.FindJoinKeyByKey(key)
	if jk == nil || jk.Paused {
		return nil
	}
	return jk
}

// publicHost is the host name shown on the pages and in email.
func publicHost(cfg *config.Config) string {
	if u, err := url.Parse(cfg.Enrollment.PublicURL); err == nil && u.Host != "" {
		return u.Host
	}
	return "this server"
}

// MailSettings builds the relay settings from config, reading the password
// file when one is set.
func MailSettings(cfg *config.Config) (mailer.Settings, error) {
	s := cfg.Enrollment.SMTP
	pw, err := s.ResolvePassword()
	if err != nil {
		return mailer.Settings{}, err
	}
	return mailer.Settings{
		Host: s.Host, Port: s.EffectivePort(), Username: s.Username, Password: pw,
		From: s.From, ReplyTo: s.ReplyTo, STARTTLS: s.StartTLS(),
	}, nil
}

func (h *EnrollHandler) clientIP(r *http.Request) string { return ratelimit.ClientIP(h.rl, r) }

// ─── Pages ───────────────────────────────────────────────────────────────────

// enrollmentOpen reports whether join links work at all right now.
func enrollmentOpen(cfg *config.Config) bool {
	return !cfg.Enrollment.Paused && cfg.Enrollment.Ready()
}

// PublicJoinPage serves GET /join (code-link's "Get a key" link): the email
// form when a public join key is active, otherwise where to get a link.
func (h *EnrollHandler) PublicJoinPage(w http.ResponseWriter, r *http.Request) {
	if !h.lookups.Allow(h.clientIP(r)) {
		h.tooMany(w)
		return
	}
	cfg := h.cs.Get()
	if !enrollmentOpen(cfg) || !cfg.Enrollment.HasPublicJoinKey() {
		h.noPublicJoin(w)
		return
	}
	h.joinForm(w, http.StatusOK, cfg, nil, "", "")
}

// PublicJoinSubmit serves POST /join: the email's domain picks the active
// public join key; the answer is the same whatever the address.
func (h *EnrollHandler) PublicJoinSubmit(w http.ResponseWriter, r *http.Request) {
	if !h.submits.Allow(h.clientIP(r)) {
		h.tooMany(w)
		return
	}
	cfg := h.cs.Get()
	if !enrollmentOpen(cfg) || !cfg.Enrollment.HasPublicJoinKey() {
		h.noPublicJoin(w)
		return
	}
	if !h.perJoinKey.Allow("public") {
		h.tooMany(w)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := r.ParseForm(); err != nil {
		h.message(w, http.StatusBadRequest, "Something went wrong", "That form couldn't be read. Please go back and try again.")
		return
	}
	h.submit(w, r, cfg, nil, cfg.Enrollment.PublicJoinKeyFor)
}

func (h *EnrollHandler) noPublicJoin(w http.ResponseWriter) {
	h.message(w, http.StatusOK, "Get your API key",
		"Ask your administrator for your team's join link.")
}

// JoinPage serves GET /join/{key}.
func (h *EnrollHandler) JoinPage(w http.ResponseWriter, r *http.Request) {
	if !h.lookups.Allow(h.clientIP(r)) {
		h.tooMany(w)
		return
	}
	cfg := h.cs.Get()
	jk := activeJoinKey(cfg, r.PathValue("key"))
	if jk == nil {
		h.inactive(w)
		return
	}
	h.joinForm(w, http.StatusOK, cfg, jk, "", "")
}

// joinForm renders the email form; jk is nil on the bare /join page, which
// names no join key or domains.
func (h *EnrollHandler) joinForm(w http.ResponseWriter, status int, cfg *config.Config, jk *config.JoinKeyConfig, email, errMsg string) {
	data := map[string]any{
		"Title": "Get your API key for " + publicHost(cfg),
		"Host":  publicHost(cfg),
		"Email": email,
		"Error": errMsg,
	}
	if jk != nil {
		var domains []string
		for _, d := range jk.Domains {
			domains = append(domains, "@"+d)
		}
		data["Name"] = jk.Name
		data["Domains"] = strings.Join(domains, " or ")
	}
	h.render(w, status, "join", data)
}

// JoinSubmit serves POST /join/{key}: emails a one-time confirmation link
// when the address may get a key, and gives the same answer either way.
func (h *EnrollHandler) JoinSubmit(w http.ResponseWriter, r *http.Request) {
	if !h.submits.Allow(h.clientIP(r)) {
		h.tooMany(w)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := r.ParseForm(); err != nil {
		h.message(w, http.StatusBadRequest, "Something went wrong", "That form couldn't be read. Please go back and try again.")
		return
	}
	cfg := h.cs.Get()
	jk := activeJoinKey(cfg, r.PathValue("key"))
	if jk == nil {
		h.inactive(w)
		return
	}
	if !h.perJoinKey.Allow(jk.ID) {
		h.tooMany(w)
		return
	}
	h.submit(w, r, cfg, jk, func(email string) *config.JoinKeyConfig {
		if jk.AllowsEmail(email) {
			return jk
		}
		return nil
	})
}

// submit handles a parsed join form: pick returns the join key that admits
// the address (or nil); form is the join key the form shows (nil on /join).
func (h *EnrollHandler) submit(w http.ResponseWriter, r *http.Request, cfg *config.Config, form *config.JoinKeyConfig, pick func(email string) *config.JoinKeyConfig) {
	raw := r.PostForm.Get("email")
	email, err := config.NormalizeEmail(raw)
	if err != nil {
		h.joinForm(w, http.StatusUnprocessableEntity, cfg, form, raw, "Enter a valid email address, like name@company.com.")
		return
	}
	if jk := pick(email); jk != nil && h.mails.Allow(email) {
		if token := h.tokens.create(email, jk.ID); token != "" {
			link := strings.TrimRight(cfg.Enrollment.PublicURL, "/") + "/join/confirm/" + token
			h.sendConfirm(cfg, email, link)
		} else {
			slog.Warn("enroll: too many pending confirmations; link not sent")
		}
	}
	h.message(w, http.StatusOK, "Check your email",
		"If that address can get a key, we've sent it a link. The link works once, for 30 minutes. It can take a few minutes to arrive — check your spam folder too.")
}

func (h *EnrollHandler) sendConfirm(cfg *config.Config, email, link string) {
	host := publicHost(cfg)
	msg := mailer.Message{
		To:      email,
		Subject: "Confirm your email for " + host,
		Text: "Someone (hopefully you) asked for an API key for " + host + " with this email address.\n\n" +
			"To get your key, open this link within 30 minutes:\n\n" + link + "\n\n" +
			"The link works once. If you already have a key, you can replace it there; " +
			"nothing changes until you press the button on that page.\n\n" +
			"If you didn't ask for this, you can ignore this email.\n",
	}
	settings, err := MailSettings(cfg)
	h.async(func() {
		if err != nil {
			slog.Error("enroll: smtp settings", "error", err)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := h.mailer.Send(ctx, settings, msg); err != nil {
			slog.Error("enroll: sending confirmation email failed", "error", err)
		}
	})
}

// ConfirmPage serves GET /join/confirm/{token}. It only shows what the
// button will do: mail scanners open links, so opening one changes nothing.
func (h *EnrollHandler) ConfirmPage(w http.ResponseWriter, r *http.Request) {
	if !h.lookups.Allow(h.clientIP(r)) {
		h.tooMany(w)
		return
	}
	p, ok := h.tokens.peek(r.PathValue("token"))
	if !ok {
		h.linkExpired(w)
		return
	}
	cfg := h.cs.Get()
	jk := h.confirmJoinKey(cfg, p)
	if jk == nil {
		h.inactive(w)
		return
	}
	h.render(w, http.StatusOK, "confirm", map[string]any{
		"Title":  "Get your API key for " + publicHost(cfg),
		"Email":  p.email,
		"HasKey": config.FindKeyByEmail(cfg, p.email) != nil,
		"Host":   publicHost(cfg),
		"Name":   jk.Name,
	})
}

// confirmJoinKey re-checks, at confirmation time, that the join key the link
// was sent for is still active and still allows the address.
func (h *EnrollHandler) confirmJoinKey(cfg *config.Config, p pendingConfirm) *config.JoinKeyConfig {
	if !enrollmentOpen(cfg) {
		return nil
	}
	jk := cfg.Enrollment.FindJoinKey(p.joinKeyID)
	if jk == nil || jk.Paused || !jk.AllowsEmail(p.email) {
		return nil
	}
	return jk
}

// ConfirmSubmit serves POST /join/confirm/{token}: uses the link up, creates
// or replaces the address's key, and shows it once.
func (h *EnrollHandler) ConfirmSubmit(w http.ResponseWriter, r *http.Request) {
	if !h.confirms.Allow(h.clientIP(r)) {
		h.tooMany(w)
		return
	}
	token := r.PathValue("token")
	p, ok := h.tokens.take(token)
	if !ok {
		h.linkExpired(w)
		return
	}
	cfg := h.cs.Get()
	jk := h.confirmJoinKey(cfg, p)
	if jk == nil {
		h.inactive(w)
		return
	}
	key, replaced, err := h.cs.EnrollKey(p.email, jk.ID, jk.Models)
	if err != nil {
		slog.Error("enroll: issuing key failed", "join_key", jk.ID, "error", err)
		h.tokens.restore(token, p)
		h.message(w, http.StatusInternalServerError, "Something went wrong",
			"Your key wasn't changed. Please go back and press the button again in a minute.")
		return
	}
	slog.Info("enroll: key issued", "email", p.email, "join_key", jk.ID, "replaced", replaced,
		"hash", config.KeyHash(key)[:8])
	dl := cfg.Enrollment.Downloads.Effective()
	h.render(w, http.StatusOK, "key", map[string]any{
		"Title":      "Your API key",
		"Key":        key,
		"Replaced":   replaced,
		"Email":      p.email,
		"Host":       publicHost(cfg),
		"ServerURL":  strings.TrimRight(cfg.Enrollment.PublicURL, "/"),
		"MacOS":      dl.MacOS,
		"Windows":    dl.Windows,
		"Quickstart": dl.Quickstart,
	})
}

func (h *EnrollHandler) inactive(w http.ResponseWriter) {
	h.message(w, http.StatusNotFound, "This link isn't active",
		"Ask your administrator for a current link.")
}

func (h *EnrollHandler) linkExpired(w http.ResponseWriter) {
	h.message(w, http.StatusNotFound, "This link has expired",
		"It has already been used or is more than 30 minutes old. Open your join link again and enter your email to get a new one.")
}

func (h *EnrollHandler) tooMany(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "3600")
	h.message(w, http.StatusTooManyRequests, "Too many requests", "Please wait a while and try again.")
}

func (h *EnrollHandler) message(w http.ResponseWriter, status int, title, msg string) {
	h.render(w, status, "message", map[string]any{"Title": title, "Message": msg})
}

// ─── Rendering ───────────────────────────────────────────────────────────────

func (h *EnrollHandler) render(w http.ResponseWriter, status int, page string, data map[string]any) {
	nb := make([]byte, 16)
	_, _ = rand.Read(nb)
	nonce := hex.EncodeToString(nb)
	data["Nonce"] = nonce
	data["CSS"] = template.CSS(dashboardCSS() + enrollCSS)
	data["Mark"] = template.HTML(eidonixMark("auth-mark"))

	httputil.SetSecurityHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'nonce-"+nonce+"'; script-src 'nonce-"+nonce+
		"'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
	w.WriteHeader(status)
	if err := enrollPages.ExecuteTemplate(w, page, data); err != nil {
		slog.Warn("enroll: render", "page", page, "error", err)
	}
}

const enrollCSS = `
.auth-card{max-width:460px}
.auth-card h1{font-size:18px;font-weight:700;margin-bottom:10px;line-height:1.3}
.auth-card p{font-size:14px;line-height:1.55;margin-bottom:12px;color:var(--black)}
.auth-card .quiet{color:var(--steel);font-size:13px}
.auth-card input[type=email]{width:100%;height:36px;padding:0 10px;border:1px solid var(--border);border-radius:4px;font-size:14px;font-family:var(--font-ui)}
.full{width:100%;justify-content:center}
.key-box{display:flex;gap:6px;align-items:stretch;margin:6px 0 14px}
.key-box code{flex:1;display:block;background:var(--panel);border:1px solid var(--hairline);padding:10px;font-family:var(--font-mono);font-size:13px;word-break:break-all}
.steps{margin:0 0 14px 20px;font-size:14px;line-height:1.55}
.steps li{margin-bottom:10px}
.dl-row{display:flex;gap:8px;margin-top:6px}
.dl-row a{text-decoration:none}
.btn-secondary{background:#fff;color:#333;border-color:#CCC}
.notice{background:var(--red-wash);border:1px solid var(--red);padding:8px 10px;font-size:13px;margin-bottom:12px}
`

var enrollPages = template.Must(template.New("enroll").Parse(`
{{define "top"}}<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="referrer" content="no-referrer">
<title>{{.Title}}</title>
<style nonce="{{.Nonce}}">{{.CSS}}</style>
</head>
<body class="auth-body">
<div class="auth-card">
{{.Mark}}
{{end}}
{{define "bottom"}}</div>
</body>
</html>{{end}}

{{define "message"}}{{template "top" .}}
<h1>{{.Title}}</h1>
<p>{{.Message}}</p>
{{template "bottom" .}}{{end}}

{{define "join"}}{{template "top" .}}
<h1>{{.Title}}</h1>
<p>{{if .Name}}{{.Name}} gives each person their own key for {{.Host}}. {{end}}Enter your work email{{if .Domains}} ({{.Domains}}){{end}} and we'll send you a link to confirm it.</p>
{{if .Error}}<div class="error-notice">{{.Error}}</div>{{end}}
<form method="POST">
<div class="field">
<label for="email">Work email</label>
<input type="email" id="email" name="email" value="{{.Email}}" required autofocus autocomplete="email" maxlength="254">
</div>
<div class="btn-row"><button class="btn btn-primary full" type="submit">Email me a link</button></div>
</form>
<p class="quiet">Lost your key, or need a new one? Enter the same email: the link lets you replace it, and your old key stops working.</p>
{{template "bottom" .}}{{end}}

{{define "confirm"}}{{template "top" .}}
<h1>{{.Title}}</h1>
{{if .HasKey}}
<p><strong>{{.Email}}</strong> already has a key.</p>
<p>Press the button to get a new one. Your old key stops working straight away.</p>
<form method="POST"><div class="btn-row"><button class="btn btn-primary full" type="submit">Get a new key — your old key stops working</button></div></form>
{{else}}
<p>Create the key for <strong>{{.Email}}</strong>.</p>
<form method="POST"><div class="btn-row"><button class="btn btn-primary full" type="submit">Create my key</button></div></form>
{{end}}
<p class="quiet">This link works once. If you didn't ask for a key, close this page; nothing changes.</p>
{{template "bottom" .}}{{end}}

{{define "key"}}{{template "top" .}}
<h1>{{if .Replaced}}Your new API key{{else}}Your API key{{end}}</h1>
{{if .Replaced}}<div class="notice">Your previous key no longer works.</div>{{end}}
<p>This key is for <strong>{{.Email}}</strong> on {{.Host}}. Copy it now: it is shown only once. Keep it private.</p>
<div class="key-box"><code id="apiKey">{{.Key}}</code><button class="btn btn-secondary" type="button" id="copyBtn">Copy</button></div>
<ol class="steps">
<li>Download code-link for your computer:
<div class="dl-row"><a class="btn btn-primary" href="{{.MacOS}}">macOS</a><a class="btn btn-primary" href="{{.Windows}}">Windows</a></div></li>
<li>Open code-link and paste this key.</li>
<li>In the code-link menu, choose <strong>Launch Claude Code</strong> or <strong>Launch Codex</strong>, then pick your project folder.</li>
</ol>
<p><a href="{{.Quickstart}}">Read the quick start</a></p>
<p class="quiet">Server address, if a tool asks for it: {{.ServerURL}}<br>Lost this key? Open your join link again with the same email to get a new one.</p>
<script nonce="{{.Nonce}}">
document.getElementById("copyBtn").addEventListener("click", function(){
  var txt = document.getElementById("apiKey").textContent, btn = this;
  function done(){ btn.textContent = "Copied"; }
  if(navigator.clipboard && navigator.clipboard.writeText){ navigator.clipboard.writeText(txt).then(done); return; }
  var r = document.createRange(); r.selectNodeContents(document.getElementById("apiKey"));
  var s = window.getSelection(); s.removeAllRanges(); s.addRange(r);
  try { document.execCommand("copy"); done(); } catch(e) {}
});
</script>
{{template "bottom" .}}{{end}}
`))
