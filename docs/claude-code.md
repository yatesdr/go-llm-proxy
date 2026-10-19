# Claude Code

Connect [Claude Code](https://claude.ai/download) to go-llm-proxy to use your self-hosted or third-party models as the backend. The proxy automatically translates between the Anthropic Messages API (which Claude Code speaks) and Chat Completions (which most local models speak).

## Quick start

The easiest path is the built-in config generator (`--serve-config-generator`). Select **Claude Code** from the dropdown, choose your models for Sonnet/Opus/Haiku slots, and generate a `settings.json` or start script.

## How it works

Claude Code uses the Anthropic Messages API exclusively. When you point it at the proxy:

- **Anthropic backends** (`type: anthropic`): requests pass through natively — full fidelity, including extended thinking with real signatures
- **OpenAI-compatible backends** (vLLM, llama-server, etc.): the proxy automatically translates Anthropic Messages → Chat Completions, and translates the response back. No configuration needed — it detects the backend type from your model config.

The translation handles:
- Text content and streaming (SSE event format translation)
- Tool calling round-trips (tool_use ↔ tool_calls, tool_result ↔ role:tool)
- Reasoning tokens → thinking blocks (models like MiniMax emit reasoning that appears as thinking in Claude Code)
- System prompts, stop sequences, temperature, max tokens
- Errors wrapped in Anthropic format (see [Errors](#errors-and-retries))
- Documents (PDF, text, content and URL sources), images and documents inside tool results
- Structured outputs (`output_config.format` json_schema becomes `response_format`) and `output_config.effort` (becomes `reasoning_effort`)
- Tool turns in the order Chat Completions templates expect (tool messages first, then user text)

### `messages_mode`

Control the translation behavior per model:

| Value | Behavior |
|---|---|
| `auto` | Default. Anthropic backends passthrough, others translate automatically |
| `native` | Force passthrough (backend must speak Anthropic protocol) |
| `translate` | Force translation to Chat Completions |

Most users don't need to set this — `auto` handles everything correctly.

### Auto mode and side queries

Claude Code's auto-mode permission classifier and its side queries are small, non-streaming requests that expect an answer immediately: `max_tokens: 64`, `thinking: {type: "disabled"}`, `temperature: 1` and `stop_sequences: ["</block>"]`, with the reply starting `<block>yes` or `<block>no`. A reasoning model that thinks first would spend all 64 tokens on reasoning and return nothing, so the proxy lets you say how to switch thinking off for each model with `thinking_off`. It is a raw JSON object merged into the Chat Completions request whenever the client sends `thinking.type: "disabled"`:

Test the setting against your server before relying on it: send the classifier-shaped request above with and without `thinking_off` and check the reply text starts `<block>`. Templates differ. On a GLM-5.3 vLLM build with a reasoning parser, for example, `enable_thinking: false` made the model put its reasoning into the answer (no opening tag), while the default, or a lower `reasoning_effort`, kept reasoning in its own field and returned a bare `<block>no` in under 20 tokens.

```yaml
models:
  # GLM-5.3 behind vLLM's reasoning parser: keep thinking, just make it short
  - name: glm-5.3
    backend: http://backend.example:8000/v1
    thinking_off:
      chat_template_kwargs:
        reasoning_effort: low

  # Qwen3-style chat templates that honour enable_thinking
  - name: qwen3
    backend: http://backend.example:8000/v1
    thinking_off:
      chat_template_kwargs:
        enable_thinking: false

  # DeepSeek-style servers that take a top-level flag
  - name: deepseek-v4
    backend: http://backend.example:8000/v1
    thinking_off:
      thinking: false
      chat_template_kwargs:
        thinking: false
```

Without `thinking_off` the disabled-thinking hint is simply dropped. A configured `defaults.reasoning_effort` is not applied to a request that disabled thinking when `thinking_off` is set. When thinking is not disabled, `output_config.effort` (`low`, `medium`, `high`, `xhigh`; `max` is sent as `xhigh`) is forwarded as `reasoning_effort`; `thinking.budget_tokens` has no portable equivalent and is ignored.

Other fields Claude Code sends are mapped as follows: `stop_sequences` becomes `stop`; `output_config.format` / `output_format` of type `json_schema` becomes `response_format: {type: "json_schema", json_schema: {name, schema, strict: true}}`; `tool_choice.disable_parallel_tool_use` becomes `parallel_tool_calls: false`; the model's `defaults:` (temperature, top_p, ...) fill in anything the client did not send; and when the model's `context_window` is known, `max_tokens` is lowered so prompt plus completion fit.

For a reliable score rather than a short generated reply, enable the decision head described under [Auto mode](#auto-mode).

### Documents, images and tool results

- A base64 PDF `document` block is converted to text by the pipeline for every model, including vision-capable ones (no Chat Completions backend accepts a raw PDF part). Set `rewrite_documents: off` on a model to skip this; leftover PDFs are then sent as OpenAI `file` parts (`file_data` data URL plus `filename`).
- `text` documents are inlined (with title and context as a heading), `content` documents have their text/image blocks inlined, and `url` documents are fetched by the proxy through its SSRF-safe client (32 MB limit, no private addresses, no redirects). `citations` and `cache_control` are ignored.
- Images and documents inside a `tool_result` cannot travel in a `role: tool` message, so they are placed in a user message directly after the tool messages, labelled with the originating tool use id. A tool error (`is_error`) is prefixed with `[tool error]`.
- Earlier assistant `thinking` blocks are replayed to the backend as `reasoning_content` (and `reasoning`) so reasoning templates keep their chain of thought.

### Errors and retries

Backend failures are returned as Anthropic errors with the status and `error.type` the client acts on:

| Backend status | Client sees |
|---|---|
| 400 / 422 | 400 `invalid_request_error` |
| 401 / 403 | 502 `api_error` (the proxy's credentials failed, not the client's) |
| 404 | 404 `not_found_error` |
| 413 | 413 `request_too_large` |
| 429 | 429 `rate_limit_error` |
| 500 | 500 `api_error` |
| 503 / 529 | same status, `overloaded_error` |

The backend's own message is passed through with URLs, addresses and credentials removed. vLLM, SGLang and llama.cpp context-length errors are rewritten to `prompt is too long: N tokens > M maximum` (or ``input length and `max_tokens` exceed context limit: N + MAX > M`` when only the completion overflows), which makes Claude Code compact the conversation or shrink `max_tokens` instead of failing. `Retry-After`, `Retry-After-Ms`, `X-Should-Retry` and `anthropic-ratelimit-*` headers are forwarded. For streaming requests the real HTTP status is returned whenever nothing has been streamed yet.

Once streaming starts the proxy sends `message_start` and a `ping` straight away and a `ping` every 15 seconds while the backend is quiet (long prompt processing). A stream that ends without a `finish_reason`, or with a read error, ends with an `overloaded_error` event rather than a normal `end_turn`, so the client retries instead of accepting a half-written answer or tool call.

## Configuration file

Save as `~/.claude/settings.json`:

```json
{
  "attribution": { "commit": "", "pr": "" },
  "env": {
    "ANTHROPIC_BASE_URL": "https://your-proxy.example.com",
    "ANTHROPIC_API_KEY": "your-proxy-api-key",
    "ANTHROPIC_DEFAULT_SONNET_MODEL": "MiniMax-M2.5",
    "ANTHROPIC_DEFAULT_SONNET_MODEL_NAME": "MiniMax M2.5",
    "ANTHROPIC_DEFAULT_SONNET_MODEL_SUPPORTED_CAPABILITIES": "thinking,interleaved_thinking",
    "ANTHROPIC_DEFAULT_OPUS_MODEL": "qwen-3.5",
    "ANTHROPIC_DEFAULT_OPUS_MODEL_NAME": "Qwen 3.5",
    "ANTHROPIC_DEFAULT_OPUS_MODEL_SUPPORTED_CAPABILITIES": "thinking,interleaved_thinking",
    "ANTHROPIC_DEFAULT_HAIKU_MODEL": "MiniMax-M2.5",
    "ANTHROPIC_DEFAULT_HAIKU_MODEL_NAME": "MiniMax M2.5",
    "ANTHROPIC_DEFAULT_HAIKU_MODEL_SUPPORTED_CAPABILITIES": "",
    "DISABLE_PROMPT_CACHING": "1",
    "CLAUDE_CODE_DISABLE_1M_CONTEXT": "1",
    "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
    "API_TIMEOUT_MS": "900000"
  }
}
```

## Start script (alternative)

Instead of editing `settings.json`, use a start script that sets environment variables and launches Claude Code:

```bash
#!/usr/bin/env bash
exec env \
  ANTHROPIC_BASE_URL="https://your-proxy.example.com" \
  ANTHROPIC_API_KEY="your-proxy-api-key" \
  ANTHROPIC_DEFAULT_SONNET_MODEL="MiniMax-M2.5" \
  ANTHROPIC_DEFAULT_SONNET_MODEL_NAME="MiniMax M2.5" \
  ANTHROPIC_DEFAULT_SONNET_MODEL_SUPPORTED_CAPABILITIES="thinking,interleaved_thinking" \
  ANTHROPIC_DEFAULT_OPUS_MODEL="qwen-3.5" \
  ANTHROPIC_DEFAULT_OPUS_MODEL_NAME="Qwen 3.5" \
  ANTHROPIC_DEFAULT_OPUS_MODEL_SUPPORTED_CAPABILITIES="thinking,interleaved_thinking" \
  ANTHROPIC_DEFAULT_HAIKU_MODEL="MiniMax-M2.5" \
  ANTHROPIC_DEFAULT_HAIKU_MODEL_NAME="MiniMax M2.5" \
  ANTHROPIC_DEFAULT_HAIKU_MODEL_SUPPORTED_CAPABILITIES="" \
  DISABLE_PROMPT_CACHING="1" \
  CLAUDE_CODE_DISABLE_1M_CONTEXT="1" \
  CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC="1" \
  API_TIMEOUT_MS="900000" \
  claude --settings '{"attribution":{"commit":"","pr":""}}' "$@"
```

Save as `claude-proxy.sh`, make executable (`chmod +x`), and run.

## Auto mode

`claude --permission-mode auto` has Claude Code review every action that is not read-only or a working-directory edit (shell commands, network access, subagent spawns) with a classifier. Behind a gateway, Claude Code first asks for server-side review (a `safeguards` field in the request body). The proxy does not return `safeguard_results`, so Claude Code falls back to sending the classifier request itself and shows a one-time notice that holds the first action.

The classifier request is a separate, non-streaming `POST /v1/messages`:

- `model` is `claude-sonnet-5` first (the default classifier model), then the session model if that fails. No alias is needed: when `auto_mode` is configured the proxy answers these requests whatever model name they carry.
- `system` is a long security-monitor prompt containing the sentence "You are a security monitor for autonomous AI coding agents" and an output-format section; `messages` holds one user message with the transcript and a stage suffix.
- **Stage 1** carries `stop_sequences: ["</severity>"]` and expects `<severity>N` (N from 0 to 100, 50 is the allow/block boundary). It runs for every reviewed action. **Stage 2** has no stop sequence, runs only after a stage 1 score above 50, and expects `<severity>N</severity>` plus `<category>Rule Name</category>` when blocking.
- Older builds use `stop_sequences: ["</block>"]` and expect `<block>yes` or `<block>no`.

### How the proxy answers

With an `auto_mode` block configured, a request is treated as a classifier request when its system text contains the sentence above **and** either its `stop_sequences` include `</severity>` or `</block>`, or the system text contains `<severity>N</severity>` or `<block>`. Anything else (ordinary turns, a prompt that only mentions a security monitor) is handled normally. The format is severity unless only the block markers are present; the stage is 1 when `stop_sequences` is non-empty and 2 otherwise.

The proxy then makes one Chat Completions call to the decision model: `max_tokens: 1`, `temperature: 0`, `logprobs: true`, `top_logprobs: 20`, `continue_final_message: true`, `add_generation_prompt: false`, with the system text, the user message and a final assistant message of `<severity>` (or `<block>`). GLM-5.3 tokenises each score 0 to 100 as one token, so the first token's `top_logprobs` is the score distribution. The proxy keeps the alternatives that are integers 0 to 100 (spaces trimmed), normalises them, and computes `p_block` (mass above 50) and the mean `E`. The reply number is `round(E)`, forced to the side `p_block` chooses (at least 51 when `p_block` is at or above `block_probability`, at most 49 otherwise), so it is never exactly 50. The generated text is never read: reasoning parsers may put it in `message.reasoning`.

For a stage 2 block, a second short call (prefill `<severity>N</severity><category>`, `max_tokens: 16`, `stop: ["</category>"]`) fetches the rule name; if it fails the category is left out. In the block format the tokens `yes` and `no` are scored the same way.

The reply is a normal Messages response (`model` is the requested name, usage comes from the upstream calls): `<severity>N` with `stop_reason: "stop_sequence"` for stage 1, `<severity>N</severity>` with `stop_reason: "end_turn"` for stage 2, `<block>yes` / `<block>no` for the older format. `stream: true` gets the same text as a minimal event sequence.

If the decision call fails, returns no logprobs, or the numeric tokens hold less than half the probability mass, the request is not failed: it continues through the ordinary translated path with the model swapped to the decision model, so the reply is still a valid classifier answer. Each decision writes one log line (stage, format, `p_block`, severity, model, latency, fallback reason); prompt text is never logged.

Interception applies to `/v1/messages` only, after authentication and rate limiting. The key must be allowed the `auto_mode.model` (see `keys[].models`); a key that is not gets the normal behaviour for the model it asked for. Usage is recorded under the decision model, with the requested name as `alias_from`. The decision model's backend may be a `connect://` URL. `/anthropic/v1/messages` is not intercepted.

### Configuration

```yaml
auto_mode:
  model: GLM-5.3-Flash      # a configured model (or alias); OpenAI-compatible backend
  block_probability: 0.5    # optional; p_block at or above this blocks
```

The model's backend must support `logprobs` and `continue_final_message` (vLLM and SGLang do; servers that ignore either field fall back to the generative path on every request). The same setting is on the admin page under Chat, Model Helpers, "Claude Code auto mode".

### Client settings

```bash
export ANTHROPIC_BASE_URL=http://your-proxy:8080
export ANTHROPIC_AUTH_TOKEN=your-key
export CLAUDE_CODE_AUTO_MODE_SERVER=0   # skip the server-side review probe and its one-time notice
claude --permission-mode auto
```

Setting `CLAUDE_CODE_AUTO_MODE_SERVER=0` is recommended: Claude Code goes straight to its own classifier requests, which the proxy answers. Three blocks in a row, or twenty in total, pause auto mode and Claude Code prompts again. A reply it cannot parse is treated as a block.

## Key settings

| Variable | Purpose |
|---|---|
| `ANTHROPIC_BASE_URL` | Your proxy URL (without `/v1` — Claude Code adds it) |
| `ANTHROPIC_API_KEY` | Your proxy API key |
| `ANTHROPIC_DEFAULT_SONNET_MODEL` | Model for the Sonnet slot (default/primary model) |
| `ANTHROPIC_DEFAULT_OPUS_MODEL` | Model for the Opus slot (large/complex tasks) |
| `ANTHROPIC_DEFAULT_HAIKU_MODEL` | Model for the Haiku slot (fast/simple tasks) |
| `*_SUPPORTED_CAPABILITIES` | `"thinking,interleaved_thinking"` to enable extended thinking display |
| `DISABLE_PROMPT_CACHING` | Set to `"1"` for non-Anthropic backends |
| `CLAUDE_CODE_DISABLE_1M_CONTEXT` | Set to `"1"` to avoid 1M context requests |
| `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC` | Set to `"1"` to reduce extraneous API calls |
| `API_TIMEOUT_MS` | Request timeout (default 900000 = 15 minutes) |

## Model selection

Claude Code appends suffixes such as `[1m]` to model IDs and uses its own names (`claude-haiku-4-5-20251001`). Besides exact `aliases:` entries, the proxy accepts glob patterns and strips a trailing `[...]` suffix before matching:

```yaml
aliases:
  claude-haiku-*: qwen-3.5-9b     # any Haiku ID
  claude-*: glm-5.3               # everything else starting with claude-
```

Matching order: real model name, exact alias, then (with the `[...]` suffix removed) the same two, then globs. `*` matches any run of characters and `?` one character; with several matching globs the most specific (longest literal text) wins. Glob aliases are not listed by `GET /v1/models`.

Claude Code has three model slots. Each can be mapped to any model in your proxy:

- **Sonnet** — the default model used for most tasks
- **Opus** — used for complex reasoning (selected with `/model opus`)
- **Haiku** — used for fast, simple tasks (selected with `/model haiku`)

All three can point to the same model if you only have one.

## Thinking / reasoning support

For **translated backends** (non-Anthropic): if the model emits reasoning tokens (like MiniMax-M2.5), the proxy converts them to Anthropic thinking blocks that appear in Claude Code's output. These use placeholder signatures — Claude Code stores them and passes them back, but they never reach a real Anthropic API for validation. On subsequent turns, the proxy replays thinking blocks to the Chat Completions backend as `reasoning_content`.

Set `*_SUPPORTED_CAPABILITIES` to `"thinking,interleaved_thinking"` so Claude Code displays the thinking content. Leave empty for models that don't emit reasoning tokens.

For **native Anthropic backends**: real extended thinking with cryptographic signatures works normally through passthrough.

## Web search

Claude Code's built-in `WebSearch` tool (`web_search_20250305`) is an Anthropic server-side feature. It works with native Anthropic backends through passthrough.

For translated backends, the proxy can handle web search transparently using the processing pipeline:

**Option 1: Proxy-side search (recommended)** — Configure a Tavily API key in the proxy's `processors` block:

```yaml
processors:
  web_search_key: tvly-your-key
```

The proxy automatically converts Claude Code's `web_search_20250305` server tool to a function tool that the backend model can call. When the model calls `web_search`, the proxy executes the Tavily search and injects the results — transparent to Claude Code. No client-side MCP configuration needed.

**Option 2: Client-side MCP** — Configure [Tavily](https://tavily.com/) as an MCP server in Claude Code's settings. The config generator can set this up — enter your Tavily API key and the generated config will include the MCP setup command.

## Image handling

The proxy's processing pipeline can handle images for text-only backends:

**Vision-capable backends** (`supports_vision: true`): Images pass through the translation normally.

**Text-only backends with a vision processor configured**: The proxy sends each image to the vision model for description, then replaces the image with the text description. The backend model receives only text. Configure this in the proxy:

```yaml
processors:
  vision: qwen-3.5    # any vision-capable model in your config

models:
  - name: MiniMax-M2.5
    backend: http://192.168.100.10:8000/v1
    # Images auto-routed to qwen-3.5 for description

  - name: qwen-3.5
    backend: http://192.168.13.30:8000/v1
    supports_vision: true    # handles images natively, no processing needed
```

**Text-only backends without a vision processor**: The proxy returns a clear error message: *"The backend model does not appear to support image inputs."* with configuration guidance.

## Proxy-side config

On the proxy side, no special model configuration is needed. Any model in your `config.yaml` is automatically available to Claude Code. The proxy detects whether the backend speaks Anthropic or OpenAI protocol and translates accordingly.

```yaml
models:
  # OpenAI backend — proxy translates Messages → Chat Completions automatically
  - name: MiniMax-M2.5
    backend: http://192.168.100.10:8000/v1

  # Anthropic backend — proxy passes through natively
  - name: claude-sonnet-4
    backend: https://api.anthropic.com
    type: anthropic
    api_key: sk-ant-...
```

## Known limitations (translated backends)

- **Extended thinking**: Reasoning tokens from the backend are displayed as thinking blocks, but they don't have real Anthropic signatures. This is cosmetic — tool calling and agentic behavior work normally.
- **Prompt caching**: Stripped silently. All translated requests are uncached.
- **Server-side web search**: Not available directly, but the proxy can execute web searches via Tavily when `web_search_key` is configured in the `processors` block. Alternatively, use Tavily MCP.
- **Image support**: Text-only models work with images when a vision processor is configured. Otherwise, the proxy returns a clear error with configuration guidance.
- **PDF support**: The proxy extracts text from PDFs for every translated model (vision fallback for scanned PDFs when a vision processor is configured).

Native Anthropic backends have full fidelity — all features work through passthrough. Use `force_pipeline: true` on an Anthropic model to override and use proxy-side processing instead.
