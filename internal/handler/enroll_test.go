package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"go-llm-proxy/internal/auth"
	"go-llm-proxy/internal/config"
	"go-llm-proxy/internal/mailer"
	"go-llm-proxy/internal/ratelimit"
)

const enrollJoinSecret = "0123456789abcdef0123456789abcdef" // gitleaks:allow

const enrollHandlerFixture = `listen: ":8080"
models:
  - name: model-a
    backend: http://localhost:8000/v1
  - name: model-b
    backend: http://localhost:8001/v1
keys:
  - key: sk-admin-key
    name: admin
enrollment:
  public_url: https://llm.example.com
  smtp:
    host: smtp.example.com
    from: "LLM <no-reply@example.com>"
  join_keys:
    - id: acme
      name: Acme
      key: ` + enrollJoinSecret + `
      domains: [acme.example]
      models: [model-a]
`

// recordingMailer captures messages instead of sending them.
type recordingMailer struct {
	mu   sync.Mutex
	msgs []mailer.Message
	last mailer.Settings
}

func (m *recordingMailer) Send(_ context.Context, s mailer.Settings, msg mailer.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.msgs = append(m.msgs, msg)
	m.last = s
	return nil
}

func (m *recordingMailer) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.msgs)
}

var confirmLinkRe = regexp.MustCompile(`https://llm\.example\.com/join/confirm/([A-Za-z0-9_-]+)`)

// lastToken returns the token in the most recent confirmation email.
func (m *recordingMailer) lastToken(t *testing.T) string {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.msgs) == 0 {
		t.Fatal("no email sent")
	}
	match := confirmLinkRe.FindStringSubmatch(m.msgs[len(m.msgs)-1].Text)
	if match == nil {
		t.Fatalf("no confirmation link in %q", m.msgs[len(m.msgs)-1].Text)
	}
	return match[1]
}

type enrollEnv struct {
	h    *EnrollHandler
	cs   *config.ConfigStore
	mail *recordingMailer
	mux  *http.ServeMux
	path string
}

func newEnrollEnv(t *testing.T, fixture string) *enrollEnv {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	cs, err := config.NewConfigStore(path)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	rl := ratelimit.NewRateLimiter(nil)
	t.Cleanup(rl.Close)
	m := &recordingMailer{}
	h := NewEnrollHandler(cs, rl, m)
	h.async = func(fn func()) { fn() }
	mux := http.NewServeMux()
	h.Mount(mux)
	return &enrollEnv{h: h, cs: cs, mail: m, mux: mux, path: path}
}

var nonceRe = regexp.MustCompile(`nonce[-=]"?[0-9a-f]+`)

func (e *enrollEnv) do(t *testing.T, method, target string, form url.Values, ip string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if form != nil {
		r = httptest.NewRequest(method, target, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	if ip == "" {
		ip = "192.0.2.10"
	}
	r.RemoteAddr = ip + ":1234"
	w := httptest.NewRecorder()
	e.mux.ServeHTTP(w, r)
	return w
}

func (e *enrollEnv) join(t *testing.T, email string) *httptest.ResponseRecorder {
	return e.do(t, "POST", "/join/"+enrollJoinSecret, url.Values{"email": {email}}, "")
}

// keyWorks reports whether key authenticates on a proxied route.
func keyWorks(cs *config.ConfigStore, key string) bool {
	ok := false
	h := auth.AuthMiddleware(cs, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { ok = true }))
	r := httptest.NewRequest("GET", "/v1/models", nil)
	r.Header.Set("Authorization", "Bearer "+key)
	h.ServeHTTP(httptest.NewRecorder(), r)
	return ok
}

var shownKeyRe = regexp.MustCompile(`<code id="apiKey">([^<]+)</code>`)

func shownKey(t *testing.T, body string) string {
	t.Helper()
	m := shownKeyRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no key on page:\n%s", body)
	}
	return m[1]
}

func TestJoinPageShowsForm(t *testing.T) {
	e := newEnrollEnv(t, enrollHandlerFixture)
	w := e.do(t, "GET", "/join/"+enrollJoinSecret, nil, "")
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{"Get your API key for llm.example.com", "Acme", "@acme.example", `name="email"`, "Lost your key"} {
		if !strings.Contains(body, want) {
			t.Errorf("join page missing %q", want)
		}
	}
	csp := w.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "frame-ancestors 'none'") || !strings.Contains(csp, "'nonce-") {
		t.Errorf("CSP = %q", csp)
	}
	if w.Header().Get("X-Frame-Options") != "DENY" || w.Header().Get("Referrer-Policy") != "no-referrer" ||
		w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("security headers: %v", w.Header())
	}
}

