# Security

go-llm-proxy is designed to be safe for public internet exposure when deployed behind a reverse proxy (nginx, Caddy, etc.). This document covers the security model, recommendations, and known considerations.

## Built-in protections

| Protection | Detail |
|---|---|
| Path allowlist | Only proxied endpoints are reachable; arbitrary backend paths are blocked |
| Body size limits | Requests capped at 50 MB, responses at 100 MB |
| Constant-time key comparison | API keys compared via SHA-256 + `subtle.ConstantTimeCompare` |
| No redirect following | HTTP client rejects redirects, preventing SSRF via backend responses |
| Backend URL validation | Config rejects non-HTTP schemes, missing hosts, embedded credentials |
| Response header filtering | Only allowlisted headers forwarded from backends |
| Panic recovery | Internal errors return generic 500; stack traces logged server-side only |
| Security headers | `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`, `Cache-Control: no-store` on all responses |

## Authentication

API keys are configured in `config.yaml` under the `keys` section. Both `Authorization: Bearer` and `x-api-key` headers are accepted.

Each key can be restricted to specific models. Remove the `keys` section entirely to disable authentication (not recommended for public deployments).

Key hashes (first 16 hex chars of SHA-256) are used in usage logs — the actual keys are never stored.

## Rate limiting

Rate limiting targets **failed authentication attempts only**. Valid API keys are never throttled.

| Failed attempts | Action |
|---|---|
| 1-2 | Normal response |
| 3-4 | Throttled (increasing delay) |
| 5+ | Rejected with `429 Too Many Requests` |

Strikes decay at 1 per minute of inactivity. An IP rejected at 5 failures recovers after ~5 minutes. Up to 100K IPs are tracked; stale records are cleaned every 5 minutes.

For production deployments, rate limiting at the nginx/CDN layer provides stronger protection against distributed attacks.

## Config generator page

The config generator (`GET /`) is **disabled by default**. When enabled, it exposes:

- Model names and protocol types
- Whether models are self-hosted or third-party (based on IP range detection)
- Context window sizes

It does **not** expose backend URLs, backend API keys, or proxy API keys. User-entered API keys stay client-side and are never sent to the server.

For public deployments, consider putting the config generator behind nginx basic auth or disabling it after initial setup.

## Usage dashboard

The dashboard (`/usage`) requires a password configured in `usage_dashboard_password`. Authentication uses:

- HMAC-based cookie (HttpOnly, SameSite=Strict, Secure on HTTPS)
- Rate-limited login (same per-IP throttling as API auth)
- 30-day cookie expiry

## Deployment recommendations

### Production checklist

1. Deploy behind nginx with TLS (see [deployment.md](deployment.md))
2. Configure `trusted_proxies` to your nginx IP(s) only
3. Use strong, unique API keys (32+ characters)
4. Set `listen: "127.0.0.1:8080"` to bind to localhost only
5. Disable the config generator after setup, or put behind nginx auth
6. Enable usage logging (`log_metrics: true`) for monitoring
7. Set appropriate `timeout` values per model

### nginx

Nginx provides TLS termination, connection limits, and an additional security layer. See [deployment.md](deployment.md) for a complete nginx config.

Key nginx settings for security:

```nginx
client_max_body_size 50m;        # match proxy's request limit
limit_conn llm_conn 20;         # per-IP connection limit
proxy_buffering off;             # required for SSE streaming
```

### Trusted proxies

Configure `trusted_proxies` so the proxy uses the real client IP (from `X-Real-IP`) instead of nginx's IP for rate limiting:

```yaml
trusted_proxies:
  - "127.0.0.1"
  - "10.0.0.0/8"     # if nginx is on another host in your network
```

Without this, all requests appear to come from the nginx IP, and a single attacker could trigger rate limiting for everyone.

## Known considerations

- **No TLS termination**: The proxy serves plain HTTP. Use nginx/Caddy for TLS.
- **Backend trust**: Backend URLs come from the admin config. The proxy trusts these hosts for all operations including context window detection.
- **Config generator exposure**: When enabled, model names are public. This is by design for usability but may be undesirable in some environments.
- **Log data**: Usage logs contain model names, key hashes, token counts, and timing data. Protect the SQLite database file appropriately.
