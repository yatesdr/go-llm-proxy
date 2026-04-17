# Changelog

All notable changes to this project will be documented in this file.

## Unreleased

### Fixed

- **Scanned-PDF extraction broken via Anthropic Messages.** `processPDFs` picked exactly one fallback model (OCR if configured, else vision) and called it once. Since paddleOCR-style OCR backends reject raw `data:application/pdf` input (they hallucinate fallback text rather than erroring), every scanned PDF silently failed. Replaced with a three-stage cascade: (1) native text extraction, (2) rasterize PDF to PNG pages → send each page to dedicated OCR model, (3) vision fallback with raw PDF. Rasterization uses `pdftoppm` (poppler-utils) or `gs` (Ghostscript) if available; if neither is installed, stage 2 is skipped and the vision model handles scanned PDFs directly. Added `poppler-utils` to the Docker image.
- **Tool-role image OCR failure was terminal.** Parallel fix in `processImages`: tool-role images (Codex `view_image` output, screenshots) now cascade OCR → vision. User-role natural photos keep the existing vision-only path — dedicated OCR models hallucinate on photographs and this is explicitly documented in the code.
- **Failure cache poisoning.** PDF extraction failures were written to `pdfCache.Store` (permanent). Added `boundedCache.StoreWithTTL` and moved all cascade failure placeholders to a 5-minute TTL so transient upstream hiccups no longer permanently block a document.
- **Cross-API PDF ingestion inconsistency.** Chat Completions and Responses API clients submitting PDFs as `image_url` with `data:application/pdf;base64,...` were silently routed through the vision pipeline (which can't extract text reliably). Added `NormalizePDFDataURLs` which runs before image and PDF processing, converting any PDF data URL into the pipeline-internal `pdf_data` shape that Anthropic's `document` block already produced. Also updated the Responses translator (`responses_translate.go`) to emit `pdf_data` directly for `input_image` parts whose URL is a PDF data URL, so Codex-style clients route correctly from the entry point.
- **Vision response parser ignored `reasoning_content`.** `describeImage` read only `message.content`; reasoning-model vision backends (e.g., Qwen3-VL variants that thought before answering) would emit an empty `content` with the actual description in `reasoning_content`, especially when `finish_reason=length` truncated the response mid-thinking. The proxy treated that as a failed vision call and the cascade collapsed. Now falls back to `reasoning_content` when `content` is empty, with a debug log line recording which field was used. Found during live testing against Qwen3-27B-VL — scanned-PDF cascade was hitting the vision model successfully, the model was producing correct page transcriptions, and the proxy was discarding them because they arrived in the wrong response field.

### Changed

- **Pipeline output uses XML-like tags** so target models don't conflate pipeline-injected content with user-authored text. `[Image description: ...]` → `<image_description>...</image_description>`; `[Page text: ...]` → `<page_text>...</page_text>`; `[PDF: foo.pdf]\n\n...` → `<pdf_content filename="foo.pdf" source="text|ocr|vision">\n...\n</pdf_content>`. Failure strings (`[Image could not be processed]`, `[PDF content could not be extracted]`) are unchanged. **Breaking for any downstream consumer that parsed the old bracket format** — this is an internal injection pipeline so no public schema is involved, but operators with custom logging/parsing on these strings should update.

### Verified

- Full unit test suite (`go test -race ./...`) green across all packages.
- New cascade tests cover: OCR success + vision not called; OCR empty → vision rescues; OCR error → vision rescues; both fail → TTL-cached placeholder; single-processor configuration does not duplicate calls; user-role images never hit OCR.
- Cross-API normalization tested for Anthropic Messages (`document`), Chat Completions (`image_url` with PDF data URL), and Responses API (`input_image` with PDF data URL).

## v0.3.8

### Security

- **SSRF via DNS rebinding (vision pipeline) — fixed.** The old `isSSRFSafe` check resolved the image hostname once up-front, then passed the hostname string to the vision backend, which resolved it again. A short-TTL DNS record could flip between a public IP (check) and `169.254.169.254` or loopback (fetch). Fixed structurally: the proxy now fetches the image in-process via an SSRF-safe `http.Transport` whose dialer validates every resolved IP at connect time (`net.Dialer.Control`), base64-encodes it, and forwards a `data:` URL to the vision backend — so the backend never makes an outbound request of its own. Expanded the blocklist to cover `0.0.0.0`, `::`, CGNAT (`100.64.0.0/10`), and IPv4-mapped IPv6 (`::ffff:*`).
- **Rate-limiter DoS / brute-force amplification — fixed.** Prior `evictOldest()` was an O(n) scan over up to 100,000 tracked IPs under a write lock that the auth hot path also took. An attacker with an IPv6 /64 could push the tracker to capacity, then every subsequent failed-auth request scanned 100k entries while blocking *all* auth checks system-wide. Replaced the linear scan with a doubly-linked-list LRU so insert/evict are O(1) and auth latency is independent of tracker size.
- **Usage dashboard session takeover — fixed.** Cookie value was a deterministic HMAC of the password — no per-session entropy, no server-side expiry, no way to revoke without rotating the password, no logout endpoint. Captured cookies retained access forever. Replaced with 256-bit random session tokens backed by a bounded server-side store with explicit TTL and logout. The `Secure` flag is now set only when the request arrived over TLS or from a trusted proxy (previously any client could spoof `X-Forwarded-Proto: https`).
- **AWS error-body log leakage — fixed.** Bedrock error responses include AWS account IDs, ARNs, and request IDs; these were being written verbatim to logs. Added `awsauth.ScrubAWSErrorBody` to redact identifiers before logging.
- **HTTP transport lacked phase-level timeouts.** Every upstream call used `&http.Client{}` with default transport — no dial, TLS-handshake, or response-header timeout, so a slow-loris upstream could pin a goroutine for the full context window. Added explicit timeouts at every phase plus bounded idle-connection pool.
- **Qdrant admin-endpoint isolation bypass — fixed.** App keys could list, create, or delete collections through `/qdrant/collections/*` — the isolation filter only applied to points-level routes. Flipped to strict allowlist: only isolation-covered operational paths (points upsert / search / scroll / query / count / delete + GET collection schema) are accepted. All other paths return 403.
- **Bedrock streaming size cap.** `streamBedrockToAnthropicSSE` / `streamBedrockToChatSSE` had no aggregate byte limit; a compromised or runaway upstream could stream unbounded data. Now capped at `api.MaxResponseBodySize` (100 MB), matching every other SSE path.
- **Goroutine leak + data race on client disconnect during streaming search.** When a client disconnected mid-search, the handler broke out of its wait loop but left the search goroutine running and read its output variables racily. Extracted `waitForSearchOrDisconnect` helper that awaits the goroutine (bounded by a grace period) before returning, applied to all three streaming handlers (Messages, Responses, ProxyChat).
- **Memory bloat on large streaming responses.** `streamRawResponse` captured the full body (up to 100 MB) into memory solely for token-usage extraction, doubling memory per request and risking OOM under concurrency. Introduced `captureBuffer` that keeps a 16 KB prefix + 64 KB rolling tail for streaming (enough to catch both the first-chunk metadata and the final `include_usage` chunk), or a 1 MB cap for non-streaming.
- **Multipart model-field OOM — fixed.** `ExtractModelFromMultipart` read the `model` field via `io.ReadAll` with no size cap; a 50 MB field could OOM the parser. Now capped at 1 KB.
- **Auth timing side-channel — fixed.** `findKey` and `findAppKey` early-returned on the first matching key, leaking which index matched via total request latency. Changed to iterate all keys unconditionally so total time is independent of match position.

### Changed

- Consolidated ten hand-rolled `usage.UsageRecord` constructions behind a single `logUsage` helper with typed `logUsageChat` / `logUsageConverse` adapters; fields can no longer drift between call sites.
- Extracted `prepareBedrockRequest` plumbing shared by `MessagesHandler.handleBedrock` and `ProxyHandler.handleBedrockChat` (URL build, SigV4 vs API-key auth, headers).
- Split model-extraction / multipart rewrite helpers out of `shared.go` into a dedicated `model_rewrite.go`.
- Rate limiter exposes `IsTrustedProxy` for reuse by the usage dashboard's TLS-detection path.

### Fixed

- **Bedrock context window auto-detection.** v0.3.7 attempted a `/models` probe against `bedrock-runtime.*.amazonaws.com`, which doesn't exist on that endpoint, producing a spurious warning for every Bedrock model at startup. Replaced with a prefix-match lookup table of well-known Bedrock models (Claude, Nova, Llama, Mistral, Cohere, Z.ai GLM, DeepSeek, Titan) so common models get a sensible default. Unknown models log at debug (not warn) and the operator can always set `context_window` explicitly.
- Race in `TestHealthStore_AuthHeaders` — concurrent health-check goroutines wrote to shared captured-header vars without synchronization.
- Broken indentation under bare-brace `else` branches in `messages.go` (compile-correct today, trap for the next edit).
- Dead `applyConverseSamplingDefaultsForChat` one-line wrapper removed.
- `.gitignore` now covers platform-suffixed build artifacts (`go-llm-proxy-linux`, etc).

### Verified

Live-tested end-to-end against AWS Bedrock (`zai.glm-4.7-flash` on-demand + `us.amazon.nova-lite-v1:0` inference profile, us-east-2) across all four client × stream-mode combinations plus tool calls. No regressions; context-window auto-detection works (128k and 300k respectively).

## v0.3.7

### Added
- **AWS Bedrock backend** — new `type: bedrock` model type proxies to Bedrock's Converse / ConverseStream API. Both `/v1/messages` (Anthropic-protocol clients) and `/v1/chat/completions` (OpenAI-protocol clients) work end-to-end against the same Bedrock-hosted model — the proxy translates each client shape to Converse, signs the request, then translates the response back. Streaming, tool calls, vision (data URLs), and reasoning content are supported in both directions. Configured per model with `region` plus either `api_key` (Bedrock API key — bearer auth) or `aws_access_key` + `aws_secret_key` (IAM credentials, falling back to `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`/`AWS_SESSION_TOKEN` env vars). Backend URL auto-derives from the region; override only for VPC endpoints.
- **Hand-rolled SigV4 signer** (`internal/awsauth`) — implements AWS Signature Version 4 for Bedrock requests. Validated against the canonical AWS-published signing-key derivation vector. No AWS SDK dependency; the implementation is ~330 lines.
- **AWS event-stream decoder** (`internal/awsstream`) — parses `application/vnd.amazon.eventstream` binary frames (prelude + headers + payload + CRC32) used by Bedrock streaming responses. Powers the streaming bridges that translate Bedrock events into Anthropic SSE and OpenAI Chat Completions SSE.
- **Inference-profile model IDs** — Bedrock cross-region inference profile IDs (e.g. `us.anthropic.claude-sonnet-4-20250514-v1:0`) are accepted unchanged as the `model` field; the proxy URL-escapes them correctly for both the request path and SigV4 canonical URI.
- **Documentation** — new "Bedrock backends" section in `docs/config-reference.md` covering both auth styles, field reference, and notes on vision / tools / reasoning. `config.yaml.example` includes a commented Bedrock entry.

### Changed
- `type` field in model config now accepts `bedrock` in addition to `openai` and `anthropic`. Unknown types still error out.
- `/v1/responses` returns a 400 with a helpful pointer (`use /v1/chat/completions or /v1/messages`) when targeted at a Bedrock-typed model — the Responses API has no Converse equivalent.

## v0.3.6

### Added
- **Qdrant vector database proxy** — new `/qdrant/*` endpoint proxies requests to a Qdrant backend with separate authentication via app keys. Configured under `services.qdrant` in config. App keys are independent from model API keys, allowing fine-grained access control for vector database operations.
- **App isolation for Qdrant** — automatic multi-tenant isolation without client changes. The proxy injects an `app` field into point payloads on writes and adds a filter clause on searches/queries to restrict results to the calling app's data. Apps cannot access each other's vectors.
- **Sampling defaults: `frequency_penalty`, `presence_penalty`, `reasoning_effort`** — three new fields in per-model `defaults` config. Injected when the client doesn't send its own values. Prevents repetition loops on prone models and gives reasoning models a default thinking budget for simple clients.

### Security
- **Qdrant path traversal protection** — `/qdrant/*` paths are normalized with `path.Clean()` and `..` sequences are rejected, preventing directory traversal attacks against the Qdrant backend.
- **Deduplicated constant-time key comparison** — extracted shared `constantTimeKeyMatch()` helper used by both model API key and app key authentication. Removed unused `FindAppKey()` from config that used non-constant-time comparison.

### Fixed
- **Context windows not refreshed on hot reload** — context window auto-detection now re-runs after config reload, so adding or changing models updates the status page without a restart.
- **Inconsistent config hot reload** — file watcher now monitors the parent directory instead of the config file directly. Editors that save via rename (vim, etc.) no longer cause the watcher to lose track of the file.
- **Content-Length mismatch on proxied responses** — removed forwarding of backend `Content-Length` header since the proxy may re-marshal the body (think tag filtering, search loops), changing its size. Go's HTTP server now sets the correct length automatically.

## v0.3.5

### Added
- **`/v1/rerank` endpoint** — added reranking to the allowed proxy paths for backends that support it.

### Fixed
- **Context window detection using training size instead of runtime** — proxy now queries the llama.cpp `/props` endpoint first for the actual runtime `n_ctx` (respects `--ctx-size`), falling back to `/models` for other backends. Previously reported the training context size which could be much larger than allocated.

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
