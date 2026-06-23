// Package metrics defines the Prometheus collectors exported at /metrics and
// scraped by VictoriaMetrics. All labels are bounded sets (service, path_group,
// rating, device, nav_type, protocol, cf_cache status) so cardinality stays flat;
// high-cardinality detail (raw path, metric_id, cf_colo) goes to Loki instead.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Buckets per design §8.
var (
	bucketsLCP  = []float64{500, 1000, 1500, 2000, 2500, 3000, 4000, 5000, 7500, 10000}
	bucketsINP  = []float64{50, 100, 150, 200, 300, 400, 500, 750, 1000}
	bucketsCLS  = []float64{0.05, 0.1, 0.15, 0.25, 0.4, 0.6, 1.0}
	bucketsFCP  = []float64{500, 1000, 1500, 1800, 2500, 3000, 4000}
	bucketsTTFB = []float64{100, 200, 400, 600, 800, 1000, 1500, 2000, 3000}

	bucketsNavMS    = []float64{5, 10, 20, 50, 100, 200, 500, 1000, 2000}
	bucketsNavBytes = []float64{1024, 4096, 16384, 65536, 262144, 1048576, 4194304}

	bucketsParse = prometheus.DefBuckets
)

// Metrics owns every collector and registers them on a dedicated registry.
type Metrics struct {
	Registry *prometheus.Registry

	// Core Web Vitals histograms keyed by metric name (LCP/INP/CLS/FCP/TTFB).
	webVitals map[string]*prometheus.HistogramVec

	navDNS      *prometheus.HistogramVec
	navTCP      *prometheus.HistogramVec
	navTLS      *prometheus.HistogramVec
	navRequest  *prometheus.HistogramVec
	navResponse *prometheus.HistogramVec
	navTransfer *prometheus.HistogramVec

	cfCacheTotal  *prometheus.CounterVec
	pageviewTotal *prometheus.CounterVec
	countryTotal  *prometheus.CounterVec
	asnTotal      *prometheus.CounterVec

	receivedTotal  *prometheus.CounterVec
	parseSeconds   prometheus.Histogram
	lokiPushErrors prometheus.Counter
}

var (
	wvLabels       = []string{"service", "path_group", "rating", "device", "nav_type"}
	navLabels      = []string{"service", "path_group", "device", "protocol"}
	navRespLabels  = []string{"service", "path_group", "device", "protocol", "cf_cache"}
	cfCacheLabels  = []string{"service", "path_group", "status", "protocol", "device"}
	pageviewLabels = []string{"service", "path_group", "device", "os", "browser", "nav_type"}
	countryLabels  = []string{"service", "country"}
	asnLabels      = []string{"service", "asn"}
)

// New builds and registers all collectors.
func New() *Metrics {
	reg := prometheus.NewRegistry()
	fac := promauto.With(reg)

	newWV := func(name string, buckets []float64) *prometheus.HistogramVec {
		return fac.NewHistogramVec(prometheus.HistogramOpts{
			Name:    name,
			Help:    "Core Web Vitals: " + name,
			Buckets: buckets,
		}, wvLabels)
	}
	newNav := func(name, help string, buckets []float64, labels []string) *prometheus.HistogramVec {
		return fac.NewHistogramVec(prometheus.HistogramOpts{
			Name:    name,
			Help:    help,
			Buckets: buckets,
		}, labels)
	}

	m := &Metrics{
		Registry: reg,
		webVitals: map[string]*prometheus.HistogramVec{
			"LCP":  newWV("web_vitals_lcp_milliseconds", bucketsLCP),
			"INP":  newWV("web_vitals_inp_milliseconds", bucketsINP),
			"CLS":  newWV("web_vitals_cls_score", bucketsCLS),
			"FCP":  newWV("web_vitals_fcp_milliseconds", bucketsFCP),
			"TTFB": newWV("web_vitals_ttfb_milliseconds", bucketsTTFB),
		},
		navDNS:      newNav("rum_nav_dns_milliseconds", "DNS lookup time", bucketsNavMS, navLabels),
		navTCP:      newNav("rum_nav_tcp_milliseconds", "TCP connect time", bucketsNavMS, navLabels),
		navTLS:      newNav("rum_nav_tls_milliseconds", "TLS handshake time", bucketsNavMS, navLabels),
		navRequest:  newNav("rum_nav_request_milliseconds", "Request send time", bucketsNavMS, navLabels),
		navResponse: newNav("rum_nav_response_milliseconds", "Response transfer time", bucketsNavMS, navRespLabels),
		navTransfer: newNav("rum_nav_transfer_bytes", "HTML transfer size", bucketsNavBytes, navLabels),
		cfCacheTotal: fac.NewCounterVec(prometheus.CounterOpts{
			Name: "rum_cf_cache_total",
			Help: "Cloudflare cache status counts",
		}, cfCacheLabels),
		pageviewTotal: fac.NewCounterVec(prometheus.CounterOpts{
			Name: "rum_pageview_total",
			Help: "Page loads by device, OS and browser (one per navigation event)",
		}, pageviewLabels),
		// country/asn kept in their own low-dimensional counters so their
		// ~country and ~ASN cardinality never multiplies the other label sets.
		countryTotal: fac.NewCounterVec(prometheus.CounterOpts{
			Name: "rum_country_total",
			Help: "Page loads by visitor country (CF edge geo; no IP stored)",
		}, countryLabels),
		asnTotal: fac.NewCounterVec(prometheus.CounterOpts{
			Name: "rum_asn_total",
			Help: "Page loads by visitor AS number (CF edge; no IP stored)",
		}, asnLabels),
		receivedTotal: fac.NewCounterVec(prometheus.CounterOpts{
			Name: "rum_collector_received_total",
			Help: "Collected events by kind and status (ok/parse_error/label_rejected)",
		}, []string{"kind", "status"}),
		parseSeconds: fac.NewHistogram(prometheus.HistogramOpts{
			Name:    "rum_collector_parse_seconds",
			Help:    "Time to parse and record a /collect event",
			Buckets: bucketsParse,
		}),
		lokiPushErrors: fac.NewCounter(prometheus.CounterOpts{
			Name: "rum_collector_loki_push_errors_total",
			Help: "Loki push failures",
		}),
	}
	return m
}

