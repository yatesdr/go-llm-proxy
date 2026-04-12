# Changelog

All notable changes to this project will be documented in this file.

## v0.3.4

### Added
- **Recursive streaming search loop** — when the model requests additional searches after receiving results, the proxy now executes them (up to 10 iterations) instead of passing unexecutable `function_call` items to the client. Fixes "Try again" churn with search-heavy queries.
- **Content buffering for search transitions** — buffers content when web search is enabled and discards "transition text" like "Let me search..." when followed by search tool calls. Prevents intermediate model commentary from appearing in client output.
- **Search result sources in web_search_call** — `web_search_call` output items now include a `sources` array with URL citations, matching the Responses API spec and enabling proper Codex search result display.
- **Think tag filtering for Chat Completions** — `<think>...</think>` tags are now stripped from Chat Completions responses (streaming and non-streaming), not just Responses API. Reasoning models using think tags now work cleanly with all API formats.
- **External backend detection for health checks** — backends pointing to external APIs (api.openai.com, api.anthropic.com, etc.) are now detected and only checked once at startup, then updated based on actual usage. Prevents spamming external APIs with health check requests.
- **Per-model sampling defaults** — new `defaults:` config section per model to set default `temperature`, `top_p`, `top_k`, `max_new_tokens`, and `stop` parameters. Defaults are injected into requests that don't specify them, useful for backends like SGLang that lack server-side default configuration.

### Fixed
- **Tool call event duplication** — when the proxy executes searches, it no longer emits `function_call` events followed by `web_search_call` events for the same search. Only the `web_search_call` items are emitted.
- **Pending search calls at iteration limit** — when the search loop hits max iterations with pending search tool calls, they are now discarded instead of being emitted as orphaned `function_call` items.

---

### Added
- **Model availability tracking** — background health checker probes each backend every 30 seconds using HEAD requests, tracking online/offline status per model. New `GET /v1/models/status` endpoint exposes real-time health data. Config generator page now displays Online/Offline badges for each model including error messages when offline.

### Fixed
- **Responses API auto-detection with SGLang** — native passthrough probe now treats HTTP 500 and 405 as "not supported" (in addition to 404), falling back to Chat Completions translation. SGLang returns 500 for unrecognized routes instead of 404, which previously caused the proxy to retry native passthrough indefinitely instead of falling back.


## v0.3.3

### Fixed
- **PowerShell Tavily MCP config** — `claude mcp add-json` has quoting issues in PowerShell. Config generator now writes MCP server config directly to `~/.claude.json` using native PowerShell JSON handling

## v0.3.2

### Added
- **`/v1/messages/count_tokens` endpoint** — proxies to native Anthropic backends, returns rough estimate for translated backends (fixes Claude Code context management)
- **Standalone config generator** for GitHub Pages deployment
- **Usage dashboard** requests/tokens chart toggle

### Changed
- Translate `server_tool_use` and `web_search_tool_result` blocks in conversation history so search context survives multi-turn translated conversations
- Post-response search tool loop for Chat Completions clients (OpenCode, Qwen Code): streaming + non-streaming detection and re-send
- Extract `copyResponseHeaders`, `proxyRequestContext`, and `logUsageFromChatResponse` helpers to reduce duplication in proxy
- README: full compatibility matrix, supported clients list, config generator link in docs nav

### Fixed
- **PowerShell config generator** — write `--settings` JSON to a temp file to avoid PowerShell quoting issues with inline JSON containing curly braces and colons

## v0.3.1

### Security
- **SSRF protection for vision pipeline** — image URLs are now validated before forwarding to vision backends. Blocks private/internal network ranges (RFC 1918, loopback, link-local, cloud metadata 169.254.x), non-http(s) schemes, and DNS-rebinding attacks. Data URIs are always allowed.
- **Sanitize backend error responses** — the compact endpoint (`/v1/responses/compact`) was forwarding raw backend error bodies to clients, potentially leaking internal URLs, API keys, or infrastructure details. Now returns a generic error like all other endpoints.
- **Sanitize internal error messages** — Go error strings from translation, pipeline processing, search, and vision model failures are now logged server-side only. Clients receive generic error messages without internal details.
- **Rate limiter eviction at capacity** — when the IP tracker reaches its 100K entry limit, the oldest record is now evicted instead of silently dropping the new entry. Prevents distributed attacks from bypassing rate limiting.

