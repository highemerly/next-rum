# rum-collector

Self-hosted Real User Monitoring for a personal Fediverse / web service fleet.
Collects Core Web Vitals, Navigation Timing, and Cloudflare cache status from the
browser and aggregates them at `rum.piyo.me`, feeding an existing
VictoriaMetrics + Loki + Grafana stack.

Design: see the `rum-design.md` design document (kept alongside this repo).
Section references below (§5, §8, §14, …) point into it.

## What it does

- Serves a one-line loader (`beacon.js`) deployable verbatim on every service —
  service identity and URL grouping come from `rum-config.json`, not the tag.
- Ingests beacons on `POST /collect`, records bounded-cardinality histograms and
  counters for VictoriaMetrics to scrape at `/metrics`, and pushes detailed logs
  to Loki.
- Privacy-first: no user/session IDs, cookies, IPs, query strings, or raw UA. GPC
  is honored client-side. See design §5.

## Layout

```
assets/                  embedded client assets
  beacon.js              loader (GPC gate, config fetch, web-vitals, sendBeacon)
  web-vitals.js          GoogleChrome/web-vitals v4 (ESM)
  rum-config.json        single source: host->service, path->path_group
cmd/rum-collector/       main: env, config hot-reload, graceful shutdown
internal/
  config/                rum-config.json compile, glob, label validation (+tests)
  metrics/               Prometheus histograms/counters (design §8)
  loki/                  batched /loki/api/v1/push client
  collect/               POST /collect handler (+tests)
  server/                routes, CORS, embedded-asset serving
embed.go                 //go:embed of assets into the binary
deploy/
  k8s/                   Namespace, ConfigMap, Deployment+HPA, Service, IngressRoute, VMServiceScrape
  traefik/               rewriteBody injection middleware (Mastodon/handon.club)
  cloudflare-worker/     HTMLRewriter injection (Misskey/mi.hiyoko.club)
  cloudflare/            Transform Rules for Server-Timing (cache visibility)
  snippets/              direct <head> inject examples (Next.js etc.)
  grafana/               dashboard JSON
  vmalert/               alert rules
```

## Endpoints

| Method | Path               | Purpose                       | CORS                |
|--------|--------------------|-------------------------------|---------------------|
| GET    | `/beacon.js`       | loader JS                     | ACAO `*`            |
| GET    | `/web-vitals.js`   | web-vitals (module import)    | ACAO `*`            |
| GET    | `/rum-config.json` | live config                   | ACAO `*`            |
| POST   | `/collect`         | beacon intake (204)           | ACAO = allowed origin |
| GET    | `/metrics`         | Prometheus (internal only)    | —                   |
| GET    | `/healthz`         | liveness                      | —                   |

## Develop

```bash
# Run locally against the checked-in config (Loki disabled).
CONFIG_PATH=assets/rum-config.json LISTEN_ADDR=:8080 go run ./cmd/rum-collector

# In another shell:
curl -s localhost:8080/healthz
curl -s -X POST localhost:8080/collect -H 'Content-Type: text/plain' \
  -d '{"kind":"web_vitals","service":"handon","path_group":"web_ui","name":"LCP","value":2341,"rating":"good","device":"mobile","nav_type":"navigate"}'
curl -s localhost:8080/metrics | grep web_vitals_lcp

go test ./...
```

### Local end-to-end in a real browser

`beacon.js` derives the collector origin from its own `src`, so the same file
works locally. No Loki needed (`LOKI_URL` unset disables push).

```bash
# 1. Collector on :8080 with a dev config that maps localhost -> "dev"
CONFIG_PATH=testdata/rum-config.dev.json LISTEN_ADDR=:8080 go run ./cmd/rum-collector

# 2. Serve the demo page on :8000 (separate shell). It loads
#    http://localhost:8080/beacon.js and needs an http origin (not file://).
cd testdata && python3 -m http.server 8000

# 3. Open http://localhost:8000/demo.html in Chrome.
#    DevTools > Network: one /rum-config.json, one /web-vitals.js, and several
#    /collect beacons (navigation + web_vitals: LCP/FCP/TTFB; click the button
#    for INP). Then check the metrics:
curl -s http://localhost:8080/metrics | grep 'service="dev"'
```

CLS/LCP/INP report on real interaction and tab-hide, so click the button and
switch tabs (or close the page) to flush the final values.

### Environment

| Var                     | Default                       | Notes                                    |
|-------------------------|-------------------------------|------------------------------------------|
| `LISTEN_ADDR`           | `:8080`                       |                                          |
| `CONFIG_PATH`           | `assets/rum-config.json`      | ConfigMap mount in production            |
| `LOKI_URL`              | (empty)                       | empty disables Loki push                 |
| `ASSET_MAX_AGE_SECONDS` | `300`                         | Cache-Control for static assets          |
| `CONFIG_RELOAD_SECONDS` | `60`                          | hot-reload interval for `rum-config.json`|

## Inject the loader

One line, identical on every service:

```html
<script async src="https://rum.piyo.me/beacon.js"></script>
```

- Self-hosted apps: write it into `<head>` (`deploy/snippets/`).
- Mastodon (`handon.club`): Traefik `rewriteBody` middleware (`deploy/traefik/`).
- Misskey (`mi.hiyoko.club`): Cloudflare Worker (`deploy/cloudflare-worker/`).
- Every zone: add the Server-Timing Transform Rule (`deploy/cloudflare/`) so cache
  status is visible to the browser.

## Build the image

```bash
docker build -t ghcr.io/highemerly/rum-collector:latest .
```

## Roadmap

Phase 2 (not implemented): NEL via `POST /report`, CSP violation reports, JS error
collection, Long Tasks, Resource Timing. See design §14.