func TestJoinInactiveLinks(t *testing.T) {
	cases := map[string]struct {
		fixture, path string
	}{
		"unknown key":     {enrollHandlerFixture, "/join/ffffffffffffffffffffffffffffffff"},
		"paused join key": {strings.Replace(enrollHandlerFixture, "      models: [model-a]\n", "      models: [model-a]\n      paused: true\n", 1), "/join/" + enrollJoinSecret},
		"global pause":    {strings.Replace(enrollHandlerFixture, "enrollment:\n", "enrollment:\n  paused: true\n", 1), "/join/" + enrollJoinSecret},
		"no public url":   {strings.Replace(enrollHandlerFixture, "  public_url: https://llm.example.com\n", "", 1), "/join/" + enrollJoinSecret},
		"no smtp":         {strings.Replace(enrollHandlerFixture, "    host: smtp.example.com\n", "", 1), "/join/" + enrollJoinSecret},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnrollEnv(t, c.fixture)
			for _, method := range []string{"GET", "POST"} {
				var form url.Values
				if method == "POST" {
					form = url.Values{"email": {"pat@acme.example"}}
				}
				w := e.do(t, method, c.path, form, "")
				if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "This link isn&#39;t active") {
					t.Errorf("%s: status %d body %s", method, w.Code, w.Body.String())
				}
			}
			if e.mail.count() != 0 {
				t.Error("an inactive link sent email")
			}
		})
	}
}

func TestJoinAnswersIdenticallyForAllowedAndDisallowed(t *testing.T) {
	e := newEnrollEnv(t, enrollHandlerFixture)
	allowed := e.join(t, "Pat@Acme.Example")
	if e.mail.count() != 1 {
		t.Fatalf("allowed address: %d emails", e.mail.count())
	}
	disallowed := e.join(t, "pat@other.example")
	subdomain := e.do(t, "POST", "/join/"+enrollJoinSecret, url.Values{"email": {"pat@mail.acme.example"}}, "192.0.2.11")
	if e.mail.count() != 1 {
		t.Fatalf("disallowed addresses sent email: %d", e.mail.count())
	}
	strip := func(w *httptest.ResponseRecorder) string { return nonceRe.ReplaceAllString(w.Body.String(), "") }
	for name, w := range map[string]*httptest.ResponseRecorder{"disallowed": disallowed, "subdomain": subdomain} {
		if w.Code != allowed.Code || strip(w) != strip(allowed) {
			t.Errorf("%s answer differs: %d vs %d", name, w.Code, allowed.Code)
		}
	}
	if allowed.Code != 200 || !strings.Contains(allowed.Body.String(), "If that address can get a key, we&#39;ve sent it a link") {
		t.Errorf("answer: %d %s", allowed.Code, allowed.Body.String())
	}

	msg := e.mail.msgs[0]
	if msg.To != "pat@acme.example" || !strings.Contains(msg.Subject, "llm.example.com") {
		t.Errorf("email: %+v", msg)
	}
	if strings.Contains(msg.Text, "dy-") {
		t.Error("the confirmation email must not carry a key")
	}
}

func TestJoinRejectsMalformedEmail(t *testing.T) {
	e := newEnrollEnv(t, enrollHandlerFixture)
	for _, in := range []string{"", "pat", "Pat <pat@acme.example>", "pat@acme.example\nBcc: x@other.example", `"a b"@acme.example`} {
		w := e.join(t, in)
		if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "Enter a valid email address") {
			t.Errorf("%q: status %d", in, w.Code)
		}
	}
	if e.mail.count() != 0 {
		t.Error("malformed input sent email")
	}
}

