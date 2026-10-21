# Configuration Reference

go-llm-proxy is configured via a single YAML file (default: `config.yaml`). See `config.yaml.example` for a complete annotated template.

## Top-level settings

| Field | Default | Description |
|---|---|---|
| `listen` | `":8080"` | Bind address (e.g., `"127.0.0.1:8080"` for localhost only) |
| `aliases` | `{}` | Exact, case-sensitive client model IDs mapped to configured chat model names |
| `trusted_proxies` | `[]` | IPs/CIDRs allowed to set `X-Real-IP` / `X-Forwarded-For` |
| `serve_config_generator` | `false` | Enable the config generator UI at `GET /` |
| `log_metrics` | `false` | Enable per-request usage logging to SQLite |
| `usage_db` | `"usage.db"` | Path to the SQLite usage database |
| `usage_dashboard` | `false` | Enable the usage dashboard at `/usage` |
| `usage_dashboard_password` | — | Required when dashboard is enabled. Falls back to `GO_LLM_USAGE_DASHBOARD_PASSWORD` if unset — see below |
| `connect` | — | Optional [Eidonix connect](connect.md) hub: `url` (default `http://connect:8080`) and `application` (default `go-llm-proxy`). Backends written `connect://<site>/<service>/<path>` are reached through it |
| `health_interval` | `30` | Seconds between backend health probes. Read at startup (restart to change). Disabled backends are never probed; connect backends are judged from the hub catalog every 15 s instead |
| `auto_mode` | — | Optional [Claude Code auto mode](claude-code.md#auto-mode) decision head: `model` (a configured model or alias with an OpenAI-compatible backend that supports `logprobs` and `continue_final_message`) and `block_probability` (default `0.5`, between 0 and 1). Absent means no interception |
| `enrollment` | — | Optional [self-service API keys](#self-service-api-keys-enrollment): people get or replace their own key by confirming a work email |
| `admin_password` | — | Enables the admin UI at `/admin` on its own; when unset, `/admin` uses the dashboard password (and requires the dashboard to be enabled). Falls back to `GO_LLM_ADMIN_PASSWORD` if unset — see below |

### Environment variable overrides

`admin_password` and `usage_dashboard_password` can be set via environment
variables instead of the config file — mainly useful for Docker, so a fresh
deployment can bootstrap the admin console without any YAML editing. The
YAML value always wins when set; the environment variable only fills in a
field left empty in the config file.

| Env var | Overrides |
|---|---|
| `GO_LLM_ADMIN_PASSWORD` | `admin_password` |
| `GO_LLM_USAGE_DASHBOARD_PASSWORD` | `usage_dashboard_password` |

See [docs/docker.md](docker.md) for the full first-time setup walkthrough.

## Model backend lists

Every logical model owns the servers that serve it. Add another entry to the
model's `backends` list to add capacity; there is no separate pool object to
create or link.

```yaml
models:
  - name: glm-4.6
    backends:
      - url: http://192.168.1.10:8000/v1
        weight: 2          # relative capacity share (default 1)
        max_inflight: 8    # spill beyond this limit (0 = unlimited)
      - url: http://192.168.1.11:8000/v1
        api_key: key       # optional credential for this server
        disabled: false    # true = configured but excluded from routing
```

A model must contain at least one `backends` entry. A one-server model still
uses a one-item list. Older `backend`, model-level `api_key`, `pool`, and
top-level `pools` YAML is expanded automatically on first startup; the original
file is retained beside it with a `.pre-backend-lists` suffix.

**Routing behavior.** Selection is sticky: the proxy hashes each request's
stable conversation prefix (system prompt + first message) and maps it onto a
backend with weighted rendezvous hashing, so every turn of a session
lands on the backend whose KV/prefix cache is already warm, and retries and
follow-up turns hash identically with no client cooperation. New sessions
spread across members proportionally to `weight`; requests with no
conversation shape go to the least-loaded member. A member at `max_inflight`
spills new sessions to the next-ranked member. Failed members are skipped: a
circuit breaker opens after 3 consecutive backend failures (30s, then a
half-open retry), the 30s health probes exclude probed-down members, and a
transport error or 5xx on `/v1/chat/completions` fails over to another
backend once before reporting an error. Adding a backend to a model only
remaps ~1/N of active sessions.

## Model aliases

Aliases let clients use an alternate model ID while routing to an existing
configured chat model. They are case-sensitive. An alias containing `*` or `?`
is a glob pattern, tried after exact model and alias names; a trailing
bracket suffix such as `[1m]` is ignored when matching. With several matching
patterns the most specific wins.

```yaml
aliases:
  codex-reviewer-model: glm-5.3
  fast-review: qwen3.5-9b
  claude-haiku-*: qwen3.5-9b   # glob: every Haiku ID Claude Code sends
```

Every target must match a `models[].name`. Alias IDs cannot duplicate a real
model name or point to another alias. API-key model restrictions are checked
against the target, so a key allowed to use `glm-5.3` can also use its aliases.
Aliases appear in `GET /v1/models` with the target model's context window.

The admin LLM page also captures up to 200 unrecognized model IDs observed
since the current process started. An administrator can map one to an existing
model from the **Model Aliasing** tab. The capture list is in-memory only;
configured aliases are saved to YAML and hot-reloaded.

## Model fields

`thinking_off` (optional, any model) is a raw object merged into the Chat
Completions request when a Messages client sends `thinking.type: "disabled"`
(Claude Code's auto-mode classifier does). Examples:
`thinking_off: {chat_template_kwargs: {enable_thinking: false}}` for templates
that honour `enable_thinking`; `thinking_off: {chat_template_kwargs:
{reasoning_effort: low}}` where switching thinking off breaks the reasoning
parser (GLM-5.3 on vLLM). Test against your server.
See [claude-code.md](claude-code.md#auto-mode-and-side-queries).

```yaml
models:
  - name: MiniMax-M2.5
    backends:
      - url: http://192.168.100.10:8000/v1
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
| `backends` | yes | — | One or more upstream replicas (see [Model backend lists](#model-backend-lists)) |
| `backends[].url` | yes | — | Upstream base URL (see [Backend URL routing](#backend-url-routing)) |
| `backends[].api_key` | no | — | Token sent upstream (`Bearer` for OpenAI, `x-api-key` for Anthropic) |
| `backends[].weight` | no | `1` | Relative capacity share |
| `backends[].max_inflight` | no | `0` | Spill to another replica beyond this count; `0` is unlimited |
| `backends[].disabled` | no | `false` | Keep configured but exclude from routing |
| `model` | no | same as `name` | Model name sent to the backend (for rewriting) |
| `timeout` | no | `300` | Request timeout in seconds |
| `type` | no | `"openai"` | Backend protocol: `"openai"`, `"anthropic"`, or `"bedrock"` (see [Bedrock backends](#bedrock-backends)) |
| `responses_mode` | no | `"auto"` | Responses API handling: `"auto"`, `"native"`, or `"translate"` (see [Responses mode](#responses-mode)) |
| `developer_role` | no | `"user"` | Translated Responses requests: how a `developer` message after the first turn is sent — `"user"` (wrapped in `<developer>` tags), `"system"`, or `"developer"`. Leading developer/system messages always merge into one system message. See [docs/codex.md](codex.md#system-prompt-and-developer-messages) |
| `messages_mode` | no | `"auto"` | Messages API handling: `"auto"`, `"native"`, or `"translate"` (see [Messages mode](#messages-mode)) |
| `context_window` | no | `0` | Max context tokens. `0` = auto-detect from backend at startup |
| `defaults` | no | — | Default sampling parameters (see below) |

### Per-model sampling defaults

The `defaults` block sets sampling parameters that are injected when the client doesn't send its own. Coding assistants (Claude Code, Codex, Cursor, etc.) send their own parameters and are unaffected.

```yaml
models:
  - name: gemma4-31b
    backends:
      - url: http://192.168.13.30:8003/v1
    defaults:
      temperature: 0.7
      top_p: 1.0
      top_k: 40
      max_new_tokens: 4096
      frequency_penalty: 0.3
      presence_penalty: 0.0
      reasoning_effort: medium
```

| Field | Type | Description |
|---|---|---|
| `temperature` | float | Controls randomness (0.0 = deterministic) |
| `top_p` | float | Nucleus sampling threshold |
| `top_k` | int | Limits vocabulary to top K tokens |
| `max_new_tokens` | int | Maximum tokens to generate (maps to `max_tokens`) |
| `frequency_penalty` | float | Penalizes repeated tokens by frequency (0.0–2.0) |
| `presence_penalty` | float | Penalizes tokens that have appeared at all (0.0–2.0) |
| `reasoning_effort` | string | Backend-specific thinking budget, such as `low`, `medium`, `high`, or `xhigh` |
| `stop` | list | Strings that trigger end of generation |

## Chat helpers

The `processors` block retains chat-only helpers: transparent image description
and web search for chat backends that do not support them natively.

```yaml
processors:
  vision: Qwen3-VL-8B           # model name for vision processing (must be in models list)
  web_search_key: tvly-...      # Tavily API key for web search
```

| Field | Default | Description |
|---|---|---|
| `vision` | — | Model name to use for describing images sent to text-only backends. Must be a vision-capable model defined in `models`. |
| `web_search_key` | — | Search API key. Supports [Tavily](https://tavily.com/) (`tvly-...`) and [Brave Search](https://brave.com/search/api/) (`BSA...`) — provider is auto-detected from the key prefix. When set, the proxy executes web searches on behalf of clients (Claude Code, Codex) transparently. |

## Audio workloads

```yaml
audio:
  whisper:
    name: whisper-large-v3-turbo
    model: /models/faster-whisper-large-v3-turbo
    timeout: 300
    backends:
      - url: http://192.168.13.30:8007/v1
  tts:
    name: kokoro
    backends:
      - url: http://192.168.13.30:8009/v1
```

Whisper is exposed on `/v1/audio/transcriptions` and
`/v1/audio/translations`; TTS is exposed on `POST /v1/audio/speech`. Backends
that provide voice discovery, such as Kokoro-FastAPI, are also exposed through
`GET /v1/audio/voices`. Voice discovery is routed to the configured TTS
backends and returned without changing the upstream JSON. These retain the
upstream request/response contracts. Configured Whisper and TTS names also
appear in the proxy's aggregated `GET /v1/models` response, subject to the
requesting API key's model restrictions. Either workload may be omitted.

## Document workloads

```yaml
documents:
  paddleocr:
    endpoint: /layout-parsing
    health_endpoint: /health
    timeout: 300
    backends:
      - url: http://192.168.13.30:8002
```

The proxy exposes PaddleOCR-VL's pipeline service, `POST /layout-parsing` and
`POST /restructure-pages`, and forwards the official request and response
unchanged, so every pipeline option (`promptLabel`, `useSealRecognition`,
`restructurePages`, `outputFormats`, ...) works. Both routes are also served
under `/v1` for clients that only take a `/v1` base URL. `/restructure-pages`
goes to the sibling of `endpoint` (`/ocr/layout-parsing` gives
`/ocr/restructure-pages`). No artificial OpenAI model ID is required.

The PaddleOCR-VL model server itself (`paddleocr genai_server`) is an
ordinary OpenAI-compatible backend: add it under `models` to offer its
single-image task prompts (`OCR:`, `Table Recognition:`, `Formula
Recognition:`, `Chart Recognition:`) on `/v1/chat/completions`.

### Per-model processor overrides

Each model can override or disable global processors:

```yaml
models:
  - name: MiniMax-M2.5
    backends:
      - url: http://192.168.13.32:8000/v1
    # No vision → images routed to qwen-3.5 automatically

  - name: qwen-3.5
    backends:
      - url: http://192.168.13.30:8000/v1
    supports_vision: true         # model handles images natively
    processors:
      vision: none                # disable vision processing for this model

  - name: glm-5.1
    backends:
      - url: https://api.z.ai/api/coding/paas/v4
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

**Legacy OCR compatibility**: Existing `processors.ocr` settings remain readable
for tool-result images and rasterized pages. New document deployments should
use `documents.paddleocr`; if neither is available, the vision model is the
fallback.

**Web search**: When a client (Claude Code, Codex) includes a web search tool, the proxy converts it to a function tool that the backend can call. If the backend calls `web_search`, the proxy executes a Tavily search, injects the results, and re-sends to the backend. The client sees only the final response with search context incorporated.

**PDF processing**: Native text is extracted first. Scanned PDFs go to the
configured document layout service, with legacy OCR and vision as fallbacks.

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
| `email` | no | Set on keys issued by [enrollment](#self-service-api-keys-enrollment): the owner's lowercase address. One key per address |
| `join_key` | no | Set on enrolled keys: the id of the join key it came through |

Remove the `keys` section entirely to disable authentication (not recommended for public exposure).

## Self-service API keys (enrollment)

Instead of issuing keys by hand, an administrator can hand out a join link
per team. Whoever opens it enters a work email in one of the link's domains,
gets a one-time confirmation link by email (valid 30 minutes, single use), and
is shown their own API key once, with steps to download code-link, paste the
key and launch Claude Code or Codex. Someone who already has a key uses the
same link to replace it: the old key stops working. Keys are written to
`keys` like any other (name = email) and appear on the admin **API Keys** tab,
where they can be rotated or revoked as usual.

Set it up on the admin **Enrollment** tab, or in YAML:

```yaml
enrollment:
  public_url: https://llm.example.com      # where emailed links point (required)
  # paused: true                           # stops every join link
  smtp:
    host: smtp.example.com
    port: 587                              # default 587
    username: no-reply@example.com
    password_file: /run/secrets/smtp_password   # preferred; or password: ...
    from: "llm.example.com <no-reply@example.com>"
    reply_to: support@example.com
    # starttls: false                      # default true: refuses to send in plaintext
  # downloads:                             # defaults point at codelink.eidonix.com
  #   macos: https://example.com/code-link/macos
  #   windows: https://example.com/code-link/windows
  #   quickstart: https://example.com/code-link/quickstart
  join_keys:
    - id: example-co                       # recorded on every key it issues
      name: Example Co                     # shown on the join page
      key: replace-with-a-long-random-string  # the secret in /join/<key>; the admin page generates one
      domains: [example.com]               # exact, lowercase; at least one
      models: [my-model]                   # issued keys' allowed models; empty = all
      # paused: true
      # public: true                       # also accept these domains at /join
```

| Route | What it does |
|---|---|
| `GET /join/{key}` | The email form for one join link. An unknown or paused link (or enrollment paused or not configured) says "This link isn't active" |
| `POST /join/{key}` | Emails a confirmation link when the address's domain is allowed. Every submission gets the same answer, so the page can't be used to find out which addresses or domains can get a key |
| `GET /join` / `POST /join` | The same form without a link (code-link's "Get a key" opens it). The email's domain picks the active `public: true` join key that lists it; a domain may belong to only one public join key. With none active it asks for a team link |
| `GET /join/confirm/{token}` | Shows what the button will do ("Create my key" or "Get a new key"). Opening it changes nothing, so mail scanners that prefetch links can't use it up |
| `POST /join/confirm/{token}` | Uses the link up, creates or replaces the key and shows it once. The key is never emailed |

Notes:

- **Links use `public_url`, never the request's Host header**, so a forged
  Host can't point a confirmation link at another site. Join links answer
  "not active" until `public_url`, `smtp.host` and `smtp.from` are set.
- **Pending confirmations live in memory.** A restart invalidates them; the
  person just asks again.
- **Rate limits** (per hour): 120 page views and 10 submissions per client IP,
  3 confirmation emails per address, 200 submissions per join key, 20 key
  creations per IP. Client IPs honour `trusted_proxies` like the rest of the
  proxy.
- **Domains are an exact allowlist** (no subdomains), so only the
  addresses you list can get a key. Input is checked strictly: plain ASCII
  addresses only, no display names, quoting or IP-literal domains.
- Deleting a join link leaves the keys it issued working; revoke them on the
  API Keys tab. If a model is force-deleted and a join key's model list
  empties, the join key is paused rather than widened to every model.
- Without any `keys`, the proxy runs unauthenticated; the first enrolled key
  turns authentication on for everyone.

## Backend URL routing

The proxy appends the request path to the backend URL. How the path is constructed depends on the backend type:

**OpenAI backends** (`type: openai` or default): The proxy strips `/v1` from the client path. A client request to `POST /v1/chat/completions` with backend `http://host:8000/v1` is sent to `http://host:8000/v1/chat/completions`.

**Anthropic backends** (`type: anthropic`): A client request to `POST /v1/messages`
is passed through natively to the backend's `/v1/messages` route. A client
request to `POST /v1/chat/completions` is translated to Anthropic Messages,
sent to the same `/v1/messages` route, and translated back to an OpenAI
ChatCompletion response. Streaming, images, tools/tool results, model defaults,
sticky backend selection, and one-shot 5xx/transport failover are preserved.
For example, backend `https://api.minimax.io/anthropic` receives both client
protocols at `https://api.minimax.io/anthropic/v1/messages`.

### Examples

```yaml
models:
  # Standard OpenAI-compatible (vLLM, llama-server)
  - name: MiniMax-M2.5
    backends:
      - url: http://192.168.100.10:8000/v1

  # Non-standard path (Zhipu GLM)
  - name: glm-5.1
    backends:
      - url: https://api.z.ai/api/coding/paas/v4
        api_key: your-key

  # Anthropic — base URL omits /v1
  - name: claude-sonnet-4-20250514
    backends:
      - url: https://api.anthropic.com
        api_key: sk-ant-your-key
    type: anthropic

  # Third-party Anthropic-compatible
  - name: MiniMax-M2.7
    backends:
      - url: https://api.minimax.io/anthropic
        api_key: your-key
    type: anthropic
```

## Bedrock backends

`type: bedrock` routes requests to AWS Bedrock's [Converse API](https://docs.aws.amazon.com/bedrock/latest/APIReference/API_runtime_Converse.html). Both Anthropic-style (`/v1/messages`) and OpenAI-style (`/v1/chat/completions`) clients are supported — the proxy translates each into Converse, signs the request, and translates the response back to whatever shape the client speaks. Streaming and tool calls work end-to-end in both directions.

The Responses API (`/v1/responses`) is **not** supported for Bedrock; clients hit those endpoints will receive a 400 with a pointer to `/v1/chat/completions` or `/v1/messages`.

### Authentication

Bedrock supports two auth styles. Pick whichever fits your deployment.

**Bedrock API key (simplest).** Set `api_key` to a Bedrock-issued key (introduced 2025; mint via the Bedrock console). The proxy sends it as a bearer token — no SigV4 signing.

```yaml
- name: claude-sonnet-4-bedrock
  type: bedrock
  region: us-east-1
  model: us.anthropic.claude-sonnet-4-20250514-v1:0
  backends:
    - url: https://bedrock-runtime.us-east-1.amazonaws.com
      api_key: bdrk-...
```

**IAM static credentials (SigV4).** Set `aws_access_key` + `aws_secret_key`, or omit them and the proxy reads `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` / `AWS_SESSION_TOKEN` from the environment. Each request is signed with SigV4.

```yaml
- name: claude-sonnet-4-bedrock
  type: bedrock
  region: us-east-1
  model: us.anthropic.claude-sonnet-4-20250514-v1:0
  backends:
    - url: https://bedrock-runtime.us-east-1.amazonaws.com
  aws_access_key: AKIA...
  aws_secret_key: ...
  # aws_session_token: ...   # optional, for STS temporary credentials
```

| Field | Required | Default | Description |
|---|---|---|---|
| `region` | yes | — | AWS region, e.g. `us-east-1` |
| `model` | yes | — | Bedrock model ID or inference profile (e.g. `us.anthropic.claude-sonnet-4-20250514-v1:0`) |
| `backends` | yes | — | Bedrock runtime endpoint(s); use a VPC endpoint here when applicable |
| `backends[].api_key` | one of | — | Bedrock API key (bearer auth, no SigV4) |
| `aws_access_key` | one of | env `AWS_ACCESS_KEY_ID` | IAM access key ID for SigV4 |
| `aws_secret_key` | one of | env `AWS_SECRET_ACCESS_KEY` | IAM secret for SigV4 |
| `aws_session_token` | no | env `AWS_SESSION_TOKEN` | STS session token if using temporary credentials |

Either `api_key` OR (`aws_access_key` + `aws_secret_key`) is required. `messages_mode` and `responses_mode` have no effect on Bedrock backends — the wire protocol is always Converse.

### Notes

- **Inference profiles** (cross-region routing IDs like `us.anthropic.claude-sonnet-4-20250514-v1:0`) work as the `model` value; the proxy URL-escapes them correctly for SigV4.
- **Vision** works natively for Anthropic models on Bedrock — image content blocks (Anthropic-style) and `image_url` data: URLs (OpenAI-style) are both translated to Converse `image` blocks. Remote `https://` image URLs are dropped (Bedrock does not fetch external images).
- **Tools** translate in both directions, including streaming tool-call deltas.
- **Reasoning** (`reasoningContent`) from Claude is preserved: as `thinking` blocks for Anthropic clients, and as the non-standard `reasoning` field for OpenAI clients.

## Responses mode

Controls how the proxy handles Responses API requests (`POST /v1/responses`, `POST /v1/responses/compact`) per model:

| Value | Behavior |
|---|---|
| `auto` | Default. Probe backend on first request; cache result; fall back to translation on 404 |
| `native` | Always passthrough. Never translate. Backend must support the Responses API |
| `translate` | Always translate to Chat Completions. Skip the probe entirely |

```yaml
- name: MiniMax-M2.5
  backends:
    - url: http://192.168.100.10:8000/v1
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
  backends:
    - url: http://192.168.100.10:8000/v1

# Auto: Anthropic backend → passthroughs natively
- name: claude-sonnet-4
  backends:
    - url: https://api.anthropic.com
  type: anthropic
```

The translation supports text, tool calling, reasoning tokens (emitted as thinking blocks), and streaming. See [claude-code.md](claude-code.md) for full details.

## Connect backends

Anywhere a backend URL is accepted (model backends, audio, documents, vision and other processor models, Qdrant), `connect://<site>/<service>/<path>` can be used instead of `http(s)://`. It resolves to `<connect.url>/<site>/<service>/<path>`, with `X-Eidonix-Application` and `X-Eidonix-For` labels, and its health comes from the hub catalog. See [connect.md](connect.md).

## Context window detection

At startup, the proxy queries each backend to discover context window sizes:

- **llama.cpp / llama-server**: `/props` endpoint → `default_generation_settings.n_ctx` (actual runtime context respecting `--ctx-size`)
- **vLLM**: `/v1/models` → `max_model_len` field
- **Anthropic**: `GET /v1/models/{model_id}` → `max_input_tokens`

Detection runs asynchronously (non-blocking). Results are served through the proxy's `/v1/models` endpoint and used in the config generator.

Set `context_window` explicitly for backends that don't report it:

```yaml
- name: MiniMax-M2.7
  backends:
    - url: https://api.minimax.io/anthropic
  type: anthropic
  context_window: 1048576   # 1M tokens
```

## Services

External service proxies configured under the `services` key. Currently supports Qdrant vector database.

```yaml
services:
  qdrant:
    backend: http://192.168.5.143:6333
    api_key: your-qdrant-api-key
    app_keys:
      - name: webapp
        key: qd-webapp-abc123
      - name: crawler
        key: qd-crawler-def456
```

| Field | Required | Description |
|---|---|---|
| `qdrant.backend` | yes | Qdrant server URL (`http(s)://` or `connect://<site>/<service>`) |
| `qdrant.api_key` | no | API key sent to the Qdrant backend |
| `qdrant.app_keys` | yes | Application keys for proxy authentication |
| `qdrant.app_keys[].name` | yes | Friendly name (used for isolation and logging) |
| `qdrant.app_keys[].key` | yes | The API key clients send via `Authorization: Bearer` |

App keys are separate from model API keys. Each app's data is automatically isolated — the proxy injects the app name into point payloads on writes and filters by app on searches. See [docs/qdrant.md](qdrant.md) for full details.

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
    backends:
      - url: https://api.z.ai/api/coding/paas/v4
        api_key: your-zhipu-key

  # Sonnet + Haiku slots — fast, capable all-rounder
  - name: MiniMax-M2.5
    backends:
      - url: http://192.168.13.32:8000/v1
    responses_mode: translate
    timeout: 600

  # Vision processor — general image description
  - name: Qwen3-VL-8B
    backends:
      - url: http://192.168.13.30:8000/v1
    supports_vision: true

processors:
  vision: Qwen3-VL-8B
  web_search_key: tvly-your-tavily-key

documents:
  paddleocr:
    backends:
      - url: http://192.168.13.30:8002
```

Claude Code model slot mapping: Opus → `glm-5.1`, Sonnet → `MiniMax-M2.5`, Haiku → `MiniMax-M2.5`.

### Codex CLI

```yaml
models:
  - name: MiniMax-M2.5
    backends:
      - url: http://192.168.13.32:8000/v1
    responses_mode: translate    # required for vLLM backends
    timeout: 600

  - name: Qwen3-VL-8B
    backends:
      - url: http://192.168.13.30:8000/v1
    supports_vision: true

processors:
  vision: Qwen3-VL-8B
  web_search_key: tvly-your-tavily-key

documents:
  paddleocr:
    backends:
      - url: http://192.168.13.30:8002
```

Codex uses a single model. Set `responses_mode: translate` for any vLLM backend — vLLM's native `/v1/responses` endpoint bypasses the proxy pipeline, breaking web search, image, and PDF processing.

### Processor model recommendations

| Role | Recommended | Parameters | Notes |
|---|---|---|---|
| **Vision** | [Qwen3-VL-8B](https://huggingface.co/Qwen/Qwen3-VL-8B-Instruct) | 8B | Best quality/speed balance for image description. Strong on charts, screenshots, diagrams. |
| **OCR** | [PaddleOCR-VL-1.5](https://huggingface.co/PaddlePaddle/PaddleOCR-VL-1.5) | 0.9B | Purpose-built for document parsing. 94.5% accuracy, 109 languages, minimal VRAM. |
| **OCR (alt)** | [DeepSeek-OCR 2](https://huggingface.co/deepseek-ai/DeepSeek-OCR) | 3B | Higher accuracy (97%), layout analysis, table extraction. ~2,500 tok/s on A100. |
| **Vision (alt)** | [Qwen3-VL-2B](https://huggingface.co/Qwen/Qwen3-VL-2B-Instruct) | 2B | Lighter alternative if GPU memory is tight. Good OCR (32 languages). |
