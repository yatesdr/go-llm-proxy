# Config Counter (Cloudflare Worker)

Simple counter API for tracking config generator usage on the GitHub Pages site.

## Setup

```sh
npm install -g wrangler
wrangler login

cd cloudflare-counter
wrangler kv namespace create COUNTER
# Copy the printed id into wrangler.toml

wrangler dev          # test locally at http://localhost:8787
wrangler deploy       # deploy to https://config-counter.<subdomain>.workers.dev
```

## API

- `POST /increment` — increments counter, returns `{"count": N}`
- `GET /count` — returns current count `{"count": N}`

## After Deploy

Update the `COUNTER_URL` variable in `docs/configure.html` with your worker URL.

## Free Tier Limits

- 100K requests/day, 100K KV reads/day, 1K KV writes/day — more than enough.
