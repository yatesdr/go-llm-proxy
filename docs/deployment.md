# Deployment

Production deployment details for go-llm-proxy.

## Production checklist

1. Deploy behind nginx with TLS (see [nginx config](#nginx) below)
2. Bind to localhost: `listen: "127.0.0.1:8080"`
3. Configure `trusted_proxies` to your nginx IP(s)
4. Use strong API keys (32+ characters) — generate with `./go-llm-proxy -adduser`
5. Set appropriate `timeout` per model (default 300s)
6. Enable usage logging: `log_metrics: true`
7. Disable config generator after setup, or protect with nginx auth
8. Set up systemd for automatic restart (see below)

## Binary run

```bash
./go-llm-proxy -config /path/to/config.yaml
```

## Hot Reload

The proxy automatically reloads `config.yaml` when the file changes on disk (via filesystem notifications). No manual action is needed — just save the file.

You can also trigger a reload manually via `SIGHUP`:

```bash
kill -HUP $(pidof go-llm-proxy)
```

If validation fails, the old config stays active and an error is logged.

## systemd

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

Enable and start it:

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now go-llm-proxy
```

Reload config:

```bash
sudo systemctl reload go-llm-proxy
```

## nginx

Add this inside your existing TLS-enabled server block, for example one managed by Certbot:

```nginx
# Per-IP connection limiting (place in http block or a shared conf snippet).
limit_conn_zone $binary_remote_addr zone=llm_conn:10m;

server {
    server_name llm.example.com;

    location / {
        proxy_pass http://127.0.0.1:8080;

        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header Host $host;

        proxy_buffering off;
        proxy_cache off;
        chunked_transfer_encoding on;

        proxy_read_timeout 600s;
        proxy_send_timeout 600s;
        proxy_connect_timeout 10s;

        client_max_body_size 50m;

        # Limit concurrent connections per IP to prevent resource exhaustion.
        limit_conn llm_conn 20;
    }
}
```

Keep nginx as a plain pass-through proxy. `go-llm-proxy` maps `/v1/...` and `/anthropic/...` requests onto each configured backend base URL, including provider-specific paths like `https://api.z.ai/api/coding/paas/v4`.

If the proxy runs on a different host from nginx, change `proxy_pass` to that host's IP (e.g., `http://192.168.5.144:8080`).

## TLS

**API keys are sent in every request.** Running without TLS exposes them on the network. Always terminate TLS at nginx.

### Certbot (Let's Encrypt)

The easiest path for a public-facing server with a domain name:

```bash
sudo apt install certbot python3-certbot-nginx   # Debian/Ubuntu
sudo certbot --nginx -d llm.example.com
```

Certbot will modify your nginx config to add the `listen 443 ssl` block and certificate paths, and set up automatic renewal. No manual certificate management needed.

### Self-signed certificates

For internal/lab deployments without a public domain:

```bash
openssl req -x509 -nodes -days 365 -newkey rsa:2048 \
  -keyout /etc/ssl/private/llm-proxy.key \
  -out /etc/ssl/certs/llm-proxy.crt \
  -subj "/CN=llm.internal"
```

Add to your nginx server block:

```nginx
listen 443 ssl;
ssl_certificate /etc/ssl/certs/llm-proxy.crt;
ssl_certificate_key /etc/ssl/private/llm-proxy.key;
```

Clients connecting to a self-signed endpoint will need to disable certificate verification or trust the CA. For coding assistants, this typically means setting an environment variable (e.g., `NODE_TLS_REJECT_UNAUTHORIZED=0`) or configuring the system trust store.
