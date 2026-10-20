package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go-llm-proxy/internal/config"
	"go-llm-proxy/internal/ratelimit"
)

const adminEnrollFixture = adminFixture + `enrollment:
  public_url: https://llm.example.com
  smtp:
    host: smtp.example.com
    username: sender@example.com
    password: smtp-secret-value
    from: "LLM <no-reply@example.com>"
    reply_to: support@example.com
  join_keys:
    - id: acme
      name: Acme
      key: 0123456789abcdef0123456789abcdef # gitleaks:allow
      domains: [acme.example]
      models: [model-a]
`

func newAdminEnrollHandler(t *testing.T) (*AdminHandler, *config.ConfigStore, *recordingMailer, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(adminEnrollFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	cs, err := config.NewConfigStore(path)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	rl := ratelimit.NewRateLimiter(nil)
	t.Cleanup(rl.Close)
	h := NewAdminHandler(cs, rl, nil, nil)
	m := &recordingMailer{}
	h.Mailer = m
	return h, cs, m, path
}

func enrollMutate(t *testing.T, h *AdminHandler, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	w := httptest.NewRecorder()
	h.RequireAPI(h.EnrollmentMutate).ServeHTTP(w, authedRequest(t, h, http.MethodPost, "/admin/enrollment/mutate", raw))
	return w
}

func TestEnrollmentPageRenders(t *testing.T) {
	h, _, _, _ := newAdminEnrollHandler(t)
	w := httptest.NewRecorder()
	h.RequirePage(h.EnrollmentPage).ServeHTTP(w, authedRequest(t, h, http.MethodGet, "/admin/enrollment", nil))
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	for _, want := range []string{`href="/admin/enrollment"`, `id="joinBody"`, `id="smtpPassFile"`, `id="jkPublic"`, "Send test email"} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("page missing %q", want)
		}
	}
	// Unauthenticated: redirected / refused.
	w = httptest.NewRecorder()
	h.RequirePage(h.EnrollmentPage).ServeHTTP(w, httptest.NewRequest("GET", "/admin/enrollment", nil))
	if w.Code != http.StatusSeeOther {
		t.Errorf("unauthenticated page: %d", w.Code)
	}
	w = httptest.NewRecorder()
	h.RequireAPI(h.EnrollmentData).ServeHTTP(w, httptest.NewRequest("GET", "/admin/enrollment/data", nil))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated data: %d", w.Code)
	}
}

