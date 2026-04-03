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
- Errors wrapped in Anthropic format

### `messages_mode`

Control the translation behavior per model:

| Value | Behavior |
|---|---|
| `auto` | Default. Anthropic backends passthrough, others translate automatically |
| `native` | Force passthrough (backend must speak Anthropic protocol) |
| `translate` | Force translation to Chat Completions |

Most users don't need to set this — `auto` handles everything correctly.

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

Claude Code has three model slots. Each can be mapped to any model in your proxy:

- **Sonnet** — the default model used for most tasks
- **Opus** — used for complex reasoning (selected with `/model opus`)
- **Haiku** — used for fast, simple tasks (selected with `/model haiku`)

All three can point to the same model if you only have one.

## Thinking / reasoning support

For **translated backends** (non-Anthropic): if the model emits reasoning tokens (like MiniMax-M2.5), the proxy converts them to Anthropic thinking blocks that appear in Claude Code's output. These use placeholder signatures — Claude Code stores them and passes them back, but they never reach a real Anthropic API for validation. On subsequent turns, the proxy strips thinking blocks before sending to the Chat Completions backend.

Set `*_SUPPORTED_CAPABILITIES` to `"thinking,interleaved_thinking"` so Claude Code displays the thinking content. Leave empty for models that don't emit reasoning tokens.

For **native Anthropic backends**: real extended thinking with cryptographic signatures works normally through passthrough.

## Web search

Claude Code's built-in `WebSearch` tool is an Anthropic server-side feature. It works with native Anthropic backends but not through translation (the search executes on Anthropic's infrastructure).

For translated backends, configure [Tavily](https://tavily.com/) as an MCP server for web search. The config generator can set this up — enter your Tavily API key and the generated config will include the MCP setup command.

## Image handling

If the backend model supports vision, images pass through the translation normally. If the backend is text-only and rejects the request, the proxy returns a clear error message: *"The backend model does not appear to support image inputs."*

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
- **Server-side web search**: Not available. Use Tavily MCP as an alternative.
- **Image support**: Depends on the backend model. Text-only models will error with a clear message.

Native Anthropic backends have full fidelity — all features work through passthrough.
