# Usage Logging and Dashboard

go-llm-proxy can log per-request metrics to a local SQLite database and serve an interactive dashboard for monitoring usage across users, models, and time periods.

## Enabling usage logging

Usage logging is off by default. Enable it with a CLI flag or config setting:

**CLI flag** (takes priority):
```bash
./go-llm-proxy -config config.yaml -log-metrics
```

**Config file**:
```yaml
log_metrics: true
usage_db: /var/lib/go-llm/usage.db   # optional, default: usage.db in working directory
```

The database is created automatically on first run. No migrations or external dependencies are needed — it uses an embedded SQLite engine with WAL mode for safe concurrent access.

### Database path

The database location is resolved in this order:
1. `--usage-db` CLI flag (highest priority)
2. `usage_db` config field
3. `usage.db` in the working directory (default)

## What data is collected

Every proxied request logs a single row with:

| Field | Description |
|---|---|
| `timestamp` | Request start time (UTC, RFC 3339) |
| `key_hash` | First 16 hex chars of SHA-256 of the API key (identifies the user without storing the key) |
| `key_name` | Friendly name from config (e.g., "admin", "guest") |
| `model` | Model name from the request |
| `endpoint` | Request path (e.g., `/v1/chat/completions`, `/v1/responses`, `/v1/messages`) |
| `status_code` | HTTP status returned to the client |
| `request_bytes` | Size of the request body forwarded upstream |
| `response_bytes` | Total bytes received from upstream |
| `input_tokens` | Prompt/input tokens (extracted from upstream response) |
| `output_tokens` | Completion/output tokens |
| `total_tokens` | Sum of input + output tokens |
| `duration_ms` | Total request duration in milliseconds |

### Token extraction

Tokens are extracted from the upstream response automatically:

- **OpenAI backends**: from the `usage` object (`prompt_tokens`, `completion_tokens`, `total_tokens`)
- **Anthropic backends**: from `usage` in `message_start` and `message_delta` SSE events (includes `cache_creation_input_tokens` and `cache_read_input_tokens` in the input count)
- **Streaming responses**: scanned from SSE data lines
- **Responses API (translated)**: extracted from the Chat Completions stream during translation
- **Responses API (native passthrough)**: token counts are not available (would require buffering the full response); byte counts are still logged

### Privacy

API keys are never stored. Only the first 16 hex characters of the SHA-256 hash are recorded — enough to identify distinct users but not reversible to the original key.

## Usage dashboard

The dashboard is a password-protected web UI at `/usage` that visualizes usage data.

### Enabling the dashboard

The dashboard requires usage logging to be enabled first. Add both settings:

```yaml
log_metrics: true
usage_dashboard: true
usage_dashboard_password: "a-strong-password"
```

Or with CLI flags:
```bash
./go-llm-proxy -config config.yaml -log-metrics -serve-dashboard
```

**Note**: `usage_dashboard_password` is always required in the config even when using the CLI flag. The proxy will refuse to start if the dashboard is enabled without a password.

### Dashboard endpoints

| Endpoint | Method | Auth | Description |
|---|---|---|---|
| `/usage` | GET | Cookie | Dashboard page (or login form if not authenticated) |
| `/usage` | POST | Password | Login form submission |
| `/usage/data` | GET | Cookie | JSON API for dashboard data |

### Authentication

The dashboard uses a simple password login:

1. Navigate to `/usage`
2. Enter the configured password
3. A secure HTTP-only cookie is set (30-day expiry, SameSite=Strict, Secure flag on HTTPS)

Failed login attempts are rate-limited per IP using the same throttling as the main API (tracked separately).

### Dashboard data

The `/usage/data` endpoint returns JSON with these sections:

| Section | Contents |
|---|---|
| `totals` | Request count, total tokens, unique users, error rate |
| `daily` | Per-day breakdown: requests, tokens, errors |
| `daily_models` | Per-day per-model breakdown: requests, tokens |
| `users` | Per-user summary: requests, tokens, active days, last seen |
| `models` | Per-model summary: requests, unique users, tokens, avg latency |

The default time range is 30 days. Use the `?days=N` query parameter on `/usage/data` to adjust.

## CLI reports

For quick terminal-based reporting without starting the server, use the report flags:

### Usage report (per-user daily breakdown)

```bash
./go-llm-proxy -usage-report -report-days 7
```

Output:
```
DATE        USER     KEY       REQUESTS  OK  ERROR  INPUT TOK  OUTPUT TOK  TOTAL TOK  REQ BYTES  RESP BYTES  AVG MS
----        ----     ---       --------  --  -----  ---------  ----------  ---------  ---------  ----------  ------
2026-04-01  admin    a1b2c3d4  142       140 2      1,245,000  312,000     1,557,000  2.1 MB     45.3 MB     1842
2026-04-01  guest    e5f6g7h8  23        23  0      89,000     22,000      111,000    156.2 KB   3.2 MB      956

=== User Summary ===
USER     KEY       REQUESTS  INPUT TOK  OUTPUT TOK  TOTAL TOK  ACTIVE DAYS  FIRST SEEN  LAST SEEN
----     ---       --------  ---------  ----------  ---------  -----------  ----------  ---------
admin    a1b2c3d4  892       8,234,000  2,156,000   10,390,000 7            2026-03-25  2026-04-01
guest    e5f6g7h8  134       612,000    153,000     765,000    5            2026-03-27  2026-04-01
```

### Model report (per-model breakdown)

```bash
./go-llm-proxy -model-report -report-days 30
```

Output:
```
=== Model Summary ===
MODEL             REQUESTS  USERS  INPUT TOK  OUTPUT TOK  TOTAL TOK  AVG MS
-----             --------  -----  ---------  ----------  ---------  ------
MiniMax-M2.5      645       3      5,234,000  1,456,000   6,690,000  1523
qwen-3.5          234       2      2,100,000  890,000     2,990,000  2341
nomic-embed        89       1      445,000    0           445,000    45
```

### Specifying the database path

If your database is not at the default location:

```bash
./go-llm-proxy -usage-report -usage-db /var/lib/go-llm/usage.db -report-days 14
```

The report commands are read-only — they open the database in read-only mode and do not interfere with a running proxy.

## Disabling usage logging

To disable logging, remove `log_metrics: true` from your config (or omit the `-log-metrics` flag) and restart the proxy. The database file is retained but no new rows are written. You can safely delete the `.db` file if you no longer need the data.

To disable only the dashboard while keeping logging active, remove `usage_dashboard: true` from the config.

## Database schema

The database contains a single `usage` table:

```sql
CREATE TABLE usage (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp      TEXT    NOT NULL,
    key_hash       TEXT    NOT NULL,
    key_name       TEXT    NOT NULL DEFAULT '',
    model          TEXT    NOT NULL,
    endpoint       TEXT    NOT NULL DEFAULT '',
    status_code    INTEGER NOT NULL DEFAULT 0,
    request_bytes  INTEGER NOT NULL DEFAULT 0,
    response_bytes INTEGER NOT NULL DEFAULT 0,
    input_tokens   INTEGER NOT NULL DEFAULT 0,
    output_tokens  INTEGER NOT NULL DEFAULT 0,
    total_tokens   INTEGER NOT NULL DEFAULT 0,
    duration_ms    INTEGER NOT NULL DEFAULT 0
);
```

Indexed on `timestamp`, `key_hash`, and `model` for efficient reporting queries. The database uses WAL journaling and a 5-second busy timeout for safe concurrent access between the proxy writer and report/dashboard readers.
