# Eidonix connect

go-llm-proxy can use services that live at remote sites through an
**eidonix-connect** hub. The hub runs next to the proxy and makes each site's
services reachable on a private network as plain HTTP:

```
http://connect:8080/<site>/<service>/<path>
```

go-llm-proxy needs no special client code for this. You write the backend as a
`connect://` URL and the proxy resolves it. Everything else keeps working by URL
exactly as before; `http://` and `https://` backends are untouched.

## Configuration

```yaml
connect:                              # optional; both fields have defaults
  url: http://connect:8080            # the hub (default)
  application: go-llm-proxy           # sent as X-Eidonix-Application (default)

models:
  - name: glm
    backends:
      - url: connect://site-a/chat/v1       # -> http://connect:8080/site-a/chat/v1
      - url: https://api.example.com/v1     # ordinary backends mix freely
```

`connect://<site>/<service>/<path>` is accepted wherever a backend URL is:
model backends (including replica lists), audio (`whisper`, `tts`), documents
(`paddleocr`), vision and other processor models, the Qdrant `services` backend,
context-window detection, and the admin "Test" buttons. Without a `connect:`
block the defaults apply.

The proxy appends the usual request path to what follows the service, so
`connect://site-a/chat/v1` plus `/chat/completions` reaches the site's `chat`
service as `POST /v1/chat/completions`.

### Labels

Requests to connect backends carry two optional labels that appear in the hub's
and the site's access logs:

| Header | Value |
|---|---|
| `X-Eidonix-Application` | `connect.application` (default `go-llm-proxy`) |
| `X-Eidonix-For` | the name of the API key (or Qdrant app key) serving the request |

Labels are cleaned to printable ASCII and cut to 64 characters. They are sent
only to connect backends, never to plain http(s) ones. They identify, they do not
authenticate.

## Health

The proxy does not probe through the hub. It polls the hub catalog
(`GET /v1/catalog/<site>`, one request per site, every 15 seconds) and judges each
connect backend from it:

- site online, service offered and healthy: up;
- anything else: down, with the hub's reason (`site site-a is offline`,
  `service chat at site site-a is reported unhealthy`, an unknown site or
  service, or a hub error / unreachable hub).

The result feeds the same health and circuit-breaker state as any other backend,
so replica selection and failover behave as usual. Real traffic still counts: a
success clears a stale "down", and repeated failures open the breaker. The admin
"Test" buttons on a connect backend ask the catalog too.

The admin LLM page shows a read-only **Connected services** card (site, online,
service, models, healthy) whenever a `connect:` block or a `connect://` backend is
configured.

## Timeouts

Responses from remote sites can be slow to start (long prefill, non-streaming
generations). The proxy has no transport-level response-header timeout; the wait
is bounded by the model's `timeout` (seconds, default 300), which stays armed
until the response headers arrive. A 2xx event stream then runs without a time
limit; anything else is bounded by `timeout`. Connecting and TLS still have 10 s
limits.

## Deploying next to the hub

`docker/compose.connect.yaml` runs the proxy on two **existing** external Docker
networks and publishes no ports:

- `eidonix-connect`: where the hub (`connect`) lives;
- `eidonix-edge`: where your reverse proxy lives; the proxy answers there as
  `go-llm-proxy:8080`.

```bash
cd docker
mkdir -p config && cp ../config.yaml.example config/config.yaml   # then edit
echo 'GO_LLM_ADMIN_PASSWORD=choose-a-password' > .env
docker compose -f compose.connect.yaml up -d --build
```

The config directory (not a single file) and the `proxy-data` volume work as in
`docker/docker-compose.yml`. Point a reverse proxy on `eidonix-edge` at
`go-llm-proxy:8080`, and use `connect://` backends in `config/config.yaml`.
