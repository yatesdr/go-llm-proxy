# Claude Code

Connect [Claude Code](https://claude.ai/download) to go-llm-proxy to use your self-hosted or third-party models as the backend.

## Quick start

The easiest path is the built-in config generator (`--serve-config-generator`). Select **Claude Code** from the dropdown, choose your models for Sonnet/Opus/Haiku slots, and generate a `settings.json` or start script.

## How it works

Claude Code uses the Anthropic API protocol. The proxy accepts requests at `/v1/messages` (or `/anthropic/v1/messages`) and routes them to the configured backend based on model name. Environment variables tell Claude Code to use the proxy instead of Anthropic's API directly.

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

Instead of editing `settings.json`, you can use a start script that sets environment variables and launches Claude Code:

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
| `*_SUPPORTED_CAPABILITIES` | `"thinking,interleaved_thinking"` to enable extended thinking |
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

## Thinking support

Set `*_SUPPORTED_CAPABILITIES` to `"thinking,interleaved_thinking"` for models that support extended thinking (reasoning). Leave empty for models that don't. The config generator UI has checkboxes for this per slot.

## Web search

The config generator can optionally configure [Tavily](https://tavily.com/) as an MCP server for web search. Enter your Tavily API key and the generated config will include the MCP setup command.

## Proxy-side config

On the proxy side, Claude Code uses the Anthropic Messages API. Models must be configured with `type: anthropic` if the backend is Anthropic-compatible, or left as default (OpenAI) if the backend speaks Chat Completions.

The proxy handles the routing transparently — Claude Code doesn't need to know which protocol the backend uses.