// WebVitalLabels carries the bounded label set for a Core Web Vitals observation.
type WebVitalLabels struct {
	Service, PathGroup, Rating, Device, NavType string
}

// ObserveWebVital records one Core Web Vitals metric. Unknown metric names are
// ignored. Returns false if the metric name was not recognized.
func (m *Metrics) ObserveWebVital(name string, value float64, l WebVitalLabels) bool {
	h, ok := m.webVitals[name]
	if !ok {
		return false
	}
	h.WithLabelValues(l.Service, l.PathGroup, l.Rating, l.Device, l.NavType).Observe(value)
	return true
}

// NavLabels carries the bounded label set for navigation-timing observations.
type NavLabels struct {
	Service, PathGroup, Device, Protocol, CFCache string
}

// ObserveNav records the navigation-timing histograms for one navigation event.
func (m *Metrics) ObserveNav(l NavLabels, dns, tcp, tls, request, response, transferBytes float64) {
	m.navDNS.WithLabelValues(l.Service, l.PathGroup, l.Device, l.Protocol).Observe(dns)
	m.navTCP.WithLabelValues(l.Service, l.PathGroup, l.Device, l.Protocol).Observe(tcp)
	m.navTLS.WithLabelValues(l.Service, l.PathGroup, l.Device, l.Protocol).Observe(tls)
	m.navRequest.WithLabelValues(l.Service, l.PathGroup, l.Device, l.Protocol).Observe(request)
	m.navResponse.WithLabelValues(l.Service, l.PathGroup, l.Device, l.Protocol, l.CFCache).Observe(response)
	m.navTransfer.WithLabelValues(l.Service, l.PathGroup, l.Device, l.Protocol).Observe(transferBytes)
}

// IncCFCache increments the Cloudflare cache-status counter.
func (m *Metrics) IncCFCache(service, pathGroup, status, protocol, device string) {
	m.cfCacheTotal.WithLabelValues(service, pathGroup, status, protocol, device).Inc()
}

// IncPageview increments the per-page-load counter that carries OS/browser
// distribution (kept off the histograms to protect their cardinality).
func (m *Metrics) IncPageview(service, pathGroup, device, os, browser, navType string) {
	m.pageviewTotal.WithLabelValues(service, pathGroup, device, os, browser, navType).Inc()
}

// IncCountry increments the per-page-load counter for the visitor country.
func (m *Metrics) IncCountry(service, country string) {
	m.countryTotal.WithLabelValues(service, country).Inc()
}

// IncASN increments the per-page-load counter for the visitor AS number.
func (m *Metrics) IncASN(service, asn string) {
	m.asnTotal.WithLabelValues(service, asn).Inc()
}

// IncReceived records a /collect outcome (status = ok/parse_error/label_rejected).
func (m *Metrics) IncReceived(kind, status string) {
	m.receivedTotal.WithLabelValues(kind, status).Inc()
}

// ObserveParse records how long parsing+recording an event took, in seconds.
func (m *Metrics) ObserveParse(seconds float64) { m.parseSeconds.Observe(seconds) }

// IncLokiPushError increments the Loki push error counter.
func (m *Metrics) IncLokiPushError() { m.lokiPushErrors.Inc() }
