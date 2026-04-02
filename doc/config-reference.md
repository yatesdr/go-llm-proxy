# Configuration Reference

go-llm-proxy is configured via a single YAML file (default: `config.yaml`). See `config.yaml.example` for a complete annotated template.

## Top-level settings

| Field | Default | Description |
|---|---|---|
| `listen` | `":8080"` | Bind address (e.g., `"127.0.0.1:8080"` for localhost only) |
| `trusted_proxies` | `[]` | IPs/CIDRs allowed to set `X-Real-IP` / `X-Forwarded-For` |
| `serve_config_generator` | `false` | Enable the config generator UI at `GET /` |
| `log_metrics` | `false` | Enable per-request usage logging to SQLite |
| `usage_db` | `"usage.db"` | Path to the SQLite usage database |
| `usage_dashboard` | `false` | Enable the usage dashboard at `/usage` |
| `usage_dashboard_password` | — | Required when dashboard is enabled |

## Model fields

```yaml
models:
  - name: MiniMax-M2.5
    backend: http://192.168.100.10:8000/v1
    api_key: your-backend-key
    model: internal-model-name    # optional
    timeout: 300                  # optional
    type: openai                  # optional
    responses_mode: auto          # optional
    context_window: 0             # optional
```

| Field | Required | Default | Description |
|---|---|---|---|
| `name` | yes | — | Model name clients use in requests |
| `backend` | yes | — | Upstream base URL (see [Backend URL routing](#backend-url-routing)) |
| `api_key` | no | — | Token sent upstream (`Bearer` for OpenAI, `x-api-key` for Anthropic) |
| `model` | no | same as `name` | Model name sent to the backend (for rewriting) |
| `timeout` | no | `300` | Request timeout in seconds |
| `type` | no | `"openai"` | Backend protocol: `"openai"` or `"anthropic"` |
| `responses_mode` | no | `"auto"` | Responses API handling: `"auto"`, `"native"`, or `"translate"` (see [Responses mode](#responses-mode)) |
| `context_window` | no | `0` | Max context tokens. `0` = auto-detect from backend at startup |

## Key fields

```yaml
keys:
  - key: sk-your-api-key
    name: admin
    models: []

  - key: sk-restricted
    name: guest
    models: [MiniMax-M2.5, nomic-embed]
```

| Field | Required | Description |
|---|---|---|
| `key` | yes | The API key value clients send via `Authorization: Bearer` or `x-api-key` |
| `name` | yes | Friendly name for logging |
| `models` | no | Allowed model names. Empty or omitted = all models |

Remove the `keys` section entirely to disable authentication (not recommended for public exposure).

## Backend URL routing

The proxy appends the request path to the backend URL. How the path is constructed depends on the backend type:

**OpenAI backends** (`type: openai` or default): The proxy strips `/v1` from the client path. A client request to `POST /v1/chat/completions` with backend `http://host:8000/v1` is sent to `http://host:8000/v1/chat/completions`.

**Anthropic backends** (`type: anthropic`): The proxy keeps `/v1` in the path. A client request to `POST /v1/messages` with backend `https://api.anthropic.com` is sent to `https://api.anthropic.com/v1/messages`.

### Examples

```yaml
models:
  # Standard OpenAI-compatible (vLLM, llama-server)
  - name: MiniMax-M2.5
    backend: http://192.168.100.10:8000/v1

  # Non-standard path (Zhipu GLM)
  - name: glm-5.1
    backend: https://api.z.ai/api/coding/paas/v4
    api_key: your-key

  # Anthropic — base URL omits /v1
  - name: claude-sonnet-4-20250514
    backend: https://api.anthropic.com
    api_key: sk-ant-your-key
    type: anthropic

  # Third-party Anthropic-compatible
  - name: MiniMax-M2.7
    backend: https://api.minimax.io/anthropic
    api_key: your-key
    type: anthropic
```

## Responses mode

Controls how the proxy handles Responses API requests (`POST /v1/responses`, `POST /v1/responses/compact`) per model:

| Value | Behavior |
|---|---|
| `auto` | Default. Probe backend on first request; cache result; fall back to translation on 404 |
| `native` | Always passthrough. Never translate. Backend must support the Responses API |
| `translate` | Always translate to Chat Completions. Skip the probe entirely |

```yaml
- name: MiniMax-M2.5
  backend: http://192.168.100.10:8000/v1
  responses_mode: translate   # vLLM: use translation for reliable Codex support
```

See [doc/codex.md](codex.md) for full details on the Responses API translation layer.

## Context window detection

At startup, the proxy queries each backend's `/v1/models` endpoint to discover context window sizes:

- **vLLM**: `max_model_len` field
- **llama-server**: `meta.n_ctx_train` field
- **Anthropic**: `max_input_tokens` from `GET /v1/models/{model_id}`

Detection runs asynchronously (non-blocking). Results are served through the proxy's `/v1/models` endpoint and used in the config generator.

Set `context_window` explicitly for backends that don't report it:

```yaml
- name: MiniMax-M2.7
  backend: https://api.minimax.io/anthropic
  type: anthropic
  context_window: 1048576   # 1M tokens
```

## Config reload

The proxy automatically reloads the config file when it changes on disk. You can also send `SIGHUP`:

```bash
kill -HUP $(pidof go-llm-proxy)
```

If validation fails, the old config stays active and an error is logged.

## CLI flags

| Flag | Description |
|---|---|
| `-config PATH` | Config file path (default: `config.yaml`) |
| `-serve-config-generator` | Enable config generator UI at `GET /` |
| `-serve-dashboard` | Enable usage dashboard at `/usage` |
| `-log-metrics` | Enable usage logging to SQLite |
| `-usage-db PATH` | Override usage database path |
| `-usage-report` | Print usage summary and exit |
| `-model-report` | Print per-model summary and exit |
| `-report-days N` | Days to include in reports (default: 30) |
| `-adduser` | Interactively create an API key |