func TestEnrollmentDataMasksPassword(t *testing.T) {
	h, _, _, _ := newAdminEnrollHandler(t)
	w := httptest.NewRecorder()
	h.RequireAPI(h.EnrollmentData).ServeHTTP(w, authedRequest(t, h, http.MethodGet, "/admin/enrollment/data", nil))
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "smtp-secret-value") {
		t.Fatal("SMTP password sent to the browser")
	}
	var d struct {
		Settings struct {
			SMTP struct {
				PasswordSet    bool   `json:"password_set"`
				PasswordSource string `json:"password_source"`
				Port           int    `json:"port"`
				StartTLS       bool   `json:"starttls"`
			} `json:"smtp"`
		} `json:"settings"`
		Missing  []string `json:"missing"`
		JoinKeys []struct {
			ID, Key string
		} `json:"join_keys"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	s := d.Settings.SMTP
	if !s.PasswordSet || s.PasswordSource != "inline" || s.Port != 587 || !s.StartTLS || len(d.Missing) != 0 {
		t.Errorf("data: %+v missing %v", s, d.Missing)
	}
	if len(d.JoinKeys) != 1 || d.JoinKeys[0].Key != "0123456789abcdef0123456789abcdef" {
		t.Errorf("join keys: %+v", d.JoinKeys)
	}
}

func TestEnrollmentSaveSettings(t *testing.T) {
	h, cs, _, path := newAdminEnrollHandler(t)
	settings := map[string]any{
		"public_url": "https://llm.example.com",
		"paused":     true,
		"smtp": map[string]any{"host": "smtp.example.com", "port": 587, "username": "sender@example.com",
			"password": "", "password_file": "", "from": "no-reply@example.com", "reply_to": "support@example.com", "starttls": true},
		"downloads": map[string]any{"macos": "", "windows": "", "quickstart": "https://docs.example.com/start"},
	}
	w := enrollMutate(t, h, map[string]any{"action": "save_settings", "settings": settings})
	if w.Code != 200 {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	e := cs.Get().Enrollment
	if !e.Paused || e.SMTP.Password != "smtp-secret-value" || e.Downloads.Quickstart != "https://docs.example.com/start" {
		t.Errorf("saved: %+v", e)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "usage_dashboard_password: hunter2") {
		t.Error("unrelated config lost")
	}

	settings["public_url"] = "not a url"
	if w := enrollMutate(t, h, map[string]any{"action": "save_settings", "settings": settings}); w.Code != http.StatusBadRequest {
		t.Errorf("invalid public url: %d %s", w.Code, w.Body.String())
	}
}

func TestEnrollmentJoinKeyActions(t *testing.T) {
	h, cs, _, _ := newAdminEnrollHandler(t)
	w := enrollMutate(t, h, map[string]any{"action": "add_join_key", "name": "Widgets Inc", "domains": []string{"Widgets.Example"},
		"models": []string{"model-b"}, "public": true})
	if w.Code != 200 {
		t.Fatalf("add: %d %s", w.Code, w.Body.String())
	}
	var res struct{ ID, Key string }
	json.Unmarshal(w.Body.Bytes(), &res)
	jk := cs.Get().Enrollment.FindJoinKey(res.ID)
	if res.ID != "widgets-inc" || jk == nil || jk.Key != res.Key || jk.Domains[0] != "widgets.example" || !jk.Public {
		t.Fatalf("added: %+v %+v", res, jk)
	}

	for name, body := range map[string]map[string]any{
		"no domains":    {"action": "add_join_key", "name": "X", "domains": []string{}},
		"bad domain":    {"action": "add_join_key", "name": "X", "domains": []string{"not a domain"}},
		"unknown model": {"action": "add_join_key", "name": "X", "domains": []string{"x.example"}, "models": []string{"nope"}},
		"no name":       {"action": "add_join_key", "name": "", "domains": []string{"x.example"}},
		"unknown id":    {"action": "pause_join_key", "id": "nope", "paused": true},
	} {
		if w := enrollMutate(t, h, body); w.Code < 400 || w.Code >= 500 {
			t.Errorf("%s: expected 4xx, got %d %s", name, w.Code, w.Body.String())
		}
	}

	if w := enrollMutate(t, h, map[string]any{"action": "update_join_key", "id": res.ID, "name": "Widgets",
		"domains": []string{"widgets.example", "widgets.example.org"}, "models": []string{}, "public": false}); w.Code != 200 {
		t.Fatalf("update: %d %s", w.Code, w.Body.String())
	}
	jk = cs.Get().Enrollment.FindJoinKey(res.ID)
	if jk.Name != "Widgets" || len(jk.Domains) != 2 || len(jk.Models) != 0 || jk.Public {
		t.Fatalf("updated: %+v", jk)
	}
	if w := enrollMutate(t, h, map[string]any{"action": "pause_join_key", "id": res.ID, "paused": true}); w.Code != 200 ||
		!cs.Get().Enrollment.FindJoinKey(res.ID).Paused {
		t.Fatalf("pause: %d", w.Code)
	}
	if w := enrollMutate(t, h, map[string]any{"action": "delete_join_key", "id": res.ID}); w.Code != 200 ||
		cs.Get().Enrollment.FindJoinKey(res.ID) != nil {
		t.Fatalf("delete: %d", w.Code)
	}
}

func TestEnrollmentTestEmail(t *testing.T) {
	h, _, m, _ := newAdminEnrollHandler(t)
	w := enrollMutate(t, h, map[string]any{"action": "test_email", "to": "admin@example.com"})
	if w.Code != 200 || m.count() != 1 {
		t.Fatalf("test email: %d, %d sent", w.Code, m.count())
	}
	if m.msgs[0].To != "admin@example.com" || m.last.Host != "smtp.example.com" || m.last.Password != "smtp-secret-value" ||
		!m.last.STARTTLS || m.last.ReplyTo != "support@example.com" {
		t.Errorf("sent with %+v", m.last)
	}
	if w := enrollMutate(t, h, map[string]any{"action": "test_email", "to": "nope"}); w.Code != http.StatusBadRequest {
		t.Errorf("bad address: %d", w.Code)
	}
}

func TestEnrollmentMutateRejectsCrossOrigin(t *testing.T) {
	h, _, _, _ := newAdminEnrollHandler(t)
	r := authedRequest(t, h, http.MethodPost, "/admin/enrollment/mutate", []byte(`{"action":"delete_join_key","id":"acme"}`))
	r.Header.Set("Origin", "https://evil.example")
	w := httptest.NewRecorder()
	h.RequireAPI(h.EnrollmentMutate).ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-origin mutate: %d", w.Code)
	}
}

func TestUsersDataShowsEnrolledKeys(t *testing.T) {
	h, cs, _, _ := newAdminEnrollHandler(t)
	key, _, err := cs.EnrollKey("pat@acme.example", "acme", []string{"model-a"})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	h.RequireAPI(h.UsersData).ServeHTTP(w, authedRequest(t, h, http.MethodGet, "/admin/users/data", nil))
	var d struct {
		Users []map[string]any `json:"users"`
	}
	json.Unmarshal(w.Body.Bytes(), &d)
	var found map[string]any
	for _, u := range d.Users {
		if u["email"] == "pat@acme.example" {
			found = u
		}
	}
	if found == nil || found["join_key"] != "acme" || found["join_key_name"] != "Acme" || found["name"] != "pat@acme.example" {
		t.Fatalf("enrolled user: %+v", d.Users)
	}
	if strings.Contains(w.Body.String(), key) {
		t.Error("full key in users data")
	}

	// Admins can still rotate and revoke an enrolled key.
	hash := config.KeyHash(key)
	raw, _ := json.Marshal(map[string]any{"action": "rotate", "key_hash": hash})
	w = httptest.NewRecorder()
	h.RequireAPI(h.UsersMutate).ServeHTTP(w, authedRequest(t, h, http.MethodPost, "/admin/users/mutate", raw))
	if w.Code != 200 {
		t.Fatalf("rotate: %d %s", w.Code, w.Body.String())
	}
	k := config.FindKeyByEmail(cs.Get(), "pat@acme.example")
	if k == nil || k.Key == key || k.JoinKey != "acme" {
		t.Fatalf("after rotate: %+v", k)
	}
	raw, _ = json.Marshal(map[string]any{"action": "delete", "key_hash": config.KeyHash(k.Key)})
	w = httptest.NewRecorder()
	h.RequireAPI(h.UsersMutate).ServeHTTP(w, authedRequest(t, h, http.MethodPost, "/admin/users/mutate", raw))
	if w.Code != 200 || config.FindKeyByEmail(cs.Get(), "pat@acme.example") != nil {
		t.Fatalf("delete: %d", w.Code)
	}
}
