# Docker Deployment

One config file, one command.

## First-time setup, start to finish

This walks through exactly what a brand-new deployment looks like — what
you get before touching the config, and how to turn on the rest.

**1. Get the compose file, an env file, and a config directory.**

```bash
mkdir -p go-llm-proxy/config && cd go-llm-proxy
curl -O https://raw.githubusercontent.com/yatesdr/go-llm-proxy/master/docker/docker-compose.yml
curl -O https://raw.githubusercontent.com/yatesdr/go-llm-proxy/master/docker/.env.example
curl -o config/config.yaml https://raw.githubusercontent.com/yatesdr/go-llm-proxy/master/config.yaml.example
cp .env.example .env
```

(Or clone the repo and copy `docker/docker-compose.yml`, `docker/.env.example`,
and `config.yaml.example` from a checkout — same result.)

**2. Set an admin password in `.env`.**

```bash
# .env
GO_LLM_ADMIN_PASSWORD=pick-a-strong-password
```

This is the one credential you need before you can log in anywhere — no
YAML editing required for it.

**3. Bring it up.**

```bash
docker compose up -d
docker compose ps        # should show "healthy" within a few seconds
curl http://localhost:8080/healthz
```

Out of the box, before you've edited `config.yaml` at all, you already have:
- a working admin console at `/admin`, logged in with the password from step 2
- one placeholder model and one placeholder key, so there's something to
  point a client at immediately:

```yaml
models:
  - name: my-model
    backends:
      - url: http://192.168.1.10:8000/v1   # <- not real, edit this

keys:
  - key: sk-change-me-to-something-secure   # <- not real, edit this
    name: admin
```

The config generator page at `/` and the usage dashboard at `/usage` are
still **off by default** — those need explicit YAML flags (step 5), since
unlike the admin console they aren't things you need just to get started.

**4. Replace the placeholders through the admin UI.**

Log into `/admin`, edit the model's backend URL and add a real key, right
from the browser — no YAML editing, no restart. Changes made through the
admin UI always apply live because the write happens from inside the
running container.

If you'd rather hand-edit `config/config.yaml` directly from the host
instead: that reloads automatically on Linux hosts, but on Docker Desktop
for Mac/Windows the file-watcher sometimes doesn't see host-side edits
through the bind mount — `docker compose restart` always works as a
fallback if a save doesn't seem to take effect.

**5. (Optional) Turn on the config generator and usage dashboard.**

Add to `config/config.yaml`:

```yaml
serve_config_generator: true      # config generator UI at GET /
log_metrics: true                 # usage logging to SQLite (needed for the dashboard)
usage_dashboard: true             # web dashboard at /usage
```

Then either set `GO_LLM_USAGE_DASHBOARD_PASSWORD` in `.env`, or add
`usage_dashboard_password:` directly to `config.yaml` — either works, same
precedence as the admin password (YAML wins if set). `docker compose restart`
to apply, then log in at `/usage`.

## Quick start (condensed)

```bash
mkdir -p config
cp config.yaml.example config/config.yaml
cp docker/.env.example docker/.env
# edit docker/.env — set GO_LLM_ADMIN_PASSWORD
docker compose -f docker/docker-compose.yml up -d
```

The included `docker/docker-compose.yml` mounts your config directory and a persistent data volume, and reads `docker/.env` for bootstrap credentials.

## Standalone run

If you don't want compose:

```bash
docker run --rm \
  -p 127.0.0.1:8080:8080 \
  -v $(pwd)/config:/config \
  -v proxy-data:/data \
  ghcr.io/yatesdr/go-llm-proxy:latest
```

## Enabling features

Everything except the admin/dashboard passwords (which can come from `.env`,
see the walkthrough above) is controlled in `config.yaml`:

```yaml
serve_config_generator: true      # config generator UI at GET /
log_metrics: true                 # usage logging to SQLite
usage_dashboard: true             # web dashboard at /usage
usage_dashboard_password: "pick-a-password"   # or GO_LLM_USAGE_DASHBOARD_PASSWORD in .env
admin_password: "pick-a-password"             # or GO_LLM_ADMIN_PASSWORD in .env
```

The image already defaults usage logging to `/data/usage.db` (the mounted
volume), so you don't need to set `usage_db` yourself unless you want a
different path.

## Telemetry / usage database

If `log_metrics: true`, every proxied request logs one row to a SQLite
database at `/data/usage.db` (inside the named volume, so it survives
container recreation). Full details — schema, what's collected, how big it
gets over time, and how to prune it — are in
[docs/usage.md](usage.md#growth-over-time). The short version: **there's no
built-in retention limit** — the dashboard's 7/30/90-day range picker only
controls what's *displayed*, it never deletes rows. The database grows for
as long as logging stays on, roughly ~300 bytes per logged request based on
real-world measurement. Manage disk usage yourself if that matters to you —
see the linked section for the exact commands.

## Docker networking

Docker containers have their own network namespace. Two things to watch:

**`listen` works unchanged.** The shipped default (`":8080"`) already binds
all interfaces inside the container, so Docker's port mapping (`-p` /
`ports:`) works with no edit needed. The one thing that *would* break it is
explicitly setting `listen` to `"127.0.0.1:8080"` — don't do that inside a
container, since it'd only accept connections from inside the container
itself.

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
docker build -t go-llm-proxy -f docker/Dockerfile .
docker run --rm \
  -p 127.0.0.1:8080:8080 \
  -v $(pwd)/config:/config \
  go-llm-proxy
```

## Volumes

| Mount | Purpose | Required |
|---|---|---|
| `/config` | Directory containing `config.yaml` | Yes |
| `/data` | Usage database persistence | Only if `log_metrics` enabled |

**Mount the `/config` directory, not the file directly.** The admin UI
(`/admin`) saves config changes with an atomic write-then-rename, which fails
with `device or resource busy` if `config.yaml` itself is bind-mounted as a
single file — bind-mounting the parent directory avoids this. The config
file must be writable (not `:ro`) whenever the admin UI is enabled. The image
runs as the unprivileged `app` user, so on Linux the host directory must also
grant that container UID/GID write access. For a bind mount, check the image
identity with `docker exec go-llm-proxy id`, then make the config directory
group-writable (using the container GID) and enable setgid inheritance; keep
the config file group-readable/writable as well. This preserves host-side
editing while allowing the admin's atomic save to create its temporary file.

A brand-new named volume gets the correct ownership automatically (the image
seeds `/data` for its `app` user on first mount) — no manual `chown` needed
for a normal `docker compose up`. This only becomes a concern if you're
manually copying an existing `usage.db` into the volume yourself (e.g.
migrating from a bare-metal install); in that case match the container's
`app` user (uid 100, gid 101 in the current image) or the proxy won't be
able to write to it.

## Health check

The image ships a `HEALTHCHECK` against `GET /healthz` (unauthenticated,
always available). `docker ps` and `docker compose ps` will show
`(healthy)`/`(unhealthy)` once the container passes its first check.
