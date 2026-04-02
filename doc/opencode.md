# OpenCode

Connect [OpenCode](https://opencode.ai) to go-llm-proxy to use your self-hosted or third-party models.

## Quick start

Use the built-in config generator (`--serve-config-generator`). Select **OpenCode** from the dropdown, choose your Build and Plan agent models, and generate an `opencode.json` config file.

## How it works

OpenCode uses provider plugins (`@ai-sdk/openai-compatible` and `@ai-sdk/anthropic`). The proxy is configured as a custom provider with the appropriate base URL. OpenCode supports multiple providers simultaneously, so models using OpenAI and Anthropic protocols can coexist.

## Configuration file

Save as `opencode.json` in your project root, or globally at `~/.config/opencode/opencode.json`:

```json
{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "go-llm-proxy": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "go-llm-proxy (OpenAI)",
      "options": {
        "baseURL": "https://your-proxy.example.com/v1",
        "apiKey": "your-proxy-api-key"
      },
      "models": {
        "MiniMax-M2.5": { "name": "MiniMax M2.5" },
        "qwen-3.5": { "name": "Qwen 3.5" }
      }
    },
    "go-llm-proxy-ant": {
      "npm": "@ai-sdk/anthropic",
      "name": "go-llm-proxy (Anthropic)",
      "options": {
        "baseURL": "https://your-proxy.example.com/anthropic/v1",
        "apiKey": "your-proxy-api-key"
      },
      "models": {
        "claude-sonnet-4-20250514": { "name": "Claude Sonnet 4" }
      }
    }
  },
  "model": "go-llm-proxy/MiniMax-M2.5",
  "small_model": "go-llm-proxy/MiniMax-M2.5",
  "agent": {
    "build": {
      "model": "go-llm-proxy/MiniMax-M2.5",
      "description": "Coding agent"
    },
    "plan": {
      "model": "go-llm-proxy/qwen-3.5",
      "description": "Planning agent"
    }
  }
}
```

## Key settings

| Field | Purpose |
|---|---|
| `provider.*.options.baseURL` | Proxy URL — use `/v1` for OpenAI models, `/anthropic/v1` for Anthropic models |
| `provider.*.options.apiKey` | Your proxy API key |
| `model` | Default model (prefixed with provider name) |
| `agent.build.model` | Model for the coding agent |
| `agent.plan.model` | Model for the planning agent |

## Dual-protocol support

If your proxy has both OpenAI and Anthropic backends, the config generator creates two providers automatically:

- **`go-llm-proxy`** — for OpenAI-compatible models (uses `@ai-sdk/openai-compatible` with `/v1` base URL)
- **`go-llm-proxy-ant`** — for Anthropic-compatible models (uses `@ai-sdk/anthropic` with `/anthropic/v1` base URL)

Models are automatically sorted into the correct provider based on their protocol.

## Web search

The config generator optionally includes [Tavily](https://tavily.com/) as a remote MCP server:

```json
{
  "mcp": {
    "tavily": {
      "type": "remote",
      "url": "https://mcp.tavily.com/mcp",
      "headers": { "Authorization": "Bearer tvly-your-key" },
      "enabled": true
    }
  }
}
```

## Agent selection

OpenCode has two agent slots:

- **Build** — the coding agent, used for writing and editing code
- **Plan** — the planning agent, used for reasoning about architecture and approach

Both can point to the same model, or you can use a stronger model for planning and a faster one for coding.
