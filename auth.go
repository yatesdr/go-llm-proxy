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

// AuthMiddleware validates Bearer tokens against configured keys.
func AuthMiddleware(cs *ConfigStore, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := cs.Get()

		// If no keys configured, allow all requests (with a config warning at startup).
		if len(cfg.Keys) == 0 {
			next.ServeHTTP(w, r)
			return
		}

		token := extractBearer(r)
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

func extractBearer(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return auth[7:]
	}
	return ""
}

func findKey(cfg *Config, token string) *KeyConfig {
	// Hash both values to fixed length before comparing.
	// This prevents length oracle attacks from subtle.ConstantTimeCompare.
	tokenHash := sha256.Sum256([]byte(token))

	var match *KeyConfig
	for i := range cfg.Keys {
		keyHash := sha256.Sum256([]byte(cfg.Keys[i].Key))
		if subtle.ConstantTimeCompare(tokenHash[:], keyHash[:]) == 1 {
			match = &cfg.Keys[i]
		}
	}
	return match
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
