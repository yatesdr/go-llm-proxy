# Deployment Notes

This document covers production deployment details that are intentionally kept out of the top-level README.

## Binary Run

```bash
./go-llm-proxy -config /path/to/config.yaml
```

## Hot Reload

After editing `config.yaml`, reload without restarting:

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
    }
}
```

Keep nginx as a plain pass-through proxy. `go-llm-proxy` maps `/v1/...` requests onto each configured backend base URL, including provider-specific paths like `https://api.z.ai/api/coding/paas/v4`.
