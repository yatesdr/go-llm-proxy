# Docker Deployment

`go-llm-proxy` works well in Docker because it is a single stateless binary with one mounted config file.

## Published Image

Version tags publish multi-arch images to GHCR:

```bash
docker pull ghcr.io/yatesdr/go-llm-proxy:latest
```

Supported container platforms:

- `linux/amd64`
- `linux/arm64`

## Run

Create a local config first:

```bash
cp config.yaml.example config.yaml
```

Run the published image:

```bash
docker run --rm \
  -p 127.0.0.1:8080:8080 \
  -v $(pwd)/config.yaml:/config/config.yaml:ro \
  ghcr.io/yatesdr/go-llm-proxy:latest
```

The image does not contain a baked-in config. Mount your own file at `/config/config.yaml`.

## Compose

A sample Compose file is included in the repo:

```bash
docker compose up -d
```

Current example:

```yaml
services:
  go-llm-proxy:
    image: ghcr.io/yatesdr/go-llm-proxy:latest
    restart: unless-stopped
    ports:
      - "127.0.0.1:8080:8080"
    volumes:
      - ./config.yaml:/config/config.yaml:ro
```

## Build Locally

```bash
docker build -t go-llm-proxy .
```

Then run it:

```bash
docker run --rm \
  -p 127.0.0.1:8080:8080 \
  -v $(pwd)/config.yaml:/config/config.yaml:ro \
  go-llm-proxy
```

## Networking

Docker containers have their own network namespace, which affects `listen` and `trusted_proxies` in your config:

- **Inside the container**, `127.0.0.1` refers to the container itself, not the host. Use `listen: "0.0.0.0:8080"` in your `config.yaml` so the proxy is reachable from the host via the mapped port.
- **Port binding** uses `127.0.0.1:8080:8080` so the proxy is only accessible from the host's loopback interface. Omitting `127.0.0.1` exposes the port on all host interfaces, bypassing nginx.
- **trusted_proxies** must match the IP that nginx connects from. When nginx runs on the host, the proxy sees the Docker bridge gateway (typically `172.17.0.1`). When nginx runs in a separate container on the same Docker network, use that network's subnet (e.g., `172.18.0.0/16`).

Example `config.yaml` for Docker with host-based nginx:

```yaml
listen: "0.0.0.0:8080"
trusted_proxies:
  - 172.17.0.1
```
