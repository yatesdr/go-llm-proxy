# Qwen Code

Connect [Qwen Code](https://github.com/QwenLM/qwen-code) to go-llm-proxy to use your self-hosted or third-party models.

## Quick start

Use the built-in config generator (`--serve-config-generator`). Select **Qwen Code** from the dropdown, choose a default model and any additional models, and generate a `settings.json` config file.

## How it works

Qwen Code supports multiple model providers with both OpenAI and Anthropic protocols. The proxy is configured as a provider with per-model entries. Qwen Code's `/model` command lets users switch between available models at runtime.

## Configuration file

Save as `~/.qwen/settings.json`:

```json
{
  "$version": 3,
  "model": { "name": "MiniMax-M2.5" },
  "security": { "auth": { "selectedType": "openai" } },
  "modelProviders": {
    "openai": [
      {
        "id": "MiniMax-M2.5",
        "name": "MiniMax M2.5",
        "envKey": "PROXY_API_KEY",
        "baseUrl": "https://your-proxy.example.com/v1",
        "generationConfig": { "timeout": 300000, "maxRetries": 1 }
      },
      {
        "id": "qwen-3.5",
        "name": "Qwen 3.5",
        "envKey": "PROXY_API_KEY",
        "baseUrl": "https://your-proxy.example.com/v1",
        "generationConfig": { "timeout": 300000, "maxRetries": 1 }
      }
    ],
    "anthropic": [
      {
        "id": "claude-sonnet-4-20250514",
        "name": "Claude Sonnet 4",
        "envKey": "PROXY_API_KEY",
        "baseUrl": "https://your-proxy.example.com/anthropic",
        "generationConfig": { "timeout": 300000, "maxRetries": 1 }
      }
    ]
  },
  "env": {
    "PROXY_API_KEY": "your-proxy-api-key"
  }
}
```

## Key settings

| Field | Purpose |
|---|---|
| `model.name` | Default model on startup |
| `security.auth.selectedType` | Auth protocol for the default model (`"openai"` or `"anthropic"`) |
| `modelProviders.openai[]` | Models using OpenAI protocol (base URL ends with `/v1`) |
| `modelProviders.anthropic[]` | Models using Anthropic protocol (base URL ends with `/anthropic`) |
| `env.PROXY_API_KEY` | Your proxy API key (referenced by `envKey` on each model) |

## Dual-protocol support

Models are grouped by protocol:

- **OpenAI models** go in `modelProviders.openai` with `baseUrl` pointing to `/v1`
- **Anthropic models** go in `modelProviders.anthropic` with `baseUrl` pointing to `/anthropic`

The config generator handles this automatically based on each model's protocol.

## Switching models

After launching Qwen Code, use the `/model` command to switch between configured models at runtime. All models listed in `modelProviders` are available.

## Web search

The config generator optionally includes [Tavily](https://tavily.com/) web search:

```json
{
  "webSearch": {
    "provider": [{ "type": "tavily", "apiKey": "tvly-your-key" }],
    "default": "tavily"
  }
}
```

## Installation

### macOS / Linux

```bash
mkdir -p ~/.qwen
# Save generated settings.json to ~/.qwen/settings.json
```

### Windows

```
mkdir %USERPROFILE%\.qwen
REM Save generated settings.json to %USERPROFILE%\.qwen\settings.json
```
