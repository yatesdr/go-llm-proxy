package auth

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"go-llm-proxy/internal/config"
	"go-llm-proxy/internal/httputil"
	"go-llm-proxy/internal/ratelimit"
)

// concurrencyRetryAfter is the Retry-After (seconds) sent with a 429 when a
// key is at its concurrent-request limit. Claude Code, Codex and the OpenAI
// SDKs honor it and retry on their own.
const concurrencyRetryAfter = "5"

// ConcurrencyMiddleware enforces each key's max_concurrent: a request that
// would exceed it gets 429 at once rather than waiting, so a client over its
// limit holds no connection or backend slot. It must run inside
// AuthMiddleware, which puts the key in the request context; requests
// without a key, or whose key has no limit, pass straight through.
//
// The limit is read from the key on every request, so an admin change takes
// effect on the next request; requests already running finish normally.
func ConcurrencyMiddleware(c *ratelimit.Concurrency, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := KeyFromContext(r.Context())
		if key == nil || key.MaxConcurrent <= 0 {
			next.ServeHTTP(w, r)
			return
		}
		release, ok := c.Acquire(concurrencyID(key), key.MaxConcurrent)
		if !ok {
			slog.Warn("request rejected: key at concurrent request limit",
				"key", key.Name, "limit", key.MaxConcurrent, "path", r.URL.Path)
			msg := fmt.Sprintf("too many concurrent requests: this API key is limited to %d at a time; retry when one finishes", key.MaxConcurrent)
			w.Header().Set("Retry-After", concurrencyRetryAfter)
			if strings.HasSuffix(r.URL.Path, "/messages") {
				httputil.WriteAnthropicError(w, http.StatusTooManyRequests, "rate_limit_error", msg)
			} else {
				httputil.WriteError(w, http.StatusTooManyRequests, msg)
			}
			return
		}
		defer release()
		next.ServeHTTP(w, r)
	})
}

// concurrencyID names the slot pool a key draws from. An enrolled key is
// counted by its owner's email, so replacing the key through enrollment
// doesn't hand out a fresh allowance while the old key's streams still run.
func concurrencyID(key *config.KeyConfig) string {
	if key.Email != "" {
		return "email:" + key.Email
	}
	return "key:" + key.Key
}
