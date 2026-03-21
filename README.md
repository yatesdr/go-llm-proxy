# go-llm-proxy

A lightweight, secure LLM API proxy that aggregates multiple backends (vLLM, llama-server, cloud APIs) behind a single OpenAI-compatible endpoint.

No database or dependencies - single binary and a YAML config file.

Most useful for when you have some local models that you want to serve, and best used behind an Nginx reverse proxy.

## Features

- OpenAI-compatible API passthrough (completions, chat, embeddings, images, audio)
- Model name routing — clients request by name, proxy routes to the right backend
- Model name rewriting — expose friendly names while backends use internal identifiers
- API key authentication with per-key model access control
- IP-based rate limiting and throttling for failed auth attempts
- Streaming (SSE) support with proper flush handling
- Hot-reload config via `SIGHUP` — no downtime for model changes
- Graceful shutdown — active connections drain on restart
- Hardened for public internet exposure

## Building

```bash
go build -o go-llm-proxy .
```

Or cross-compile for a Linux server:

```bash
GOOS=linux GOARCH=amd64 go build -o go-llm-proxy .
```

## Configuration

Copy the example config and edit it:

```bash
cp config.yaml.example config.yaml
```

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
| `backend` | yes | Upstream base URL. This can be `/v1` for OpenAI-compatible servers or a provider-specific root such as `/api/coding/paas/v4`. |
| `api_key` | no | Bearer token sent to the backend |
| `model` | no | Model name sent to the backend (defaults to `name`) |
| `timeout` | no | Request timeout in seconds (default: 300) |

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
```

For example, a client request to:

```text
POST /v1/chat/completions
```

is sent upstream as:

```text
https://api.z.ai/api/coding/paas/v4/chat/completions
```

This is why nginx should stay a plain pass-through proxy and why the `backend` value should point at the provider's actual base path.

### Key fields

| Field | Required | Description |
|-------|----------|-------------|
| `key` | yes | The API key value clients send as `Bearer <key>` |
| `name` | yes | Friendly name for logging |
| `models` | no | List of allowed model names. Empty or omitted = all models |

Remove the `keys` section entirely to disable authentication (not recommended for public exposure).

## Running

```bash
./go-llm-proxy -config /path/to/config.yaml
```

### Hot-reload config

After editing `config.yaml`, reload without restarting:

```bash
kill -HUP $(pidof go-llm-proxy)
```

The proxy validates the new config before applying it. If validation fails, the old config stays active and an error is logged.

### Systemd service

Create `/etc/systemd/system/go-llm-proxy.service`:

```ini
[Unit]
Description=go-llm-proxy
After=network.target

[Service]
Type=simple
ExecStart=/usr/local/bin/go-llm-proxy -config /etc/go-llm-proxy/config.yaml
ExecReload=/bin/kill -HUP $MAINPID
Restart=on-failure
RestartSec=5
LimitNOFILE=65536

# Security hardening
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
ReadOnlyPaths=/etc/go-llm-proxy
PrivateTmp=true

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now go-llm-proxy
```

Reload config via systemd:

```bash
sudo systemctl reload go-llm-proxy
```

## Nginx configuration

Add this inside your existing server block that handles TLS termination, for example one managed by Certbot:

```nginx
server {
    server_name llm.example.com;

    # Proxy to go-llm-proxy without rewriting paths.
    location / {
        proxy_pass http://127.0.0.1:8080;

        # Pass real client IP for rate limiting
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header Host $host;

        # Streaming support — disable buffering
        proxy_buffering off;
        proxy_cache off;
        chunked_transfer_encoding on;

        # Timeouts — match the longest model timeout in your config
        proxy_read_timeout 600s;
        proxy_send_timeout 600s;
        proxy_connect_timeout 10s;

        # Request body size — match go-llm-proxy's limit (50 MB for vision/audio)
        client_max_body_size 50m;
    }
}
```

If go-llm-proxy runs on a different host from nginx, replace `127.0.0.1` with the internal IP. Keep nginx as a plain pass-through proxy here; go-llm-proxy handles translating `/v1/...` requests to each configured backend base URL, including provider-specific paths like `https://api.z.ai/api/coding/paas/v4`.

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

## Rate limiting

Rate limiting applies only to failed authentication attempts. Valid API keys are never throttled.

| Failed attempts | Action |
|-----------------|--------|
| 1-2 | Normal response |
| 3-9 | Progressive delay: 1s, 2s, 4s, 8s... up to 30s |
| 10+ | IP blocked for 15 minutes |

Strikes decay at 1 per minute of inactivity. Stale records are cleaned up every 5 minutes.

## Client usage

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

## Security

- Path allowlist prevents access to arbitrary backend endpoints
- Request body capped at 50 MB to prevent memory exhaustion
- Server timeouts protect against slowloris attacks
- HTTP client does not follow redirects (prevents SSRF via backend redirects)
- Backend URLs validated on config load (scheme, host, no embedded credentials)
- Upstream response headers filtered through an allowlist
- Constant-time API key comparison
- Graceful shutdown drains active connections
