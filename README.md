# go-llm-proxy

A single-binary LLM proxy that connects coding assistants and AI agents to local and upstream models. Translates between API protocols, routes requests across backends, and adds tools that local backends lack — web search, image description, PDF text extraction, and OCR. Works with Claude Code, Codex, OpenCode, Qwen Code, OpenClaw, and any OpenAI/Anthropic-compatible client.

**[Landing page](https://go-llm-proxy.com)** · **[Config generator](https://go-llm-proxy.com/configure.html)** · **[Releases](https://github.com/yatesdr/go-llm-proxy/releases)**

## Common use cases

You need data security and self-host models or have upstream secure vendors (Azure, Bedrock, etc) that don't have all the expected tooling you're used to. You want to use glm-5.1 for planning and MiniMax-M2.5 for implementation and agent work, with Qwen3-VL-8B as your vision processor. You want to connect using Claude Code and Codex and have it 'just work'. You upload a PDF and it works, you upload an image and that works too. Call for a web search? The proxy intercepts natively and sends it through Tavily or Brave.

## What it does

- **Protocol translation** — Claude Code speaks Anthropic Messages. Codex speaks OpenAI Responses. Your vLLM speaks Chat Completions. Anthropic-compatible providers speak Messages. AWS Bedrock speaks Converse with SigV4. The proxy translates between them automatically in either direction.
- **Model multiplexing** — Aggregate local GPU servers, cloud APIs, and third-party providers behind one endpoint. Clients see one model list.
- **API key management** — Issue proxy keys with per-key model restrictions. Backend credentials stay on the server.
- **Vision pipeline** — Images sent to text-only models are described by a vision-capable model and replaced with text. Transparent to the client.
- **Document processing** — Text extraction for native PDFs plus an official PaddleOCR `/layout-parsing` route for scanned and layout-heavy documents. The original PDF goes to the configured layout service; vision remains a fallback.
- **Audio routing** — Dedicated, load-balanced Whisper transcription/translation and text-to-speech routes, including TTS voice discovery for compatible backends such as Kokoro-FastAPI.
- **Web search** — When coding assistants request web search, the proxy executes it via Tavily or Brave Search (auto-detected from key prefix) and injects the results. No client-side MCP setup needed.
- **MCP endpoint** — `/mcp/sse` exposes web search for OpenCode, Qwen Code, and any MCP-compatible agent.
- **Qdrant proxy** — `/qdrant/*` proxies to a Qdrant vector database with separate app key auth and automatic multi-tenant isolation.
- **Usage monitoring** — Per-request logging to SQLite. Web dashboard, CLI reports, per-user/model breakdowns.
- **Config generator** — Built-in web UI creates ready-to-use configs for Claude Code, Codex, OpenCode, and Qwen Code. Also available [standalone](https://go-llm-proxy.com/configure.html).
- **Context window detection** — Auto-queries backends at startup. Manual override per model.
- **Hot reload** — Config reloads on file save or SIGHUP. Add models or rotate keys without restarting.
- **Security** — Constant-time auth, IP rate limiting, SSRF protection, sanitized error responses, path allowlisting.

## Quick start

```bash
./go-llm-proxy -config config.yaml
```

Or with Docker:

```bash
mkdir -p config
cp config.yaml.example config/config.yaml
cp docker/.env.example docker/.env
# edit docker/.env — set GO_LLM_ADMIN_PASSWORD
docker compose -f docker/docker-compose.yml up -d
```

Boots with a placeholder model/key so `docker compose ps` shows healthy
immediately, and the admin console at `/admin` is already usable with the
password from `.env` — no YAML editing required to log in and replace the
placeholders through the UI. Full walkthrough (first login, enabling the
usage dashboard, telemetry retention): [docs/docker.md](docs/docker.md).

## Minimum config

```yaml
listen: ":8080"

models:
  - name: my-model
    backends:
      - url: http://192.168.1.10:8000/v1

keys:
  - key: sk-your-secret-key
    name: admin
```

See [config.yaml.example](config.yaml.example) for a fully annotated starter config with all options.

## Compatibility matrix

What works with each coding assistant through the proxy.

**Protocol**

|  | Claude Code | Codex CLI | OpenCode | Qwen Code |
|---|:---:|:---:|:---:|:---:|
| Native API | Anthropic Messages | OpenAI Responses | Chat Completions | Chat Completions |
| Translation | auto-translated | auto-translated | passthrough | passthrough |

**Core features**

|  | Claude Code | Codex CLI | OpenCode | Qwen Code |
|---|:---:|:---:|:---:|:---:|
| Text + streaming | ✓ | ✓ | ✓ | ✓ |
| Tool calling | ✓ | ✓ | ✓ | ✓ |
| Multi-turn tool loops | ✓ | ✓ | ✓ | ✓ |
| Reasoning display | ✓ | ✓ | — | — |
| Extended thinking | ✓ | ✓ | — | — |

**Proxy-side processing** ([details](docs/pipeline.md))

|  | Claude Code | Codex CLI | OpenCode | Qwen Code |
|---|:---:|:---:|:---:|:---:|
| Web search (Tavily / Brave) | ✓ proxy | ✓ proxy | ✓ MCP | ✓ MCP |
| Image description | ✓ vision | ✓ vision | ✓ vision | ✓ vision |
| PDF text extraction | ✓ proxy | client-side | ✓ | ✓ |
| Scanned PDF / layout | ✓ PaddleOCR | ✓ PaddleOCR | ✓ | ✓ |
| Context compaction | — | ✓ | — | — |
| Usage logging & reports | ✓ | ✓ | ✓ | ✓ |

Each assistant speaks a different API protocol. The proxy detects this and translates automatically — no per-model configuration needed for the common case.

## Chat helpers and documents

Optional. Handles content that local backends don't support natively:

```yaml
processors:
  vision: Qwen3-VL-8B        # vision model for image descriptions
  web_search_key: tvly-...    # Tavily or Brave Search key (auto-detected from prefix)

documents:
  paddleocr:
    backends:
      - url: http://192.168.13.30:8002
```

Chat helpers are optional. The document service keeps PaddleOCR's native API
contract and is configured independently from OpenAI/Anthropic chat models.

### Recommended processor models

| Processor | Model | Notes |
|---|---|---|
| Vision | [Qwen3-VL-8B](https://huggingface.co/Qwen/Qwen3-VL-8B-Instruct) | Best quality/speed balance for image description. Handles charts, screenshots, diagrams. |
| Documents | [PaddleOCR-VL](https://www.paddleocr.ai/main/en/version3.x/pipeline_usage/PaddleOCR-VL.html) | Configure the full layout pipeline, not the element-level recognizer alone. |
| Web search | [Tavily](https://tavily.com) or [Brave Search](https://brave.com/search/api/) | Tavily free: 1,000 req/month. Brave free: $5/month credit. Auto-detected from key prefix. |

## Documentation

| Topic | Link |
|---|---|
| Configuration reference | [docs/config-reference.md](docs/config-reference.md) |
| Claude Code | [docs/claude-code.md](docs/claude-code.md) |
| Codex CLI | [docs/codex.md](docs/codex.md) |
| OpenCode | [docs/opencode.md](docs/opencode.md) |
| Qwen Code | [docs/qwen-code.md](docs/qwen-code.md) |
| Processing pipeline | [docs/pipeline.md](docs/pipeline.md) |
| Docker deployment | [docs/docker.md](docs/docker.md) |
| Production deployment | [docs/deployment.md](docs/deployment.md) |
| Usage monitoring | [docs/usage.md](docs/usage.md) |
| Qdrant proxy | [docs/qdrant.md](docs/qdrant.md) |
| Security | [docs/security.md](docs/security.md) |