### Changed
- Keepalive goroutine race condition fix consolidated into `runPipelineWithKeepalives()` with `sync.Mutex` (both Messages and Responses handlers)
- Image and PDF caches migrated from `sync.Map` to `boundedCache` (max 1024 entries with full eviction)
- Brave Search URL construction uses `url.QueryEscape` instead of manual string replacement
- `sendChatCompletionsRequest` makes a shallow copy before setting `stream=false` to avoid mutating the caller's map
- `WriteError` now returns context-appropriate error types (authentication_error, rate_limit_error, server_error) instead of always returning invalid_request_error
- Context window detection uses `httputil.NewHTTPClient()` (redirect-refusing) instead of default `http.Client`
- Usage logging deduplicated into shared `logUsageRecord()` helper
- PDF processor uses `normalizeContentParts()` for consistent `[]any`/`[]map[string]any` handling

## v0.3.0

### Added
- **Full Codex CLI compatibility** — all pipeline features (web search, image description, PDF OCR) tested and working end-to-end with Codex
  - Recognize `web_search` tool type (current Codex) in addition to legacy `web_search_preview`
  - Emit proper `reasoning` output items with streaming `reasoning_summary_text.delta` events for Codex's thinking display
  - Emit `web_search_call` output items for Codex's native search UI
  - Handle structured `function_call_output` (arrays/objects) from Codex's `view_image` tool — was silently dropping image data
  - Handle `mcp_tool_call_output` and `mcp_list_tools` item types in Responses API input translation
  - Enrich usage format with `input_tokens_details` / `output_tokens_details` fields
- **Dual vision + OCR image pipeline**
  - User-attached images: vision model description only (no OCR — dedicated OCR models hallucinate on photos)
  - Tool output images (Codex `view_image`, screenshots): dedicated OCR model with `OCR:` prompt
  - Scanned PDFs (Claude Code `processPDFs` fallback): OCR model preferred over vision model, ~17x faster with PaddleOCR-VL
  - Separate cache keys per mode (`:v` for vision, `:o` for OCR)
- **Brave Search API support** — auto-detected from `web_search_key` prefix (`BSA` → Brave, `tvly-` → Tavily). No new config fields needed
- **Proxy MCP search for Qwen Code** — config generator adds proxy `/mcp/sse` endpoint to Qwen Code's `mcpServers`, enabling Brave Search support via proxy
- **Pipeline documentation** (`docs/pipeline.md`) — comprehensive reference for image, PDF, and web search behavior per coding client
- **Recommended model recipes** in quick start and config-reference — Qwen3-VL-8B for vision, PaddleOCR-VL-1.5 for OCR, Tavily/Brave for search

### Changed
- Landing page diagram: split local/cloud backends, add Web Search service, add cloud API passthrough example
- Compatibility matrix: distinguish proxy-side vs client-side features, show OCR model usage, all four clients support web search
- Config generator: per-client search key hints, Qwen Code proxy MCP integration, OpenCode prioritizes client-side Tavily over proxy MCP when key entered
- Renamed "Qwen Coder" → "Qwen Code" throughout

### Fixed
- **Responses API `[]map[string]any` content type mismatch** — vision pipeline failed to detect images from Responses API translation path. Added `normalizeContentParts()` to handle both `[]any` and `[]map[string]any`
- **Structured tool output silently dropped** — `inputItem.Output` was typed as `string` but Codex `view_image` returns `[{type: input_image, ...}]`. Changed to `json.RawMessage` with `translateToolOutput()` for arrays/objects
- **OCR not triggering for Codex PDFs** — heuristic required 3+ images per tool message; Codex sends 1 per `view_image`. Changed to trigger OCR for all tool-role images
- **Config generator syntax error** — stray closing brace in OpenCode MCP config broke the page

