# Docker Deployment

One config file, one command.

## Quick start

```bash
cp config.yaml.example config.yaml
# edit config.yaml — add your models and keys
docker compose up -d
```

That's it. The included `docker-compose.yml` mounts your config and a persistent data volume.

## Standalone run

If you don't want compose:

```bash
docker run --rm \
  -p 127.0.0.1:8080:8080 \
  -v $(pwd)/config.yaml:/config/config.yaml:ro \
  ghcr.io/yatesdr/go-llm-proxy:latest
```

## Enabling features

All features are controlled in `config.yaml` — no CLI flags needed:

```yaml
listen: "0.0.0.0:8080"           # required for Docker networking

serve_config_generator: true      # config generator UI at GET /
log_metrics: true                 # usage logging to SQLite
usage_db: /data/usage.db          # persist in the mounted volume
usage_dashboard: true             # web dashboard at /usage
usage_dashboard_password: "pick-a-password"
```

Mount a volume for `/data` to persist the usage database across container restarts:

```yaml
volumes:
  - ./config.yaml:/config/config.yaml:ro
  - proxy-data:/data
```

## Docker networking

Docker containers have their own network namespace. Two things to watch:

**`listen` must be `"0.0.0.0:8080"`** inside the container (not `127.0.0.1`), or the port mapping won't work.

**`trusted_proxies`** must match where nginx connects from:
- Host-based nginx → Docker bridge gateway (typically `172.17.0.1`)
- Containerized nginx on same network → that network's subnet

```yaml
trusted_proxies:
  - "172.17.0.1"
```

## Published images

Multi-arch images on GHCR:

```bash
docker pull ghcr.io/yatesdr/go-llm-proxy:latest
```

Supported: `linux/amd64`, `linux/arm64`.

## Build locally

```bash
docker build -t go-llm-proxy .
docker run --rm \
  -p 127.0.0.1:8080:8080 \
  -v $(pwd)/config.yaml:/config/config.yaml:ro \
  go-llm-proxy
```

## Volumes

| Mount | Purpose | Required |
|---|---|---|
| `/config/config.yaml` | Proxy configuration | Yes |
| `/data` | Usage database persistence | Only if `log_metrics` enabled |

The config file should be mounted read-only (`:ro`). The `/data` directory is writable by the `app` user (UID 1000) that the container runs as.
