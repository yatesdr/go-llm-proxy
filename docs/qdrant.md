# Qdrant Vector Database Proxy

The proxy can front a [Qdrant](https://qdrant.tech/) vector database, adding authentication and automatic multi-tenant isolation.

## Configuration

```yaml
services:
  qdrant:
    backend: http://192.168.5.143:6333
    api_key: your-qdrant-backend-key    # optional, sent to Qdrant
    app_keys:
      - name: webapp
        key: qd-webapp-abc123
      - name: crawler
        key: qd-crawler-def456
```

- **`backend`** — Qdrant server URL
- **`api_key`** — API key sent to the Qdrant backend (optional)
- **`app_keys`** — List of application keys for proxy authentication

App keys are separate from model API keys. This lets you grant Qdrant access without exposing LLM models, and vice versa.

## Usage

Access Qdrant via `/qdrant/*` with an app key:

```bash
# List collections
curl https://your-proxy/qdrant/collections \
  -H "Authorization: Bearer qd-webapp-abc123"

# Insert points
curl -X PUT https://your-proxy/qdrant/collections/docs/points \
  -H "Authorization: Bearer qd-webapp-abc123" \
  -H "Content-Type: application/json" \
  -d '{
    "points": [
      {"id": 1, "vector": [0.1, 0.2, ...], "payload": {"text": "..."}}
    ]
  }'

# Search
curl -X POST https://your-proxy/qdrant/collections/docs/points/search \
  -H "Authorization: Bearer qd-webapp-abc123" \
  -H "Content-Type: application/json" \
  -d '{"vector": [0.1, 0.2, ...], "limit": 5}'
```

## App Isolation

The proxy automatically isolates each app's data:

1. **On writes** (`PUT /collections/*/points`) — the proxy injects `"app": "<app_name>"` into each point's payload
2. **On searches** (`POST /collections/*/points/search|scroll|query`) — the proxy adds a filter clause to restrict results to the calling app
3. **On deletes** (`POST /collections/*/points/delete`) — the filter ensures apps can only delete their own points

This is transparent to clients. Apps don't need to know about isolation — they see only their own data.

### Example

When `webapp` inserts a point:

```json
{"points": [{"id": 1, "vector": [...], "payload": {"text": "..."}}]}
```

The proxy transforms it to:

```json
{"points": [{"id": 1, "vector": [...], "payload": {"text": "...", "app": "webapp"}}]}
```

When `webapp` searches:

```json
{"vector": [...], "limit": 5}
```

The proxy transforms it to:

```json
{"vector": [...], "limit": 5, "filter": {"must": [{"key": "app", "match": {"value": "webapp"}}]}}
```

The `crawler` app cannot see `webapp`'s vectors, and vice versa.

## Endpoints

All Qdrant REST API endpoints are supported. The proxy passes through requests to the backend after applying auth and isolation transformations.

Common endpoints:
- `GET /collections` — list collections
- `PUT /collections/{name}` — create collection
- `GET /collections/{name}` — get collection info
- `PUT /collections/{name}/points` — upsert points
- `POST /collections/{name}/points/search` — search vectors
- `POST /collections/{name}/points/scroll` — scroll through points
- `POST /collections/{name}/points/query` — query points
- `POST /collections/{name}/points/delete` — delete points

## Logging

Qdrant requests are logged to the usage database (if enabled) with:
- Model field set to `qdrant`
- Endpoint showing the full path (e.g., `/qdrant/collections/docs/points/search`)
- Request/response byte counts
- Duration