## v0.2.1

### Added
- **Processing pipeline** for transparent content handling on text-only backends
  - **Vision pipeline**: images sent to text-only models are described by a vision-capable model and replaced with text. Configurable via `processors.vision` in config.
  - **Web search**: proxy intercepts server-side search tools (`web_search_20250305` for Claude Code, `web_search_preview` for Codex), executes via Tavily, and injects results. Configurable via `processors.web_search_key`. Works in both streaming and non-streaming modes with multi-iteration tool loops.
  - **PDF processing**: text extraction via pure Go library with vision model fallback for scanned/image PDFs. Anthropic `type: "document"` blocks translated to text before sending to backend.
  - **Per-model processor overrides**: `supports_vision`, `force_pipeline`, per-model `processors` block with `vision: none` to disable
  - Auto-infer `supports_vision` on models referenced as vision processors
  - SSE keepalive comments during pipeline processing to prevent client timeout
- **MCP SSE endpoint** (`/mcp/sse`, `/mcp/messages`) exposing `web_search` tool for OpenCode, Qwen Code, and any MCP-compatible client
- **Config generator restored** (full UI, ~1200 lines)
  - Tool selector: Claude Code, Codex, OpenCode, Qwen Code
  - Claude Code: Sonnet/Opus/Haiku model selectors, thinking toggles, `settings.json` and start scripts (.sh/.bat/.ps1)
  - Codex: model, reasoning effort, context window selectors, `config.toml` and start scripts
  - OpenCode: build/plan agent selectors with model checkboxes, `opencode.json`
  - Qwen Code: default + additional model multi-select, `settings.json`
  - Per-OS installation instructions, download buttons, copy-to-clipboard
  - Proxy-side web search awareness: skips client Tavily MCP when proxy has `web_search_key`, uses proxy MCP endpoint for OpenCode
  - SVG logo, "Data Safety" column, `CLAUDE_CODE_DISABLE_1M_CONTEXT` env var
  - MCP config card shown when web search is configured
- **Landing page redesign** with compatibility matrix, visual diagram, and coding agent focus

### Changed
- Consolidated `doc/` into `docs/` for GitHub Pages compatibility
- Moved Docker files into `docker/` directory
- Simplified `docker-compose.yml` to config-file-driven settings
- Rewrote `README.md`, `config.yaml.example`, and all documentation for pipeline features
- `docs/index.html` redesigned with hero section, compatibility matrix, and updated doc links

### Fixed
- **Streaming `content_block_stop` re-buffering**: tool_use stop events were re-buffered by `bufferOrEmit()` after replay, never reaching the client. Claude Code saw incomplete tool_use blocks and couldn't execute tools (Read, Bash, etc.). Fixed by emitting stop events directly after replay.
- **Vision `[]map[string]any` type mismatch**: Messages API translation produces `[]map[string]any` message slices, but `processImages` only handled `[]any`. Images silently passed through unprocessed to text-only backends.
- **Reasoning tokens consuming vision budget**: vision model calls with thinking enabled could produce empty content (all tokens spent on chain-of-thought). Fixed by disabling thinking for vision utility calls.
- **Client context cancellation killing vision calls**: vision HTTP calls now use a dedicated 60s timeout detached from the client connection, surviving client disconnects during processing.
- **Copy button on HTTP**: fallback to `execCommand("copy")` when `navigator.clipboard` API unavailable in non-secure contexts.
- Config page logo sizing (missing `.header-logo` CSS class)

## v0.2.0

### Added
- **OpenAI Responses API support** (`POST /v1/responses`, `POST /v1/responses/compact`)
  - Native passthrough for backends that support the Responses API (e.g. OpenAI, Azure OpenAI)
  - Automatic translation to Chat Completions for backends that don't (vLLM, llama-server, etc.)
  - Auto-detection with cached fallback: probes backend on first request, caches the result
  - Streaming SSE translation with full event lifecycle (created, deltas, done, completed)
  - Reasoning token support: translates `delta.reasoning` from Chat Completions into Responses API reasoning events
  - Context compaction endpoint with model-based summarization fallback for non-native backends
  - Proper `type` field in all SSE event payloads per Responses API spec
  - Streaming error handling: upstream errors wrapped as `response.failed` SSE events