func TestLinkUsesPublicURLNotHost(t *testing.T) {
	e := newEnrollEnv(t, enrollHandlerFixture)
	r := httptest.NewRequest("POST", "/join/"+enrollJoinSecret, strings.NewReader("email=pat%40acme.example"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Host = "attacker.example"
	r.Header.Set("X-Forwarded-Host", "attacker.example")
	e.mux.ServeHTTP(httptest.NewRecorder(), r)
	if e.mail.count() != 1 || strings.Contains(e.mail.msgs[0].Text, "attacker") ||
		!strings.Contains(e.mail.msgs[0].Text, "https://llm.example.com/join/confirm/") {
		t.Fatalf("link: %+v", e.mail.msgs)
	}
}

func TestConfirmCreateThenRotate(t *testing.T) {
	e := newEnrollEnv(t, enrollHandlerFixture)
	e.join(t, "pat@acme.example")
	token := e.mail.lastToken(t)

	// Opening the link (as mail scanners do) changes nothing, however often.
	for i := 0; i < 3; i++ {
		w := e.do(t, "GET", "/join/confirm/"+token, nil, "")
		if w.Code != 200 || !strings.Contains(w.Body.String(), "Create my key") {
			t.Fatalf("confirm page: %d %s", w.Code, w.Body.String())
		}
	}
	if config.FindKeyByEmail(e.cs.Get(), "pat@acme.example") != nil {
		t.Fatal("GET created a key")
	}

	w := e.do(t, "POST", "/join/confirm/"+token, url.Values{}, "")
	if w.Code != 200 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	key1 := shownKey(t, w.Body.String())
	body := w.Body.String()
	for _, want := range []string{config.DefaultDownloadMacOS, config.DefaultDownloadWindows, config.DefaultDownloadQuickstart,
		"Launch Claude Code", "Launch Codex", "paste this key", "Read the quick start"} {
		if !strings.Contains(body, want) {
			t.Errorf("key page missing %q", want)
		}
	}
	k := config.FindKeyByEmail(e.cs.Get(), "pat@acme.example")
	if k == nil || k.Key != key1 || k.Name != "pat@acme.example" || k.JoinKey != "acme" || len(k.Models) != 1 || k.Models[0] != "model-a" {
		t.Fatalf("issued key: %+v", k)
	}
	if !keyWorks(e.cs, key1) {
		t.Fatal("new key does not authenticate")
	}

	// The link is single use.
	again := e.do(t, "POST", "/join/confirm/"+token, url.Values{}, "")
	if again.Code != http.StatusNotFound || !strings.Contains(again.Body.String(), "This link has expired") {
		t.Fatalf("reused link: %d", again.Code)
	}
	if got := e.do(t, "GET", "/join/confirm/"+token, nil, ""); got.Code != http.StatusNotFound {
		t.Fatalf("GET of a used link: %d", got.Code)
	}

	// Lost key: the same flow replaces it.
	e.join(t, "pat@acme.example")
	token2 := e.mail.lastToken(t)
	page := e.do(t, "GET", "/join/confirm/"+token2, nil, "")
	if !strings.Contains(page.Body.String(), "Get a new key — your old key stops working") {
		t.Fatalf("rotate confirm page: %s", page.Body.String())
	}
	w = e.do(t, "POST", "/join/confirm/"+token2, url.Values{}, "")
	key2 := shownKey(t, w.Body.String())
	if key2 == key1 || !strings.Contains(w.Body.String(), "Your previous key no longer works") {
		t.Fatalf("rotate: same=%v", key2 == key1)
	}
	if keyWorks(e.cs, key1) {
		t.Error("old key still authenticates after rotation")
	}
	if !keyWorks(e.cs, key2) {
		t.Error("rotated key does not authenticate")
	}
	n := 0
	for _, k := range e.cs.Get().Keys {
		if k.Email == "pat@acme.example" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d keys for the address", n)
	}
	data, _ := os.ReadFile(e.path)
	if strings.Contains(string(data), key1) {
		t.Error("old key left in the config")
	}
}

func TestConfirmLinkExpires(t *testing.T) {
	e := newEnrollEnv(t, enrollHandlerFixture)
	e.join(t, "pat@acme.example")
	token := e.mail.lastToken(t)
	e.h.tokens.now = func() time.Time { return time.Now().Add(confirmTTL + time.Second) }
	for _, method := range []string{"GET", "POST"} {
		w := e.do(t, method, "/join/confirm/"+token, url.Values{}, "")
		if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "This link has expired") {
			t.Errorf("%s expired link: %d", method, w.Code)
		}
	}
	if config.FindKeyByEmail(e.cs.Get(), "pat@acme.example") != nil {
		t.Error("expired link created a key")
	}
	if got := e.do(t, "GET", "/join/confirm/not-a-token", nil, ""); got.Code != http.StatusNotFound {
		t.Errorf("unknown token: %d", got.Code)
	}
}

func TestConfirmRechecksJoinKey(t *testing.T) {
	e := newEnrollEnv(t, enrollHandlerFixture)
	e.join(t, "pat@acme.example")
	token := e.mail.lastToken(t)
	if err := e.cs.SetJoinKeyPaused("acme", true); err != nil {
		t.Fatal(err)
	}
	w := e.do(t, "POST", "/join/confirm/"+token, url.Values{}, "")
	if w.Code != http.StatusNotFound || config.FindKeyByEmail(e.cs.Get(), "pat@acme.example") != nil {
		t.Fatalf("paused join key still issued a key: %d", w.Code)
	}
}

func TestJoinRateLimits(t *testing.T) {
	e := newEnrollEnv(t, enrollHandlerFixture)
	// Per address: only three emails an hour, with the same answer after.
	for i := 0; i < 5; i++ {
		w := e.do(t, "POST", "/join/"+enrollJoinSecret, url.Values{"email": {"pat@acme.example"}}, fmt.Sprintf("192.0.2.%d", 100+i))
		if w.Code != 200 {
			t.Fatalf("submission %d: %d", i, w.Code)
		}
	}
	if e.mail.count() != enrollMailsPerEmail {
		t.Fatalf("expected %d emails, got %d", enrollMailsPerEmail, e.mail.count())
	}
	// Per IP: submissions beyond the limit get 429.
	var last *httptest.ResponseRecorder
	for i := 0; i <= enrollSubmitsPerIP; i++ {
		last = e.do(t, "POST", "/join/"+enrollJoinSecret, url.Values{"email": {"x@other.example"}}, "198.51.100.7")
	}
	if last.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 after %d submissions, got %d", enrollSubmitsPerIP, last.Code)
	}
	if other := e.do(t, "POST", "/join/"+enrollJoinSecret, url.Values{"email": {"x@other.example"}}, "198.51.100.8"); other.Code != 200 {
		t.Fatalf("another IP should not be limited: %d", other.Code)
	}
	// Confirmations per IP.
	for i := 0; i < enrollConfirmsPerIP; i++ {
		e.do(t, "POST", "/join/confirm/nope", url.Values{}, "203.0.113.5")
	}
	if w := e.do(t, "POST", "/join/confirm/nope", url.Values{}, "203.0.113.5"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("confirm limit: %d", w.Code)
	}
}

