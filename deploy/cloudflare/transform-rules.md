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
