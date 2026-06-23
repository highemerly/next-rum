# Cloudflare Transform Rules — Server-Timing (cache visibility)

`cf-cache-status` is a response header that beacon.js **cannot read cross-origin**.
Instead, expose it (and the colo) as a same-origin-readable `Server-Timing` header,
which `PerformanceNavigationTiming.serverTiming[]` makes available to the page.

Apply this **per service zone**: `handon.club`, `hiyoko.club`, `poyo.me`, `anypost.dev`.

## Rule: add response header `Server-Timing`

Dashboard → Rules → Transform Rules → Modify Response Header → Create rule.

- When incoming requests match: `Hostname` equals the service host (or `All incoming requests` for the whole zone)
- Then: **Set static**? No — use **Set dynamic** so the values come from Cloudflare fields.

Header name: `Server-Timing`

Value (dynamic expression):

```
concat("cf-cache;desc=\"", cf.cache_status, "\", cf-colo;desc=\"", cf.colo.id, "\"")
```

## Verify

```bash
curl -sI https://handon.club/ | grep -i server-timing
# Server-Timing: cf-cache;desc="HIT", cf-colo;desc="NRT"
```

beacon.js reads `cf-cache` → `cf_cache` label and `cf-colo` → `cf_colo` (Loki only).

---

# Visitor geo: country and ASN (rum.piyo.me zone only)

These are read **server-side** by the collector from request headers that
Cloudflare adds at its edge — the client IP is never sent to or stored by the
collector. Apply on the **`rum.piyo.me` zone** (where `/collect` is served).

## Country (`CF-IPCountry`) — default header, no rule needed

`CF-IPCountry` (2-letter ISO 3166-1 alpha-2) is added automatically when
**Network → IP Geolocation** is ON for the zone (default on for proxied zones).
The collector reads it as `country` (special values: `XX`→`unknown`, `T1`→`tor`).

```bash
curl -sI https://rum.piyo.me/healthz | grep -i cf-ipcountry  # via the edge
```

## ASN (`HAN-CF-ASN`) — needs a Transform Rule

The AS number is **not** a default origin header, so inject it.
Dashboard → Rules → Transform Rules → **Modify Request Header** → Create rule.

- When incoming requests match: `Hostname` equals `rum.piyo.me`
- Then: **Set dynamic** header

Header name: `HAN-CF-ASN`

Value (dynamic expression):

```
to_string(ip.src.asnum)
```

(If `to_string` is rejected by the expression editor, use `concat("", ip.src.asnum)`.)

The collector reads numeric ASN only (any `AS` prefix is stripped; non-numeric →
`unknown`). AS **organization name** is not available as a Rules field — only via
a Worker's `request.cf.asOrganization` — so the metric carries the number.

## Header names are configurable

The collector's header names default to `CF-IPCountry` / `HAN-CF-ASN` but are
overridable via env, so you can rename the CF Transform Rule header later without
a code change — just keep the two in sync:

```
GEO_COUNTRY_HEADER=CF-IPCountry
GEO_ASN_HEADER=HAN-CF-ASN
```

Metrics produced: `rum_country_total{service,country}` and
`rum_asn_total{service,asn}` (one increment per page load); `country`/`asn` also
go to the Loki navigation log line.

---

## Future Work (Phase 2): NEL / Reporting endpoints

Cloudflare auto-injects its own NEL `report-to`. To collect NEL ourselves, disable
it via the zone API and set our own endpoints with Transform Rules:

```
PATCH /zones/{zone_id}/settings/nel   {"value":{"enabled":false}}
```

```
Reporting-Endpoints: default="https://rum.piyo.me/report"
Report-To: {"group":"default","max_age":2592000,"endpoints":[{"url":"https://rum.piyo.me/report"}]}
NEL: {"report_to":"default","max_age":2592000,"failure_fraction":1.0,"success_fraction":0.0}
```

The collector would then implement `POST /report` (`application/reports+json`).
Out of scope for Phase 1.
