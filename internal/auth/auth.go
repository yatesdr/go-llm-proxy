package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strings"

	"go-llm-proxy/internal/config"
	"go-llm-proxy/internal/httputil"
)

type contextKey int

const keyContextKey contextKey = iota

func withKeyContext(ctx context.Context, key *config.KeyConfig) context.Context {
	return context.WithValue(ctx, keyContextKey, key)
}

func KeyFromContext(ctx context.Context) *config.KeyConfig {
	key, _ := ctx.Value(keyContextKey).(*config.KeyConfig)
	return key
}

// AuthMiddleware validates API tokens against configured keys.
// Accepts both OpenAI-style (Authorization: Bearer) and Anthropic-style (x-api-key) headers.
func AuthMiddleware(cs *config.ConfigStore, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := cs.Get()

		// If no keys configured, allow all requests (with a config warning at startup).
		if len(cfg.Keys) == 0 {
			next.ServeHTTP(w, r)
			return
		}

		token := extractToken(r)
		if token == "" {
			slog.Warn("auth failed: missing token", "remote", r.RemoteAddr, "path", r.URL.Path)
			httputil.WriteError(w, http.StatusUnauthorized, "missing or invalid Authorization header")
			return
		}

		key := findKey(cfg, token)
		if key == nil {
			slog.Warn("auth failed: invalid key", "remote", r.RemoteAddr, "path", r.URL.Path)
			httputil.WriteError(w, http.StatusUnauthorized, "invalid API key")
			return
		}

		ctx := withKeyContext(r.Context(), key)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// extractToken checks Authorization: Bearer first, then falls back to x-api-key.
func extractToken(r *http.Request) string {
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		return auth[7:]
	}
	return r.Header.Get("X-Api-Key")
}

func findKey(cfg *config.Config, token string) *config.KeyConfig {
	// Hash to fixed length before comparing to prevent length oracle attacks.
	tokenHash := sha256.Sum256([]byte(token))

	for i := range cfg.Keys {
		keyHash := sha256.Sum256([]byte(cfg.Keys[i].Key))
		if subtle.ConstantTimeCompare(tokenHash[:], keyHash[:]) == 1 {
			return &cfg.Keys[i]
		}
	}
	return nil
}

// KeyAllowsModel checks if the key is authorized for the given model.
func KeyAllowsModel(key *config.KeyConfig, model string) bool {
	if key == nil || len(key.Models) == 0 {
		return true
	}
	for _, m := range key.Models {
		if m == model {
			return true
		}
	}
	return false
}