- **Codex CLI support** in the config generator
  - Generates `config.toml` with custom provider, reasoning effort, context window, and web search settings
  - Start command mode using `-c` CLI overrides (no config file changes needed)
  - API keys embedded directly via `experimental_bearer_token` (env var alternative commented in output)
  - Tavily MCP web search integration (with `http_headers` for embedded key)
  - Context window auto-detection displayed in UI with amber highlight when not detected
  - Filters Anthropic-protocol models from Codex model selector
- **`responses_mode` config field** per model: `auto` (default), `native`, or `translate`
  - `native`: always passthrough, never fall back to translation
  - `translate`: always translate to Chat Completions, skip native probe
  - `auto`: probe on first request, cache result, fall back as needed
- **Context window auto-detection** from backends at startup
  - Queries `/v1/models` for `max_model_len` (vLLM), `meta.n_ctx_train` (llama-server), or `max_input_tokens` (Anthropic)
  - Async, non-blocking — failures logged as warnings
  - Results served through `/v1/models` endpoint in `max_model_len` field
  - Manual override via `context_window` config field per model
- **Per-request usage logging** to SQLite with token extraction for OpenAI and Anthropic formats
  - `--log-metrics` CLI flag or `log_metrics` config field
  - Configurable database path via `usage_db` config field
- **Usage dashboard** at `/usage` with password authentication
  - Daily, per-user, and per-model breakdowns with token counts
  - JSON data API at `/usage/data` with configurable time range
  - Rate-limited login with secure cookie-based sessions
- **CLI usage reports**: `--usage-report` and `--model-report` flags with tabular output
- Documentation: `docs/codex.md` (Codex setup, Responses API details), `docs/usage.md` (usage logging and dashboard)

### Changed
- Trusted proxies now update on config reload (previously required a restart)
- Improved error handling throughout: streaming writes, multipart rewriting, and HTTP responses
- Improved IPv6 support for private IP detection in config page
- Rate limiter comments clarified to match actual behavior
- `/v1/models` response now includes `max_model_len` when available
- `fsnotify` correctly listed as direct dependency in go.mod

### Fixed
- Context window detection now holds write lock during config update to prevent race with config reload
- Usage dashboard `?days` parameter capped at 365 to prevent expensive queries
- Responses API streaming handler enforces `maxResponseBodySize` (100 MB) matching the main proxy handler

## v0.1.0

### Added
- OpenAI and Anthropic API passthrough proxy with model routing
- Anthropic Messages API support via `/v1/messages` and explicit `/anthropic/` route prefix
- Anthropic-style client authentication via `x-api-key` header (in addition to `Authorization: Bearer`)
- Backend type configuration (`type: anthropic`) for models using the Anthropic Messages API
  - Sends `x-api-key` header upstream instead of `Authorization: Bearer`
  - Forwards `Anthropic-Version` and `Anthropic-Beta` request headers to upstream
  - Forwards `Request-Id` response header from upstream
  - Preserves `/v1` in upstream path (Anthropic convention: base URL omits `/v1`)
- Model name rewriting (expose friendly names while backends use internal identifiers)
- API key authentication with per-key model access control
- IP-based rate limiting and throttling for failed auth attempts
- Streaming (SSE) support with proper flush handling
- Hot-reload config via filesystem watching and `SIGHUP`
- Graceful shutdown with connection draining
- Interactive `--adduser` CLI for API key creation
- Config generator web UI at `GET /` for Claude Code, Qwen Code, and OpenCode
- Multi-platform builds (Linux, macOS, Windows) and Docker images (amd64, arm64)
- Comprehensive config validation (URLs, types, duplicates, key-model references)
- Security hardening: path allowlist, body size limits, constant-time key comparison, SSRF prevention