func TestPublicJoinPage(t *testing.T) {
	// No public join key: the bare page says where to get a link.
	e := newEnrollEnv(t, enrollHandlerFixture)
	w := e.do(t, "GET", "/join", nil, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Ask your administrator for your team&#39;s join link") ||
		strings.Contains(w.Body.String(), `name="email"`) {
		t.Fatalf("no public join key: %d %s", w.Code, w.Body.String())
	}
	if w := e.do(t, "POST", "/join", url.Values{"email": {"pat@acme.example"}}, ""); e.mail.count() != 0 || w.Code != 200 {
		t.Fatalf("POST without a public join key: %d, %d emails", w.Code, e.mail.count())
	}

	// A public join key: the bare form works for its domains only, with the
	// same answer for every address, and never names the join key.
	e = newEnrollEnv(t, strings.Replace(enrollHandlerFixture, "      models: [model-a]\n", "      models: [model-a]\n      public: true\n", 1))
	w = e.do(t, "GET", "/join", nil, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `name="email"`) || strings.Contains(w.Body.String(), "Acme") ||
		strings.Contains(w.Body.String(), "acme.example") {
		t.Fatalf("public join page: %d %s", w.Code, w.Body.String())
	}
	allowed := e.do(t, "POST", "/join", url.Values{"email": {"pat@acme.example"}}, "")
	other := e.do(t, "POST", "/join", url.Values{"email": {"pat@other.example"}}, "")
	strip := func(w *httptest.ResponseRecorder) string { return nonceRe.ReplaceAllString(w.Body.String(), "") }
	if allowed.Code != other.Code || strip(allowed) != strip(other) {
		t.Fatalf("public join answers differ by address: %d %d\n%s\n---\n%s", allowed.Code, other.Code, strip(allowed), strip(other))
	}
	if e.mail.count() != 1 {
		t.Fatalf("expected one email, got %d", e.mail.count())
	}
	token := e.mail.lastToken(t)
	cw := e.do(t, "POST", "/join/confirm/"+token, url.Values{}, "")
	key := shownKey(t, cw.Body.String())
	k := config.FindKeyByEmail(e.cs.Get(), "pat@acme.example")
	if k == nil || k.Key != key || k.JoinKey != "acme" || k.Models[0] != "model-a" {
		t.Fatalf("public join issued: %+v", k)
	}
}
