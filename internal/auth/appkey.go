package auth

import (
	"context"
	"log/slog"
	"net/http"

	"go-llm-proxy/internal/config"
	"go-llm-proxy/internal/httputil"
)

type appKeyContextKey struct{}

// WithAppKeyContext attaches an app key to the context. Exported primarily
// so handler tests can set up authenticated requests without going through
// the middleware.
func WithAppKeyContext(ctx context.Context, key *config.AppKeyConfig) context.Context {
	return context.WithValue(ctx, appKeyContextKey{}, key)
}

func withAppKeyContext(ctx context.Context, key *config.AppKeyConfig) context.Context {
	return WithAppKeyContext(ctx, key)
}

// AppKeyFromContext returns the AppKeyConfig stored in the context, or nil if not present.
func AppKeyFromContext(ctx context.Context) *config.AppKeyConfig {
	key, _ := ctx.Value(appKeyContextKey{}).(*config.AppKeyConfig)
	return key
}

// AppKeyAuthMiddleware validates app keys for service access (e.g., Qdrant).
// Uses the same token extraction pattern as AuthMiddleware (Bearer or X-Api-Key).
func AppKeyAuthMiddleware(cs *config.ConfigStore, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := cs.Get()

		// If no Qdrant configured or no app keys, reject all requests.
		if cfg.Services.Qdrant == nil || len(cfg.Services.Qdrant.AppKeys) == 0 {
			slog.Warn("qdrant auth failed: no app keys configured", "remote", r.RemoteAddr, "path", r.URL.Path)
			httputil.WriteError(w, http.StatusUnauthorized, "service not configured")
			return
		}

		token := extractToken(r)
		if token == "" {
			slog.Warn("qdrant auth failed: missing token", "remote", r.RemoteAddr, "path", r.URL.Path)
			httputil.WriteError(w, http.StatusUnauthorized, "missing or invalid Authorization header")
			return
		}

		key := findAppKey(cfg, token)
		if key == nil {
			slog.Warn("qdrant auth failed: invalid app key", "remote", r.RemoteAddr, "path", r.URL.Path)
			httputil.WriteError(w, http.StatusUnauthorized, "invalid API key")
			return
		}

		ctx := withAppKeyContext(r.Context(), key)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// findAppKey looks up an app key by token without early-return so total
// time is independent of match position. Mirrors findKey in auth.go.
func findAppKey(cfg *config.Config, token string) *config.AppKeyConfig {
	if cfg.Services.Qdrant == nil {
		return nil
	}
	var matched *config.AppKeyConfig
	for i := range cfg.Services.Qdrant.AppKeys {
		if constantTimeKeyMatch(token, cfg.Services.Qdrant.AppKeys[i].Key) {
			matched = &cfg.Services.Qdrant.AppKeys[i]
		}
	}
	return matched
}
