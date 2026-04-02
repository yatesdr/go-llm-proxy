# Docker Deployment

`go-llm-proxy` is a single statically-linked binary with no runtime dependencies — Docker is one config file and a volume mount away.

## Published Image

Multi-arch images are published to GHCR on version tags:

```bash
docker pull ghcr.io/yatesdr/go-llm-proxy:latest
```

Supported platforms: `linux/amd64`, `linux/arm64`.

## Quick run

```bash
cp config.yaml.example config.yaml
# edit config.yaml with your models and keys

docker run --rm \
  -p 127.0.0.1:8080:8080 \
  -v $(pwd)/config.yaml:/config/config.yaml:ro \
  ghcr.io/yatesdr/go-llm-proxy:latest
```

The image does not contain a baked-in config. Mount your own at `/config/config.yaml`.

## Enabling all features

The default entrypoint only loads the config. To enable the config generator, usage logging, and dashboard, pass additional flags:

```bash
docker run --rm \
  -p 127.0.0.1:8080:8080 \
  -v $(pwd)/config.yaml:/config/config.yaml:ro \
  -v usage-data:/data \
  ghcr.io/yatesdr/go-llm-proxy:latest \
  -config /config/config.yaml \
  -serve-config-generator \
  -log-metrics \
  -serve-dashboard \
  -usage-db /data/usage.db
```

The `/data` volume persists the SQLite usage database across container restarts.

## Compose

The included `docker-compose.yml` enables all features out of the box:

```yaml
services:
  go-llm-proxy:
    image: ghcr.io/yatesdr/go-llm-proxy:latest
    restart: unless-stopped
    ports:
      - "127.0.0.1:8080:8080"
    volumes:
      - ./config.yaml:/config/config.yaml:ro
      - usage-data:/data
    command:
      - "-config"
      - "/config/config.yaml"
      - "-serve-config-generator"
      - "-log-metrics"
      - "-serve-dashboard"
      - "-usage-db"
      - "/data/usage.db"

volumes:
  usage-data:
```

Run with:

```bash
docker compose up -d
```

To use only the proxy without extra features, override the command:

```bash
docker run --rm \
  -p 127.0.0.1:8080:8080 \
  -v $(pwd)/config.yaml:/config/config.yaml:ro \
  ghcr.io/yatesdr/go-llm-proxy:latest
```

## Build locally

```bash
docker build -t go-llm-proxy .
docker run --rm \
  -p 127.0.0.1:8080:8080 \
  -v $(pwd)/config.yaml:/config/config.yaml:ro \
  go-llm-proxy
```

## Config options via flags vs config file

Some features can be enabled via CLI flags or config file settings. Either works:

| Feature | CLI flag | Config field |
|---|---|---|
| Config generator | `-serve-config-generator` | `serve_config_generator: true` |
| Usage logging | `-log-metrics` | `log_metrics: true` |
| Usage dashboard | `-serve-dashboard` | `usage_dashboard: true` |
| Database path | `-usage-db /data/usage.db` | `usage_db: /data/usage.db` |

When using config file settings, the Docker command simplifies to just `-config /config/config.yaml` and you put everything in the YAML:

```yaml
listen: "0.0.0.0:8080"
serve_config_generator: true
log_metrics: true
usage_db: /data/usage.db
usage_dashboard: true
usage_dashboard_password: "your-dashboard-password"
# ... models, keys, etc.
```

## Networking

Docker containers have their own network namespace. Key points:

- **`listen`** must be `"0.0.0.0:8080"` inside the container (not `127.0.0.1`), otherwise the port mapping won't reach the proxy.
- **Port binding** uses `127.0.0.1:8080:8080` so the proxy is only accessible from the host's loopback. Omitting `127.0.0.1` exposes the port on all host interfaces, bypassing nginx.
- **`trusted_proxies`** must match the IP nginx connects from:
  - Host-based nginx → Docker bridge gateway (typically `172.17.0.1`)
  - Containerized nginx on the same network → that network's subnet (e.g., `172.18.0.0/16`)

Example config for Docker with host-based nginx:

```yaml
listen: "0.0.0.0:8080"
trusted_proxies:
  - 172.17.0.1
```

## Volumes

| Mount | Purpose | Required |
|---|---|---|
| `/config/config.yaml` | Proxy configuration | Yes |
| `/data` | Usage database persistence | Only if `log_metrics` enabled |

The `/data` directory is writable by the `app` user (UID 1000) that the container runs as. The config file should be mounted read-only (`:ro`).
