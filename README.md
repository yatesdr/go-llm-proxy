# go-llm-proxy

A lightweight, secure LLM API proxy that aggregates multiple backends (vLLM, llama-server, cloud APIs) behind a single endpoint. Supports both OpenAI and Anthropic API protocols.

This was built to proxy internally hosted models on a single endpoint, combine them with your subscription plans, and make it easy to select the models you want to use.   For example, if you're running a production model on VLLM and an embeddings model on llama-server, you can join them and proxy through go-llm-proxy to list both models on the same endpoint, then also link in your gpt or glm subs to allow for easily switching models and testing or serving them in production.

This use-case is also served by litellm[proxy], but this is a much lighter weight approach and much simpler to install and configure.  No database or dependencies - single binary and a YAML config file.

If serving publicly, it's probably best to put an NGINX reverse proxy ahead of this (see config examples later for this), and to make sure you select secure and lengthy API keys.   Basic security best practices are included for throttling and banning, but no guarantee is made as to suitability for any particular use.

## Features

- OpenAI and Anthropic API passthrough (completions, chat, embeddings, images, audio, messages)
- Anthropic Messages API support via `/v1/messages` and explicit `/anthropic/` route prefix
- Model name routing — clients request by name, proxy routes to the right backend
- Model name rewriting — expose friendly names while backends use internal identifiers
- API key authentication with per-key model access control (supports both `Authorization: Bearer` and `x-api-key` headers)
- IP-based rate limiting and throttling for failed auth attempts
- Streaming (SSE) support with proper flush handling
- Hot-reload config via `SIGHUP` — no downtime for model changes
- Graceful shutdown — active connections drain on restart
- Hardened for public internet exposure

## Quick Start

There are two easy deployment paths:

1. Docker. Often the easiest option for servers, especially if you already use Compose or container-based deployment.

```bash
cp config.yaml.example config.yaml
docker run --rm \
  -p 8080:8080 \
  -v $(pwd)/config.yaml:/config/config.yaml:ro \
  ghcr.io/yatesdr/go-llm-proxy:latest
```

2. Prebuilt binary. Download the archive for your platform from:

   https://github.com/yatesdr/go-llm-proxy/releases

   Then:

```bash
cp config.yaml.example config.yaml
./go-llm-proxy -config ./config.yaml
```

Docker images are published to GHCR on version tags and stable releases also update `:latest`.
The Docker image does not bake in a config file; mount your own `config.yaml` at `/config/config.yaml`.

## Configuration

Copy the example config and edit it:

```bash
cp config.yaml.example config.yaml
```

If you are deploying with Docker, mount that file into the container as `/config/config.yaml`.

### config.yaml

```yaml
listen: ":8080"

models:
  - name: MiniMax-M2.5
    backend: http://192.168.100.10:8000/v1
    api_key: your-backend-key
    timeout: 300

  - name: glm-4.5
    backend: https://api.z.ai/api/coding/paas/v4
    api_key: your-zhipu-key

  - name: nomic-embed
    backend: http://192.168.100.12:8002/v1
    model: nomic-embed-text-v1.5.Q8_0.gguf   # name sent to backend
    api_key: your-backend-key

  - name: Nemotron-3-Super
    backend: http://192.168.100.15:8003/v1
    api_key: anything
    timeout: 600

  - name: claude-sonnet-4-20250514
    backend: https://api.anthropic.com
    api_key: sk-ant-your-anthropic-key
    type: anthropic
    timeout: 300

keys:
  - key: sk-your-api-key-here
    name: admin
    models: []    # empty = access to all models

  - key: sk-restricted-key
    name: guest
    models:       # restricted to specific models
      - MiniMax-M2.5
      - nomic-embed
```

### Model fields

