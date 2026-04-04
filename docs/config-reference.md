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
    messages_mode: auto           # optional
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
| `messages_mode` | no | `"auto"` | Messages API handling: `"auto"`, `"native"`, or `"translate"` (see [Messages mode](#messages-mode)) |
| `context_window` | no | `0` | Max context tokens. `0` = auto-detect from backend at startup |

## Processors (pipeline)

The `processors` block configures the proxy's content processing pipeline. This enables transparent image description, PDF extraction, and web search for backends that don't support these features natively.

```yaml
processors:
  vision: Qwen3-VL-8B           # model name for vision processing (must be in models list)
  ocr: PaddleOCR-VL-1.5         # fast model for PDF/document text extraction (falls back to vision)
  web_search_key: tvly-...      # Tavily API key for web search
```

| Field | Default | Description |
|---|---|---|
| `vision` | — | Model name to use for describing images sent to text-only backends. Must be a vision-capable model defined in `models`. |
| `ocr` | — | Model name for OCR/text extraction from PDF page images. Use a fast, lightweight vision model here. Falls back to `vision` if not set. |
| `web_search_key` | — | Search API key. Supports [Tavily](https://tavily.com/) (`tvly-...`) and [Brave Search](https://brave.com/search/api/) (`BSA...`) — provider is auto-detected from the key prefix. When set, the proxy executes web searches on behalf of clients (Claude Code, Codex) transparently. |

### Per-model processor overrides

Each model can override or disable global processors:

```yaml
models:
  - name: MiniMax-M2.5
    backend: http://192.168.13.32:8000/v1
    # No vision → images routed to qwen-3.5 automatically

  - name: qwen-3.5
    backend: http://192.168.13.30:8000/v1
    supports_vision: true         # model handles images natively
    processors:
      vision: none                # disable vision processing for this model

  - name: glm-5.1
    backend: https://api.z.ai/api/coding/paas/v4
    processors:
      vision: MiniMax-M2.5        # use a specific model for this backend's images
```

### Additional model fields for pipeline

| Field | Default | Description |
|---|---|---|
| `supports_vision` | `false` | Set to `true` if the model handles images natively. Skips vision processing. |
| `force_pipeline` | `false` | Run the pipeline even on native Anthropic backends. Use to force Tavily search instead of Anthropic's server-side search, or to test pipeline processing. |
| `processors` | — | Per-model processor overrides. Set `vision: none` to disable, or `vision: other-model` to use a specific processor. Same for `ocr`. |

### How it works

**Vision processing**: When a client sends an image to a text-only model, the proxy sends the image to the configured vision model with a description prompt, then replaces the image with the text description. The backend model receives only text. Images are processed concurrently (up to 5 in parallel) and cached by content hash so follow-up turns are instant.

**OCR processing**: Images in tool result messages (PDF page renders, Codex `view_image` output, screenshots) are routed to the `ocr` model with a text-extraction prompt instead of the general vision description prompt. This covers both proxy-side PDF pipelines and client-side image extraction. If no `ocr` model is configured, the `vision` model is used as a fallback.

**Web search**: When a client (Claude Code, Codex) includes a web search tool, the proxy converts it to a function tool that the backend can call. If the backend calls `web_search`, the proxy executes a Tavily search, injects the results, and re-sends to the backend. The client sees only the final response with search context incorporated.

**PDF processing**: PDF content is extracted as text. If text extraction fails (scanned PDFs), the OCR/vision processor is used as a fallback.

Native Anthropic backends skip the pipeline by default (images, search, and PDFs pass through to Anthropic's infrastructure). Set `force_pipeline: true` to override this.

### Server-side features

Some coding agents (Claude Code, Codex) expect server-side features from the API. The proxy emulates or passes through the following:

| Feature | Status | Notes |
|---|---|---|
| Token usage tracking | Supported | `input_tokens` and `output_tokens` from backend response forwarded to client. Enables `/context` display and auto-compact triggers. |
| Context compaction | Supported | `/compact` and auto-compact work via normal API calls. Token counts from backend drive the compaction threshold. |
| Web search results | Emulated | `server_tool_use` + `web_search_tool_result` blocks emitted for Claude Code UI |
| Extended thinking | Supported | Reasoning tokens translated to thinking blocks |
| Prompt caching | Passthrough | `cache_control` fields forwarded to backends that support them; stripped otherwise |
| Token counting endpoint | Not supported | Chat Completions backends have no equivalent; Claude Code falls back to local estimates |
| Advisor tool | Not supported | Server-side reviewer model — requires Anthropic API |
| Tool search / deferral | Not supported | `tool_reference` blocks — requires Anthropic API |
| Files API | Not supported | File upload/download — requires Anthropic API |
| Code execution | Not supported | Sandboxed server-side execution — requires Anthropic API |

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

See [codex.md](codex.md) for full details on the Responses API translation layer.

## Messages mode

Controls how the proxy handles Anthropic Messages API requests (`POST /v1/messages`) per model:

| Value | Behavior |
|---|---|
| `auto` | Default. Anthropic backends (`type: anthropic`) passthrough natively; all others translate to Chat Completions automatically |
| `native` | Force passthrough. Backend must speak the Anthropic Messages API |
| `translate` | Force translation to Chat Completions. Skip auto-detection |

In `auto` mode, the proxy determines the behavior from the model's `type` field — no probing needed since no standard OpenAI backend supports `/v1/messages`.

```yaml
# Auto (default): OpenAI backend → translates automatically
- name: MiniMax-M2.5
  backend: http://192.168.100.10:8000/v1

# Auto: Anthropic backend → passthroughs natively
- name: claude-sonnet-4
  backend: https://api.anthropic.com
  type: anthropic
```

The translation supports text, tool calling, reasoning tokens (emitted as thinking blocks), and streaming. See [claude-code.md](claude-code.md) for full details.

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

## Recommended configurations

Tested recipes for each coding agent with full pipeline support.

### Claude Code

```yaml
models:
  # Opus slot — strong reasoning model
  - name: glm-5.1
    backend: https://api.z.ai/api/coding/paas/v4
    api_key: your-zhipu-key

  # Sonnet + Haiku slots — fast, capable all-rounder
  - name: MiniMax-M2.5
    backend: http://192.168.13.32:8000/v1
    responses_mode: translate
    timeout: 600

  # Vision processor — general image description
  - name: Qwen3-VL-8B
    backend: http://192.168.13.30:8000/v1
    supports_vision: true

  # OCR processor — fast document text extraction
  - name: PaddleOCR-VL-1.5
    backend: http://192.168.13.30:8000/v1
    supports_vision: true

processors:
  vision: Qwen3-VL-8B
  ocr: PaddleOCR-VL-1.5
  web_search_key: tvly-your-tavily-key
```

Claude Code model slot mapping: Opus → `glm-5.1`, Sonnet → `MiniMax-M2.5`, Haiku → `MiniMax-M2.5`.

### Codex CLI

```yaml
models:
  - name: MiniMax-M2.5
    backend: http://192.168.13.32:8000/v1
    responses_mode: translate    # required for vLLM backends
    timeout: 600

  - name: Qwen3-VL-8B
    backend: http://192.168.13.30:8000/v1
    supports_vision: true

  - name: PaddleOCR-VL-1.5
    backend: http://192.168.13.30:8000/v1
    supports_vision: true

processors:
  vision: Qwen3-VL-8B
  ocr: PaddleOCR-VL-1.5
  web_search_key: tvly-your-tavily-key
```

Codex uses a single model. Set `responses_mode: translate` for any vLLM backend — vLLM's native `/v1/responses` endpoint bypasses the proxy pipeline, breaking web search, image, and PDF processing.

### Processor model recommendations

| Role | Recommended | Parameters | Notes |
|---|---|---|---|
| **Vision** | [Qwen3-VL-8B](https://huggingface.co/Qwen/Qwen3-VL-8B-Instruct) | 8B | Best quality/speed balance for image description. Strong on charts, screenshots, diagrams. |
| **OCR** | [PaddleOCR-VL-1.5](https://huggingface.co/PaddlePaddle/PaddleOCR-VL-1.5) | 0.9B | Purpose-built for document parsing. 94.5% accuracy, 109 languages, minimal VRAM. |
| **OCR (alt)** | [DeepSeek-OCR 2](https://huggingface.co/deepseek-ai/DeepSeek-OCR) | 3B | Higher accuracy (97%), layout analysis, table extraction. ~2,500 tok/s on A100. |
| **Vision (alt)** | [Qwen3-VL-2B](https://huggingface.co/Qwen/Qwen3-VL-2B) | 2B | Lighter alternative if GPU memory is tight. Good OCR (32 languages). |
