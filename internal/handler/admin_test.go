package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go-llm-proxy/internal/config"
	"go-llm-proxy/internal/ratelimit"
)

const adminFixture = `listen: ":8080"
models:
  - name: model-a
    backend: http://localhost:8000/v1
  - name: model-b
    backend: http://localhost:8001/v1
keys:
  - key: sk-first
    name: first
    models: [model-a]
  - key: sk-second
    name: second
usage_dashboard: true
usage_dashboard_password: hunter2
log_metrics: true
`

func newAdminTestHandler(t *testing.T) (*AdminHandler, *config.ConfigStore, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(adminFixture), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	cs, err := config.NewConfigStore(path)
	if err != nil {
		t.Fatalf("config store: %v", err)
	}
	rl := ratelimit.NewRateLimiter(nil)
	t.Cleanup(rl.Close)
	return NewAdminHandler(cs, rl, nil), cs, path
}

func authedRequest(t *testing.T, h *AdminHandler, method, target string, body []byte) *http.Request {
	t.Helper()
	token := h.sessions.create()
	if token == "" {
		t.Fatal("session create failed")
	}
	var r *http.Request
	if body != nil {
		r = httptest.NewRequest(method, target, bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	r.AddCookie(&http.Cookie{Name: adminCookieName, Value: token})
	// Same-origin defaults for CSRF check.
	r.Host = "admin.test"
	r.Header.Set("Origin", "http://admin.test")
	return r
}

func TestLoginPageRenders(t *testing.T) {
	h, _, _ := newAdminTestHandler(t)
	w := httptest.NewRecorder()
	h.LoginPage(w, httptest.NewRequest("GET", "/admin/login", nil))
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Sign In") {
		t.Errorf("login page missing Sign In: %q", w.Body.String())
	}
}

func TestLoginWithCorrectPasswordSetsCookie(t *testing.T) {
	h, _, _ := newAdminTestHandler(t)
	form := url.Values{"password": {"hunter2"}}
	r := httptest.NewRequest("POST", "/admin/login", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.HandleLogin(w, r)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", w.Code)
	}
	cookies := w.Result().Cookies()
	found := false
	for _, c := range cookies {
		if c.Name == adminCookieName && c.Value != "" {
			found = true
			if c.HttpOnly != true {
				t.Error("cookie missing HttpOnly")
			}
			if c.SameSite != http.SameSiteStrictMode {
				t.Error("cookie missing SameSite=Strict")
			}
			if c.Path != "/admin" {
				t.Errorf("cookie path = %q, want /admin", c.Path)
			}
		}
	}
	if !found {
		t.Error("admin_auth cookie not set")
	}
}

func TestLoginWithWrongPasswordRejected(t *testing.T) {
	h, _, _ := newAdminTestHandler(t)
	form := url.Values{"password": {"wrong"}}
	r := httptest.NewRequest("POST", "/admin/login", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.HandleLogin(w, r)
	body := w.Body.String()
	if !strings.Contains(body, "Incorrect password") {
		t.Errorf("expected error notice, got %q", body)
	}
}

func TestRequireAPIRejectsUnauthenticated(t *testing.T) {
	h, _, _ := newAdminTestHandler(t)
	called := false
	handler := h.RequireAPI(func(w http.ResponseWriter, r *http.Request) { called = true })
	r := httptest.NewRequest("GET", "/admin/users/data", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
	if called {
		t.Error("inner handler should not run for unauthenticated request")
	}
}

func TestRequireAPIRejectsCrossOriginPost(t *testing.T) {
	h, _, _ := newAdminTestHandler(t)
	handler := h.RequireAPI(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	token := h.sessions.create()
	r := httptest.NewRequest("POST", "/admin/users/mutate", strings.NewReader("{}"))
	r.AddCookie(&http.Cookie{Name: adminCookieName, Value: token})
	r.Host = "admin.test"
	r.Header.Set("Origin", "http://evil.example.com")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", w.Code)
	}
}

func TestRequireAPIRequiresOriginOnPost(t *testing.T) {
	h, _, _ := newAdminTestHandler(t)
	handler := h.RequireAPI(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	token := h.sessions.create()
	r := httptest.NewRequest("POST", "/admin/users/mutate", strings.NewReader("{}"))
	r.AddCookie(&http.Cookie{Name: adminCookieName, Value: token})
	r.Host = "admin.test"
	// No Origin or Referer
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 for missing origin, got %d", w.Code)
	}
}

func TestRequirePageRedirectsUnauthenticated(t *testing.T) {
	h, _, _ := newAdminTestHandler(t)
	handler := h.RequirePage(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/admin/users", nil))
	if w.Code != http.StatusSeeOther {
		t.Errorf("expected 303, got %d", w.Code)
	}
	if !strings.Contains(w.Header().Get("Location"), "/admin/login") {
		t.Errorf("expected redirect to /admin/login, got %q", w.Header().Get("Location"))
	}
}

func TestLogoutRevokesSession(t *testing.T) {
	h, _, _ := newAdminTestHandler(t)
	token := h.sessions.create()
	r := httptest.NewRequest("POST", "/admin/logout", nil)
	r.AddCookie(&http.Cookie{Name: adminCookieName, Value: token})
	w := httptest.NewRecorder()
	h.HandleLogout(w, r)
	if h.sessions.validate(token) {
		t.Error("session should be revoked after logout")
	}
}

func TestUsersDataReturnsMaskedKeys(t *testing.T) {
	h, _, _ := newAdminTestHandler(t)
	r := authedRequest(t, h, "GET", "/admin/users/data", nil)
	w := httptest.NewRecorder()
	h.RequireAPI(h.UsersData).ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Users []struct {
			Name    string   `json:"name"`
			KeyHash string   `json:"key_hash"`
			Masked  string   `json:"masked"`
			Models  []string `json:"models"`
		} `json:"users"`
		AllModels []string `json:"all_models"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Users) != 2 {
		t.Fatalf("expected 2 users, got %d", len(resp.Users))
	}
	for _, u := range resp.Users {
		if strings.Contains(u.Masked, "sk-first") || strings.Contains(u.Masked, "sk-second") {
			t.Errorf("masked form leaks key contents: %q", u.Masked)
		}
		if len(u.KeyHash) != 16 {
			t.Errorf("key_hash should be 16 hex chars, got %q", u.KeyHash)
		}
	}
	if len(resp.AllModels) != 2 {
		t.Errorf("expected 2 all_models, got %d", len(resp.AllModels))
	}
}

func TestUsersMutateUpdateModels(t *testing.T) {
	h, cs, _ := newAdminTestHandler(t)
	// Target the "second" key.
	hash := config.KeyHash("sk-second")
	body, _ := json.Marshal(map[string]any{
		"action":   "update_models",
		"key_hash": hash,
		"models":   []string{"model-b"},
	})
	r := authedRequest(t, h, "POST", "/admin/users/mutate", body)
	w := httptest.NewRecorder()
	h.RequireAPI(h.UsersMutate).ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	got := cs.Get().Keys[1].Models
	if len(got) != 1 || got[0] != "model-b" {
		t.Errorf("expected [model-b], got %v", got)
	}
}

func TestUsersMutateAddReturnsFullKeyOnce(t *testing.T) {
	h, cs, _ := newAdminTestHandler(t)
	body, _ := json.Marshal(map[string]any{"action": "add", "name": "brand-new"})
	r := authedRequest(t, h, "POST", "/admin/users/mutate", body)
	w := httptest.NewRecorder()
	h.RequireAPI(h.UsersMutate).ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		OK     bool   `json:"ok"`
		Key    string `json:"key"`
		Masked string `json:"masked"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.HasPrefix(resp.Key, "dy-") {
		t.Errorf("expected dy- prefix, got %q", resp.Key)
	}
	if len(cs.Get().Keys) != 3 {
		t.Errorf("expected 3 keys after add, got %d", len(cs.Get().Keys))
	}
	// Subsequent /admin/users/data must NOT return the full key.
	r2 := authedRequest(t, h, "GET", "/admin/users/data", nil)
	w2 := httptest.NewRecorder()
	h.RequireAPI(h.UsersData).ServeHTTP(w2, r2)
	if strings.Contains(w2.Body.String(), resp.Key) {
		t.Error("/admin/users/data leaks full key after add")
	}
}

func TestUsersMutateDeleteRefusesLast(t *testing.T) {
	h, _, path := newAdminTestHandler(t)
	// Delete one first so only one remains.
	hash := config.KeyHash("sk-first")
	body, _ := json.Marshal(map[string]any{"action": "delete", "key_hash": hash})
	r := authedRequest(t, h, "POST", "/admin/users/mutate", body)
	w := httptest.NewRecorder()
	h.RequireAPI(h.UsersMutate).ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("first delete should succeed, got %d: %s", w.Code, w.Body.String())
	}
	// Try to delete the remaining key.
	hash2 := config.KeyHash("sk-second")
	body2, _ := json.Marshal(map[string]any{"action": "delete", "key_hash": hash2})
	r2 := authedRequest(t, h, "POST", "/admin/users/mutate", body2)
	w2 := httptest.NewRecorder()
	h.RequireAPI(h.UsersMutate).ServeHTTP(w2, r2)
	if w2.Code != http.StatusConflict {
		t.Fatalf("expected 409 for last key, got %d: %s", w2.Code, w2.Body.String())
	}
	// Confirm file still contains the second key.
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "sk-second") {
		t.Error("second key should remain in config.yaml")
	}
}

func TestModelsMutateAddAndUpdate(t *testing.T) {
	h, cs, _ := newAdminTestHandler(t)
	// Add
	body, _ := json.Marshal(map[string]any{
		"action": "add",
		"model": map[string]any{
			"name":            "model-c",
			"backend":         "http://localhost:9000/v1",
			"context_window":  32000,
			"supports_vision": true,
		},
	})
	r := authedRequest(t, h, "POST", "/admin/models/mutate", body)
	w := httptest.NewRecorder()
	h.RequireAPI(h.ModelsMutate).ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("add: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if m := config.FindModel(cs.Get(), "model-c"); m == nil || !m.SupportsVision {
		t.Fatalf("added model missing or malformed: %+v", m)
	}
	// Update: rename + change context window
	body2, _ := json.Marshal(map[string]any{
		"action":        "update",
		"original_name": "model-c",
		"model": map[string]any{
			"name":           "model-c-prime",
			"backend":        "http://localhost:9000/v1",
			"context_window": 65536,
		},
	})
	r2 := authedRequest(t, h, "POST", "/admin/models/mutate", body2)
	w2 := httptest.NewRecorder()
	h.RequireAPI(h.ModelsMutate).ServeHTTP(w2, r2)
	if w2.Code != 200 {
		t.Fatalf("update: expected 200, got %d: %s", w2.Code, w2.Body.String())
	}
	m := config.FindModel(cs.Get(), "model-c-prime")
	if m == nil || m.ContextWindow != 65536 {
		t.Fatalf("update failed: %+v", m)
	}
}

func TestModelsDataMasksSecrets(t *testing.T) {
	// Write a fixture with a secret, then ensure ModelsData never surfaces it.
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	fixture := `listen: ":8080"
models:
  - name: model-a
    backend: http://localhost:8000/v1
    api_key: super-secret-key-value
keys:
  - key: sk-only
    name: only
usage_dashboard: true
usage_dashboard_password: hunter2
log_metrics: true
`
	if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	cs, err := config.NewConfigStore(path)
	if err != nil {
		t.Fatal(err)
	}
	rl := ratelimit.NewRateLimiter(nil)
	t.Cleanup(rl.Close)
	h := NewAdminHandler(cs, rl, nil)

	r := authedRequest(t, h, "GET", "/admin/models/data", nil)
	w := httptest.NewRecorder()
	h.RequireAPI(h.ModelsData).ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "super-secret-key-value") {
		t.Fatal("full api key leaked in models data payload")
	}
	if !strings.Contains(w.Body.String(), "has_api_key") {
		t.Error("has_api_key field missing from payload")
	}
}

func TestModelsMutateUpdatePreservesOmittedSecret(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	fixture := `listen: ":8080"
models:
  - name: model-a
    backend: http://localhost:8000/v1
    api_key: keep-me-please
keys:
  - key: sk-only
    name: only
usage_dashboard: true
usage_dashboard_password: hunter2
log_metrics: true
`
	if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	cs, err := config.NewConfigStore(path)
	if err != nil {
		t.Fatal(err)
	}
	rl := ratelimit.NewRateLimiter(nil)
	t.Cleanup(rl.Close)
	h := NewAdminHandler(cs, rl, nil)

	// Update without sending api_key field — should preserve existing secret.
	body, _ := json.Marshal(map[string]any{
		"action":        "update",
		"original_name": "model-a",
		"model": map[string]any{
			"name":           "model-a",
			"backend":        "http://localhost:8000/v1",
			"context_window": 16000,
		},
	})
	r := authedRequest(t, h, "POST", "/admin/models/mutate", body)
	w := httptest.NewRecorder()
	h.RequireAPI(h.ModelsMutate).ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	m := config.FindModel(cs.Get(), "model-a")
	if m == nil || m.APIKey != "keep-me-please" {
		t.Errorf("api_key not preserved after update: got %q", func() string {
			if m == nil {
				return "<nil>"
			}
			return m.APIKey
		}())
	}
	// Now send api_key:"" explicitly — should clear.
	empty := ""
	body2, _ := json.Marshal(map[string]any{
		"action":        "update",
		"original_name": "model-a",
		"model": map[string]any{
			"name":    "model-a",
			"backend": "http://localhost:8000/v1",
			"api_key": empty,
		},
	})
	r2 := authedRequest(t, h, "POST", "/admin/models/mutate", body2)
	w2 := httptest.NewRecorder()
	h.RequireAPI(h.ModelsMutate).ServeHTTP(w2, r2)
	if w2.Code != 200 {
		t.Fatalf("explicit empty api_key: expected 200, got %d: %s", w2.Code, w2.Body.String())
	}
	m2 := config.FindModel(cs.Get(), "model-a")
	if m2 == nil || m2.APIKey != "" {
		t.Errorf("api_key should be cleared: got %q", func() string {
			if m2 == nil {
				return "<nil>"
			}
			return m2.APIKey
		}())
	}
}

func TestOriginMatches(t *testing.T) {
	r := httptest.NewRequest("POST", "/admin/x", nil)
	r.Host = "admin.test"

	r.Header.Set("Origin", "http://admin.test")
	if !originMatches(r) {
		t.Error("same origin should match")
	}
	r.Header.Set("Origin", "http://evil.com")
	if originMatches(r) {
		t.Error("cross origin should not match")
	}
	r.Header.Del("Origin")
	r.Header.Set("Referer", "http://admin.test/admin/users")
	if !originMatches(r) {
		t.Error("referer same-host should match")
	}
	r.Header.Del("Referer")
	if originMatches(r) {
		t.Error("no origin/referer should not match")
	}
}
