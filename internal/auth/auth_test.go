package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go-llm-proxy/internal/config"
)

func TestFindKey_Match(t *testing.T) {
	cfg := &config.Config{
		Keys: []config.KeyConfig{
			{Key: "sk-alpha", Name: "alpha"},
			{Key: "sk-beta", Name: "beta"},
		},
	}

	key := findKey(cfg, "sk-beta")
	if key == nil || key.Name != "beta" {
		t.Fatalf("expected beta key, got: %v", key)
	}
}

func TestFindKey_NoMatch(t *testing.T) {
	cfg := &config.Config{
		Keys: []config.KeyConfig{
			{Key: "sk-alpha", Name: "alpha"},
		},
	}

	key := findKey(cfg, "sk-wrong")
	if key != nil {
		t.Fatalf("expected nil, got: %v", key)
	}
}

func TestFindKey_MatchesLastKey(t *testing.T) {
	cfg := &config.Config{
		Keys: []config.KeyConfig{
			{Key: "sk-first", Name: "first"},
			{Key: "sk-second", Name: "second"},
			{Key: "sk-third", Name: "third"},
		},
	}

	key := findKey(cfg, "sk-third")
	if key == nil || key.Name != "third" {
		t.Fatalf("expected third key, got: %v", key)
	}
}

func TestFindKey_EmptyToken(t *testing.T) {
	cfg := &config.Config{
		Keys: []config.KeyConfig{
			{Key: "sk-alpha", Name: "alpha"},
		},
	}

	key := findKey(cfg, "")
	if key != nil {
		t.Fatalf("expected nil for empty token, got: %v", key)
	}
}

func TestKeyAllowsModel_NilKey(t *testing.T) {
	if !KeyAllowsModel(nil, "any-model") {
		t.Fatal("nil key should allow all models")
	}
}

func TestKeyAllowsModel_EmptyModels(t *testing.T) {
	key := &config.KeyConfig{Key: "sk-test", Name: "test", Models: []string{}}
	if !KeyAllowsModel(key, "any-model") {
		t.Fatal("empty models list should allow all models")
	}
}

func TestKeyAllowsModel_Allowed(t *testing.T) {
	key := &config.KeyConfig{Key: "sk-test", Name: "test", Models: []string{"model-a", "model-b"}}
	if !KeyAllowsModel(key, "model-b") {
		t.Fatal("expected model-b to be allowed")
	}
}

func TestKeyAllowsModel_Denied(t *testing.T) {
	key := &config.KeyConfig{Key: "sk-test", Name: "test", Models: []string{"model-a"}}
	if KeyAllowsModel(key, "model-b") {
		t.Fatal("expected model-b to be denied")
	}
}

func TestExtractToken_Bearer(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer sk-test-123")
	if got := extractToken(r); got != "sk-test-123" {
		t.Fatalf("expected sk-test-123, got: %q", got)
	}
}

func TestExtractToken_XApiKey(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Api-Key", "sk-ant-test-456")
	if got := extractToken(r); got != "sk-ant-test-456" {
		t.Fatalf("expected sk-ant-test-456, got: %q", got)
	}
}

func TestExtractToken_BearerTakesPrecedence(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer sk-bearer")
	r.Header.Set("X-Api-Key", "sk-api-key")
	if got := extractToken(r); got != "sk-bearer" {
		t.Fatalf("expected Bearer to take precedence, got: %q", got)
	}
}

func TestExtractToken_Missing(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	if got := extractToken(r); got != "" {
		t.Fatalf("expected empty, got: %q", got)
	}
}

func TestExtractToken_NonBearer(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	if got := extractToken(r); got != "" {
		t.Fatalf("expected empty for Basic auth with no x-api-key, got: %q", got)
	}
}

func TestKeyContext_RoundTrip(t *testing.T) {
	key := &config.KeyConfig{Key: "sk-test", Name: "test"}
	ctx := withKeyContext(context.Background(), key)

	got := KeyFromContext(ctx)
	if got != key {
		t.Fatalf("expected same key back, got: %v", got)
	}
}

func TestKeyContext_Missing(t *testing.T) {
	got := KeyFromContext(context.Background())
	if got != nil {
		t.Fatalf("expected nil from empty context, got: %v", got)
	}
}

func TestAuthMiddleware_NoKeys(t *testing.T) {
	cfg := &config.Config{}
	cs := config.NewTestConfigStore(cfg)

	called := false
	handler := AuthMiddleware(cs, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))

	r := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if !called {
		t.Fatal("expected handler to be called when no keys configured")
	}
}

func TestAuthMiddleware_ValidKey(t *testing.T) {
	cfg := &config.Config{
		Keys: []config.KeyConfig{{Key: "sk-valid", Name: "admin"}},
	}
	cs := config.NewTestConfigStore(cfg)

	var gotKey *config.KeyConfig
	handler := AuthMiddleware(cs, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = KeyFromContext(r.Context())
	}))

	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer sk-valid")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if gotKey == nil || gotKey.Name != "admin" {
		t.Fatalf("expected admin key in context, got: %v", gotKey)
	}
}

func TestAuthMiddleware_InvalidKey(t *testing.T) {
	cfg := &config.Config{
		Keys: []config.KeyConfig{{Key: "sk-valid", Name: "admin"}},
	}
	cs := config.NewTestConfigStore(cfg)

	handler := AuthMiddleware(cs, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called")
	}))

	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer sk-wrong")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got: %d", w.Code)
	}
}

func TestAuthMiddleware_ValidXApiKey(t *testing.T) {
	cfg := &config.Config{
		Keys: []config.KeyConfig{{Key: "sk-valid", Name: "admin"}},
	}
	cs := config.NewTestConfigStore(cfg)

	var gotKey *config.KeyConfig
	handler := AuthMiddleware(cs, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = KeyFromContext(r.Context())
	}))

	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Api-Key", "sk-valid")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if gotKey == nil || gotKey.Name != "admin" {
		t.Fatalf("expected admin key via x-api-key, got: %v", gotKey)
	}
}

func TestAuthMiddleware_MissingHeader(t *testing.T) {
	cfg := &config.Config{
		Keys: []config.KeyConfig{{Key: "sk-valid", Name: "admin"}},
	}
	cs := config.NewTestConfigStore(cfg)

	handler := AuthMiddleware(cs, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called")
	}))

	r := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got: %d", w.Code)
	}
}
