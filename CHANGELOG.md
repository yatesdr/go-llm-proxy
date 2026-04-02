# Changelog

All notable changes to this project will be documented in this file.

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
- Documentation: `doc/codex.md` (Codex setup, Responses API details), `doc/usage.md` (usage logging and dashboard)

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

## v1.0.0

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
