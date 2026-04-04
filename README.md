# go-llm-proxy

A single-binary LLM proxy targeting compatibility and native behaviros with common coding agents such as Claude Codex, Codex, OpenCode, Qwen, and Claw.    It proxies as you'd expect a proxy to for local models and upstream providers, ranslates between API protocols, routes requests across backends, and runs upstream tools locally to enable commonly used features for local back-ends like llama-server and vllm.  Adds web search (via Tavily key), and routes vision requests to a specified vision model to give you full capability in your harness tooling.

## Common Use Cases
You need data security and self-host models or have upstream secure vendors (Azure, Bedrock, etc) that don't have all the expected supported tooling you're used to.   You want to use glm-5.1 for planning and MiniMax-M2.5 for implementation and agent work, with qwen-3.5 as your vision processor;  you want to connect using claude code and codex and have it 'just work'.  You upload a pdf and it works, you upload an image and that works too.  Call for a web search?  The proxy intercepts natively and sends it through tavily.

## What it does

- **Protocol translation** — Claude Code speaks Anthropic Messages. Codex speaks OpenAI Responses. Your vLLM speaks Chat Completions. The proxy translates between them automatically.
- **Model multiplexing** — Aggregate local GPU servers, cloud APIs, and third-party providers behind one endpoint. Clients see one model list.
- **API key management** — Issue proxy keys with per-key model restrictions. Backend credentials stay on the server.
- **Vision pipeline** — Images sent to text-only models are described by a vision-capable model and replaced with text. Transparent to the client.
- **Web search** — When coding assistants request web search, the proxy executes it via Tavily and injects the results. No client-side MCP setup needed.
- **Usage monitoring** — Per-request logging to SQLite. Web dashboard, CLI reports, per-user/model breakdowns.
- **Config generator** — Built-in web UI creates ready-to-use configs for Claude Code, Codex, OpenCode, and Qwen Code.

## Quick start

```bash
cp config.yaml.example config.yaml
# edit config.yaml — add your models and keys
docker compose -f docker/docker-compose.yml up -d
```

Or without Docker:

```bash
./go-llm-proxy -config config.yaml
```

## Minimum config

```yaml
listen: ":8080"

models:
  - name: my-model
    backend: http://192.168.1.10:8000/v1

keys:
  - key: sk-your-secret-key
    name: admin
```

See [config.yaml.example](config.yaml.example) for a fully annotated starter config with all options.

## Coding assistant support

|  | Claude Code | Codex CLI | OpenCode | Qwen Code |
|---|:---:|:---:|:---:|:---:|
| Text + streaming | Yes | Yes | Yes | Yes |
| Tool calling | Yes | Yes | Yes | Yes |
| Reasoning display | Yes | Yes | — | — |
| Web search (proxy) | Yes | Yes | — | — |
| Image processing | Yes | Yes | Yes | Yes |
| Config generator | Yes | Yes | Yes | Yes |

Each assistant speaks a different API protocol. The proxy detects this and translates automatically — no per-model configuration needed for the common case.

## Processing pipeline

Optional. Handles content that local backends don't support natively:

```yaml
processors:
  vision: qwen-3.5              # vision model for image descriptions
  ocr: minicpm-ocr              # fast model for PDF page text extraction (optional, falls back to vision)
  web_search_key: tvly-...      # Tavily API key for web search
```

Without `processors`, the proxy just translates and routes. With it, images, PDFs, and search work on text-only backends.

## Documentation

| Topic | Link |
|-------|------|
| Configuration reference | [docs/config-reference.md](docs/config-reference.md) |
| Claude Code | [docs/claude-code.md](docs/claude-code.md) |
| Codex CLI | [docs/codex.md](docs/codex.md) |
| OpenCode | [docs/opencode.md](docs/opencode.md) |
| Qwen Code | [docs/qwen-code.md](docs/qwen-code.md) |
| Docker deployment | [docs/docker.md](docs/docker.md) |
| Production deployment | [docs/deployment.md](docs/deployment.md) |
| Usage monitoring | [docs/usage.md](docs/usage.md) |
| Security | [docs/security.md](docs/security.md) |