| Field | Required | Description |
|-------|----------|-------------|
| `name` | yes | Model name clients use in requests |
| `backend` | yes | Upstream base URL (see [Backend URL examples](#backend-url-examples)) |
| `api_key` | no | Token sent to the backend (`Bearer` for OpenAI, `x-api-key` for Anthropic) |
| `model` | no | Model name sent to the backend (defaults to `name`) |
| `timeout` | no | Request timeout in seconds (default: 300) |
| `type` | no | Backend type: `"openai"` (default) or `"anthropic"` |

### Backend URL examples

Use the backend's base path, not a hardcoded `/v1` assumption:

```yaml
models:
  # Standard OpenAI-compatible backend
  - name: MiniMax-M2.5
    backend: http://192.168.100.10:8000/v1
    api_key: your-backend-key

  # Provider-specific root path: requests like /v1/chat/completions
  # are appended to /api/coding/paas/v4 by go-llm-proxy.
  - name: glm-5
    backend: https://api.z.ai/api/coding/paas/v4
    api_key: your-provider-key

  # Anthropic backend — base URL omits /v1 (the proxy preserves it in the path)
  - name: claude-sonnet-4-20250514
    backend: https://api.anthropic.com
    api_key: sk-ant-your-key
    type: anthropic

  # Third-party Anthropic-compatible endpoint
  - name: MiniMax-M2.7
    backend: https://api.minimax.io/anthropic
    api_key: your-minimax-key
    type: anthropic
```

**OpenAI backends** (`type: openai` or default): The proxy strips `/v1` from the client path and appends the remainder to the backend URL. A client request to `POST /v1/chat/completions` with backend `https://api.z.ai/api/coding/paas/v4` is sent upstream as `https://api.z.ai/api/coding/paas/v4/chat/completions`.

**Anthropic backends** (`type: anthropic`): The proxy keeps `/v1` in the path. A client request to `POST /v1/messages` with backend `https://api.anthropic.com` is sent upstream as `https://api.anthropic.com/v1/messages`. This matches the Anthropic SDK convention where the base URL omits `/v1`.

### Key fields

| Field | Required | Description |
|-------|----------|-------------|
| `key` | yes | The API key value clients send via `Authorization: Bearer` or `x-api-key` header |
| `name` | yes | Friendly name for logging |
| `models` | no | List of allowed model names. Empty or omitted = all models |

Remove the `keys` section entirely to disable authentication (not recommended for public exposure).

## More Docs

- Docker: [doc/docker.md](/Users/derek/Library/Mobile%20Documents/com~apple~CloudDocs/Code/go-llm/doc/docker.md)
- Deployment, systemd, and nginx: [doc/deployment.md](/Users/derek/Library/Mobile%20Documents/com~apple~CloudDocs/Code/go-llm/doc/deployment.md)
- Build from source:

```bash
go build -o go-llm-proxy .
```

## Supported endpoints

All endpoints are proxied transparently to the backend identified by the `model` field in the request body:

| Endpoint | Description |
|----------|-------------|
| `GET /v1/models` | Aggregated model list from config |
| `POST /v1/chat/completions` | Chat completions (streaming supported) |
| `POST /v1/completions` | Text completions |
| `POST /v1/embeddings` | Embeddings |
| `POST /v1/images/generations` | Image generation |
| `POST /v1/audio/transcriptions` | Speech-to-text |
| `POST /v1/audio/translations` | Audio translation |
| `POST /v1/audio/speech` | Text-to-speech |
| `POST /v1/messages` | Anthropic Messages API |
| `POST /anthropic/v1/messages` | Anthropic Messages API (explicit prefix, validates backend type) |

### The `/anthropic/` route prefix

Clients can use `/anthropic/v1/...` instead of `/v1/...` to explicitly target Anthropic-type backends. Requests via this prefix are validated: if the resolved model is not `type: anthropic`, the proxy returns `400 Bad Request`.

This allows Anthropic SDKs to use their standard base URL convention:

```python
# base_url omits /v1 — the SDK appends /v1/messages automatically
client = anthropic.Anthropic(
    base_url="https://llm.example.com/anthropic",
    api_key="sk-your-proxy-key",
)
```

## Rate limiting

Rate limiting applies only to failed authentication attempts. Valid API keys are never throttled.

| Failed attempts | Action |
|-----------------|--------|
| 1-2 | Normal response |
| 3-4 | Throttled (computed delay within tolerance) |
| 5+ | Rejected with 429 |

Strikes decay at 1 per minute of inactivity. An IP rejected at 5 failures recovers after ~5 minutes without further attempts. Stale records are cleaned up every 5 minutes.

## Client usage

### OpenAI-compatible clients

Any OpenAI-compatible client works. Point it at your endpoint and use the model names from your config:

```python
from openai import OpenAI

client = OpenAI(
    base_url="https://llm.example.com/v1",
    api_key="sk-your-api-key-here",
)

response = client.chat.completions.create(
    model="MiniMax-M2.5",
    messages=[{"role": "user", "content": "Hello!"}],
    stream=True,
)

for chunk in response:
    print(chunk.choices[0].delta.content or "", end="")
```

### Anthropic-compatible clients

Use the `/anthropic` prefix as the base URL so the SDK constructs the correct paths:

```python
import anthropic

client = anthropic.Anthropic(
    base_url="https://llm.example.com/anthropic",
    api_key="sk-your-api-key-here",
)

message = client.messages.create(
    model="claude-sonnet-4-20250514",
    max_tokens=1024,
    messages=[{"role": "user", "content": "Hello!"}],
)
print(message.content[0].text)
```

Anthropic-style clients can also use `/v1/messages` directly without the prefix — the `/anthropic` prefix simply adds backend type validation.

## Security

- Path allowlist prevents access to arbitrary backend endpoints
- Request body capped at 50 MB to prevent memory exhaustion
- Server timeouts protect against slowloris attacks
- HTTP client does not follow redirects (prevents SSRF via backend redirects)
- Backend URLs validated on config load (scheme, host, no embedded credentials)
- Upstream response headers filtered through an allowlist
- Constant-time API key comparison
- Graceful shutdown drains active connections
