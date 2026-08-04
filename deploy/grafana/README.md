# Grafana dashboard

`rum-dashboard.json` — import it into Grafana (uid `rum-overview`).

## Why the histogram panels use `increase()`, not `rate()`

Every `histogram_quantile` in this dashboard rolls the buckets up with
`increase(...[1h])`. Do not "fix" it back to `rate()`.

`rate()` only sees increments *between samples inside the window*. A time series
whose first sample is already at its full value — a brand-new label combination,
a pod restart, a metric that fired once and then went quiet — looks perfectly
flat to `rate()`, which returns 0. `histogram_quantile` over all-zero buckets is
NaN, and the panel goes blank while the data is sitting right there.

This is not hypothetical: it hit both times the label set changed, and LCP is the
most exposed of all because beacon.js only flushes it on page hide, so on a
low-traffic install its counter can stay flat for a long time.

VictoriaMetrics' `increase()` counts the first sample of a new series as a rise
from zero, so it keeps reporting through all of the above. The quantile itself is
unaffected by the swap: `rate` is `increase` divided by the window, so every
bucket is scaled by the same constant and `histogram_quantile` interpolates to
exactly the same value. (Measured: both returned 1625 ms on the same series.)

Note this first-sample behaviour is a VictoriaMetrics extension — upstream
Prometheus `increase()` does not do it. The dashboard targets VM.

## Panels that are deliberately fleet-wide

`rum_collector_received_total` carries no `service` label, and
`rum_country_total` / `rum_asn_total` carry no `path_group`, so the template
variables do not narrow those panels. Their descriptions say so.
