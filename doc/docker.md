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
  -p 8080:8080 \
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
      - "8080:8080"
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
  -p 8080:8080 \
  -v $(pwd)/config.yaml:/config/config.yaml:ro \
  go-llm-proxy
```
