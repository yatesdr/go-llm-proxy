package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
)

type contextKey int

const keyContextKey contextKey = iota

func withKeyContext(ctx context.Context, key *KeyConfig) context.Context {
	return context.WithValue(ctx, keyContextKey, key)
}

func keyFromContext(ctx context.Context) *KeyConfig {
	key, _ := ctx.Value(keyContextKey).(*KeyConfig)
	return key
}

// AuthMiddleware validates API tokens against configured keys.
// Accepts both OpenAI-style (Authorization: Bearer) and Anthropic-style (x-api-key) headers.
func AuthMiddleware(cs *ConfigStore, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := cs.Get()

		// If no keys configured, allow all requests (with a config warning at startup).
		if len(cfg.Keys) == 0 {
			next.ServeHTTP(w, r)
			return
		}

		token := extractToken(r)
		if token == "" {
			writeError(w, http.StatusUnauthorized, "missing or invalid Authorization header")
			return
		}

		key := findKey(cfg, token)
		if key == nil {
			writeError(w, http.StatusUnauthorized, "invalid API key")
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

func findKey(cfg *Config, token string) *KeyConfig {
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

// keyAllowsModel checks if the key is authorized for the given model.
func keyAllowsModel(key *KeyConfig, model string) bool {
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
