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
- Ingests beacons on `POST /collect` (single event or a batched array), records
  bounded-cardinality histograms and counters for VictoriaMetrics to scrape at
  `/metrics`, and pushes detailed logs to Loki.
- Privacy-first: no user/session IDs, cookies, IPs, query strings, or raw UA
  string. Only coarse `device` / `os` / `browser` (no versions) are derived from
  UA-CH, plus visitor `country` / `asn` read server-side from Cloudflare edge
  headers (the IP itself is never read or stored). GPC is honored client-side.
  See design §5.

### Beacon batching

Web Vitals finalize at different times, so the loader does not POST per metric.
It coalesces into at most **two** `/collect` beacons (each a JSON array):

1. a **load batch** flushed at **3s** — `navigation` + `TTFB` + `FCP`, the
   metrics that finalize during load;
2. a **tail batch** flushed when the page is hidden/unloaded
   (`visibilitychange`→hidden / `pagehide`) — `LCP` (finalizes on first input),
   `INP`, `CLS`.

Buffering the tail until hide minimizes requests; a metric that never reaches the
server (e.g. the tab crashes before hide) is acceptable loss for RUM. The
collector accepts a single event object or an array.

### OS / browser

`os` (`windows`/`macos`/`ios`/`android`/`linux`/`chromeos`/`other`) and `browser`
(`chrome`/`firefox`/`safari`/`edge`/`opera`/…) are kept **off the latency
histograms** to protect their cardinality. They land in Loki and in a dedicated
counter `rum_pageview_total{service,path_group,device,os,browser,nav_type}`
(one increment per page load) for distribution panels.

### Country / ASN (Cloudflare edge geo)

The collector reads the visitor `country` (`CF-IPCountry`, a CF default header)
and `asn` (`HAN-CF-ASN`, injected by a CF Transform Rule) from request headers —
**Cloudflare resolves geo at the edge and we read only the coarse code; the IP is
never read or stored.** Both land in Loki and in dedicated counters
`rum_country_total{service,country}` / `rum_asn_total{service,asn}` (kept separate
so their large value spaces don't multiply other labels). The header names are
configurable (`GEO_COUNTRY_HEADER` / `GEO_ASN_HEADER`) so the CF rule can be
renamed without a rebuild. Setup: `deploy/cloudflare/transform-rules.md`.

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
  k8s/                   manifests for k8sg1-repo manifests/rum/prd/ (+ setup runbook)
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
| `GEO_COUNTRY_HEADER`    | `CF-IPCountry`                | request header carrying the country code |
| `GEO_ASN_HEADER`        | `HAN-CF-ASN`                  | request header carrying the AS number    |

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

## Release / deploy

Tag-driven, same flow as the other services (movapic-neo / nmd):

```bash
git tag v0.1.0 && git push origin v0.1.0
```

`.github/workflows/docker-build.yml` runs `checks` (gofmt/vet/test) and
`build-and-push` (ghcr.io, `linux/amd64`) in parallel; when both are green it
opens a PR against `highemerly/k8sg1-repo` bumping the `image:` line in
`manifests/rum/prd/deployment.yaml`. Merging that PR lets Flux roll it out.

Branch pushes run `ci.yml` → the same `checks.yml`, so CI never runs twice.

First-time cluster setup (namespace, cert, Flux Kustomization, DNS, `K8SG1_PAT`)
and the post-rollout verification runbook: **[deploy/k8s/README.md](deploy/k8s/README.md)**.

Local image build (arm64 Mac → amd64 node needs the platform flag):

```bash
docker build --platform linux/amd64 -t ghcr.io/highemerly/rum-collector:dev .
```

## Roadmap

Phase 2 (not implemented): NEL via `POST /report`, CSP violation reports, JS error
collection, Long Tasks, Resource Timing. See design §14.
