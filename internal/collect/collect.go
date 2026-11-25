// Package collect implements POST /collect: it parses a single RUM event,
// validates client-supplied labels against rum-config.json, records bounded
// metrics, and pushes a detailed line to Loki. Client IP is never read or stored.
//
// A request from a browser with Global Privacy Control (the Sec-GPC: 1 header,
// or "gpc": true in the event) is reduced to page-view counts and performance
// histograms, and only for services that opted into gpc_counts; otherwise it
// is dropped. Nothing from it reaches Loki, and the geo headers are not read.
package collect

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/highemerly/rum-collector/internal/config"
	"github.com/highemerly/rum-collector/internal/loki"
	"github.com/highemerly/rum-collector/internal/metrics"
)

const maxBodyBytes = 64 << 10 // 64 KiB; a batched beacon holds a handful of events

// event is the union of fields beacon.js may send for any kind. There is no
// path: only the path_group leaves the browser.
type event struct {
	Kind      string `json:"kind"`
	Service   string `json:"service"`
	PathGroup string `json:"path_group"`
	Device    string `json:"device"`
	OS        string `json:"os"`
	Browser   string `json:"browser"`
	Conn      string `json:"conn"`
	TS        int64  `json:"ts"` // unix milliseconds
	NavType   string `json:"nav_type"`

	// GPC marks an event from a browser with Global Privacy Control, which
	// sends only the fields the GPC path records. The Sec-GPC header has the
	// same effect.
	GPC bool `json:"gpc"`

	// Opt-in per service (rum-config.json "features"); ignored otherwise.
	Referrer     string       `json:"referrer"`      // host of document.referrer (navigation only)
	Auth         string       `json:"auth"`          // "in" / "out" from <html data-rum-auth>
	Viewport     string       `json:"viewport"`      // viewport width bucket (navigation only)
	BrowserMajor string       `json:"browser_major"` // navigation only
	OSMajor      string       `json:"os_major"`      // navigation only
	UTMSource    string       `json:"utm_source"`    // navigation only
	UTMMedium    string       `json:"utm_medium"`
	UTMCampaign  string       `json:"utm_campaign"`
	Attribution  *attribution `json:"attribution"` // web_vitals only
	VisibleMs    float64      `json:"visible_ms"`  // engagement: visible time since the previous report
	TotalMs      float64      `json:"total_ms"`    // engagement: the page view's visible time so far

	// web_vitals
	Name     string  `json:"name"`
	Value    float64 `json:"value"`
	Rating   string  `json:"rating"`
	MetricID string  `json:"metric_id"`

	// navigation
	DNSMs        float64 `json:"dns_ms"`
	TCPMs        float64 `json:"tcp_ms"`
	TLSMs        float64 `json:"tls_ms"`
	RequestMs    float64 `json:"request_ms"`
	ResponseMs   float64 `json:"response_ms"`
	DCLMs        float64 `json:"dcl_ms"`  // navigation start to DOMContentLoaded end; 0 = not reached
	LoadMs       float64 `json:"load_ms"` // navigation start to load event end; 0 = not reached
	TransferSize float64 `json:"transfer_size"`
	EncodedSize  float64 `json:"encoded_size"`
	DecodedSize  float64 `json:"decoded_size"`
	Protocol     string  `json:"protocol"`
	CFCache      string  `json:"cf_cache"`
	CFColo       string  `json:"cf_colo"`
}

// attribution is the Web Vitals breakdown from the web-vitals attribution
// build, reduced by beacon.js. Which fields apply depends on the metric.
type attribution struct {
	Target         string  `json:"target"`            // LCP/INP/CLS: reduced CSS selector
	LCPType        string  `json:"lcp_type"`          // LCP: resource / text
	TTFB           float64 `json:"ttfb"`              // LCP/FCP
	LoadDelay      float64 `json:"load_delay"`        // LCP
	LoadDuration   float64 `json:"load_duration"`     // LCP
	RenderDelay    float64 `json:"render_delay"`      // LCP
	Interaction    string  `json:"interaction"`       // INP: pointer / keyboard
	InputDelay     float64 `json:"input_delay"`       // INP
	Processing     float64 `json:"processing"`        // INP
	Presentation   float64 `json:"presentation"`      // INP
	ShiftValue     float64 `json:"shift_value"`       // CLS
	FirstByteToFCP float64 `json:"first_byte_to_fcp"` // FCP
	Waiting        float64 `json:"waiting"`           // TTFB
	Cache          float64 `json:"cache"`             // TTFB
	DNS            float64 `json:"dns"`               // TTFB
	Connection     float64 `json:"connection"`        // TTFB
	Request        float64 `json:"request"`           // TTFB
	LoadState      string  `json:"load_state"`        // INP/CLS/FCP
}

// Default header names for the Cloudflare-derived geo signals. Both are
// overridable (see Options) because the CF/Traefik header naming may change.
const (
	DefaultCountryHeader = "CF-IPCountry" // CF default header
	DefaultASNHeader     = "HAN-CF-ASN"   // set via a CF Transform Rule (ip.src.asnum)
)

// Options configures the collect handler.
type Options struct {
	CountryHeader string // request header carrying the 2-letter country code
	ASNHeader     string // request header carrying the AS number
}

// Handler serves /collect.
type Handler struct {
	cfg           *config.Loader
	mx            *metrics.Metrics
	loki          *loki.Client
	countryHeader string
	asnHeader     string
}

// NewHandler wires the collect handler to its dependencies. Empty header names
// in opts fall back to the defaults.
func NewHandler(cfg *config.Loader, mx *metrics.Metrics, lk *loki.Client, opts Options) *Handler {
	if opts.CountryHeader == "" {
		opts.CountryHeader = DefaultCountryHeader
	}
	if opts.ASNHeader == "" {
		opts.ASNHeader = DefaultASNHeader
	}
	return &Handler{
		cfg:           cfg,
		mx:            mx,
		loki:          lk,
		countryHeader: opts.CountryHeader,
		asnHeader:     opts.ASNHeader,
	}
}

// request carries what one /collect request contributes to each of its events.
type request struct {
	gpc bool // Sec-GPC: 1
	// geo returns the visitor country and ASN. It reads the headers on first
	// use, so a request whose events all take the GPC path never reads them.
	geo func() (country, asn string)
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	start := time.Now()
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		h.mx.IncReceived("unknown", "parse_error")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// A batched beacon is a JSON array; a single event is a JSON object.
	events, err := decodeEvents(body)
	if err != nil {
		h.mx.IncReceived("unknown", "parse_error")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Geo signals come from Cloudflare's edge as request headers (country is a CF
	// default; ASN is injected by a Transform Rule). We read the coarse code only
	// — the client IP itself is never read or stored. Header names are
	// case-insensitive and configurable. One value applies to the whole batch.
	var geoRead bool
	var country, asn string
	req := request{
		gpc: strings.TrimSpace(r.Header.Get("Sec-GPC")) == "1",
		geo: func() (string, string) {
			if !geoRead {
				geoRead = true
				country = sanitizeCountry(r.Header.Get(h.countryHeader))
				asn = sanitizeASN(r.Header.Get(h.asnHeader))
			}
			return country, asn
		},
	}

	cfg := h.cfg.Get()
	for i := range events {
		h.record(cfg, events[i], req)
	}
	h.mx.ObserveParse(time.Since(start).Seconds())

	w.WriteHeader(http.StatusNoContent)
}

// decodeEvents accepts either a single event object or an array of events.
func decodeEvents(body []byte) ([]event, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var events []event
		if err := json.Unmarshal(trimmed, &events); err != nil {
			return nil, err
		}
		return events, nil
	}
	var ev event
	if err := json.Unmarshal(trimmed, &ev); err != nil {
		return nil, err
	}
	return []event{ev}, nil
}

// record validates and records one event, updating the intake meta-counter.
func (h *Handler) record(cfg *config.Config, ev event, req request) {
	service, svcRejected := cfg.ValidateService(ev.Service)
	pathGroup, pgRejected := cfg.ValidatePathGroup(service, ev.PathGroup)

	status := "ok"
	if svcRejected || pgRejected {
		status = "label_rejected"
	}

	if ev.GPC || req.gpc {
		// A rejected service is the fallback id, which never opts in.
		if svcRejected || !cfg.HasFeature(service, config.FeatureGPCCounts) {
			h.mx.IncReceived(eventKind(ev.Kind), "gpc_dropped")
			return
		}
		if st := h.recordGPC(ev, service, pathGroup); st != "ok" {
			status = st
		}
		h.mx.IncReceived(eventKind(ev.Kind), status)
		return
	}

	device := sanitizeDevice(ev.Device)
	navType := sanitizeNavType(ev.NavType)
	os := sanitizeOS(ev.OS)
	browser := sanitizeBrowser(ev.Browser)

	// Opt-in signals. A service that has not opted in gets neither the counter
	// nor the Loki field, whatever the client sends — the switch lives in the
	// config the operator controls, next to the privacy policy that names it.
	// A rejected service is the fallback id, which never opts in.
	has := func(feature string) bool { return !svcRejected && cfg.HasFeature(service, feature) }
	auth := sanitizeAuth(ev.Auth)

	recorded := true
	switch ev.Kind {
	case "web_vitals":
		rating := sanitizeRating(ev.Rating)
		ok := h.mx.ObserveWebVital(ev.Name, ev.Value, metrics.WebVitalLabels{
			Service:   service,
			PathGroup: pathGroup,
			Rating:    rating,
			Device:    device,
			NavType:   navType,
		})
		if !ok {
			recorded = false
			break
		}
		fields := map[string]any{
			"service": service, "kind": "web_vitals", "metric": ev.Name,
			"value": ev.Value, "rating": rating,
			"path_group": pathGroup,
			"metric_id":  ev.MetricID, "nav_type": navType,
			"device": device, "os": os, "browser": browser, "conn": ev.Conn,
		}
		if has(config.FeatureAttribution) && ev.Attribution != nil {
			if a := attributionFields(ev.Name, ev.Attribution); len(a) > 0 {
				fields["attribution"] = a
			}
		}
		h.pushLoki(service, "web_vitals", ev.TS, fields)

	case "navigation":
		country, asn := req.geo()
		protocol := sanitizeProtocol(ev.Protocol)
		cfCache := sanitizeCFCache(ev.CFCache)
		navLabels := metrics.NavLabels{
			Service:   service,
			PathGroup: pathGroup,
			Device:    device,
			Protocol:  protocol,
			CFCache:   cfCache,
		}
		h.mx.ObserveNav(navLabels, ev.DNSMs, ev.TCPMs, ev.TLSMs, ev.RequestMs, ev.ResponseMs, ev.TransferSize)
		dcl, load := sanitizeLoadMs(ev.DCLMs), sanitizeLoadMs(ev.LoadMs)
		h.mx.ObserveLoad(navLabels, dcl, load)
		h.mx.IncCFCache(service, pathGroup, cfCache, protocol, device)
		// One navigation == one page load: carry the OS/browser/geo distribution
		// here, each in its own counter to keep cardinality isolated.
		h.mx.IncPageview(service, pathGroup, device, os, browser, navType)
		h.mx.IncCountry(service, country)
		h.mx.IncASN(service, asn)
		fields := map[string]any{
			"service": service, "kind": "navigation",
			"path_group": pathGroup,
			"dns_ms":     ev.DNSMs, "tcp_ms": ev.TCPMs, "tls_ms": ev.TLSMs,
			"request_ms": ev.RequestMs, "response_ms": ev.ResponseMs,
			"transfer_size": ev.TransferSize, "encoded_size": ev.EncodedSize,
			"decoded_size": ev.DecodedSize, "protocol": protocol,
			"cf_cache": cfCache, "cf_colo": ev.CFColo,
			"device": device, "os": os, "browser": browser, "conn": ev.Conn,
			"country": country, "asn": asn,
		}
		// The page was hidden before these points when 0; leave them out
		// rather than log a load time of 0.
		if dcl > 0 {
			fields["dcl_ms"] = dcl
		}
		if load > 0 {
			fields["load_ms"] = load
		}
		// Only a fresh navigation says where the visitor came from: a reload or
		// back/forward keeps the referrer of the original load and would count
		// the same arrival again.
		// The metric gets one of four labels; the Loki line also keeps the host,
		// which is free there and lets "other" be broken down later.
		if has(config.FeatureReferrer) && navType == "navigate" {
			ref, host := cfg.ResolveReferrer(service, ev.Referrer)
			h.mx.IncReferrer(service, pathGroup, ref)
			fields["referrer"] = ref
			if host != "" {
				fields["referrer_host"] = host
			}
		}
		if has(config.FeatureAuth) {
			h.mx.IncPageviewAuth(service, pathGroup, auth, navType)
			fields["auth"] = auth
		}
		if has(config.FeatureViewport) {
			vp := sanitizeViewport(ev.Viewport)
			h.mx.IncViewport(service, device, vp)
			fields["viewport"] = vp
		}
		if has(config.FeatureVersions) {
			bm := sanitizeMajor(ev.BrowserMajor, maxBrowserMajor)
			om := sanitizeMajor(ev.OSMajor, maxOSMajor)
			h.mx.IncVersions(service, browser, bm, os, om)
			fields["browser_major"] = bm
			fields["os_major"] = om
		}
		// Like the referrer, campaign parameters describe the arrival, so only
		// a fresh navigation carries them.
		if has(config.FeatureUTM) && navType == "navigate" {
			for k, v := range map[string]string{
				"utm_source": ev.UTMSource, "utm_medium": ev.UTMMedium, "utm_campaign": ev.UTMCampaign,
			} {
				if v = sanitizeUTM(v); v != "" {
					fields[k] = v
				}
			}
		}
		h.pushLoki(service, "navigation", ev.TS, fields)

	case "soft_navigation":
		// A client-side route change in a SPA (history.pushState / popstate):
		// no document load happened, so there is no Navigation Timing or cache
		// status to record — only the screen view. Counted as a pageview with
		// nav_type="soft" (set here, not taken from the client) so panels can
		// tell route changes from document loads. country/asn stay per document
		// load, so they are not incremented again for each route change.
		country, asn := req.geo()
		h.mx.IncPageview(service, pathGroup, device, os, browser, navTypeSoft)
		fields := map[string]any{
			"service": service, "kind": "soft_navigation",
			"path_group": pathGroup, "nav_type": navTypeSoft,
			"device": device, "os": os, "browser": browser, "conn": ev.Conn,
			"country": country, "asn": asn,
		}
		// No referrer here: a route change inside the app has no outside source.
		if has(config.FeatureAuth) {
			h.mx.IncPageviewAuth(service, pathGroup, auth, navTypeSoft)
			fields["auth"] = auth
		}
		h.pushLoki(service, "soft_navigation", ev.TS, fields)

	case "engagement":
		// Visible time of one page view, reported at each page hide and when a
		// SPA route change ends the view. Metrics only: the sum of seconds and
		// the thresholds reached — no Loki line.
		if !has(config.FeatureEngagement) {
			h.mx.IncReceived("engagement", "feature_off")
			return
		}
		visible := clamp(ev.VisibleMs/1000, 0, maxEngagementReport)
		total := clamp(ev.TotalMs/1000, visible, maxEngagementTotal)
		h.mx.AddEngagement(service, pathGroup, visible, total)

	default:
		recorded = false
	}

	if !recorded {
		status = "parse_error"
	}
	h.mx.IncReceived(eventKind(ev.Kind), status)
}

// recordGPC records an event from a GPC browser: page-view counts and the
// performance histograms, with no visitor attribute (OS, browser, geo,
// referrer, login state, versions, viewport) and no Loki line, whatever else
// the client sent. Returns the intake status: ok, gpc_dropped (a kind GPC
// browsers do not report) or parse_error.
func (h *Handler) recordGPC(ev event, service, pathGroup string) string {
	device := sanitizeDevice(ev.Device)
	navType := sanitizeNavType(ev.NavType)
	switch ev.Kind {
	case "web_vitals":
		if !h.mx.ObserveWebVital(ev.Name, ev.Value, metrics.WebVitalLabels{
			Service:   service,
			PathGroup: pathGroup,
			Rating:    sanitizeRating(ev.Rating),
			Device:    device,
			NavType:   navType,
		}) {
			return "parse_error"
		}
		return "ok"
	case "navigation":
		h.mx.ObserveNav(metrics.NavLabels{
			Service:   service,
			PathGroup: pathGroup,
			Device:    device,
			Protocol:  sanitizeProtocol(ev.Protocol),
			CFCache:   sanitizeCFCache(ev.CFCache),
		}, ev.DNSMs, ev.TCPMs, ev.TLSMs, ev.RequestMs, ev.ResponseMs, ev.TransferSize)
		h.mx.IncPageviewGPC(service, pathGroup, navType)
		return "ok"
	case "soft_navigation":
		h.mx.IncPageviewGPC(service, pathGroup, navTypeSoft)
		return "ok"
	case "engagement":
		// Not collected from GPC browsers. beacon.js does not send it when it
		// sees GPC, but a browser may send the Sec-GPC header without exposing
		// navigator.globalPrivacyControl, and then the full beacon arrives.
		return "gpc_dropped"
	default:
		return "parse_error"
	}
}

func (h *Handler) pushLoki(service, kind string, tsMs int64, fields map[string]any) {
	ns := tsMs * int64(time.Millisecond)
	if tsMs <= 0 {
		ns = time.Now().UnixNano()
	}
	fields["ts"] = time.Unix(0, ns).UTC().Format(time.RFC3339Nano)
	line, err := json.Marshal(fields)
	if err != nil {
		return
	}
	h.loki.Push(loki.Entry{Service: service, Kind: kind, TimeNS: ns, Line: string(line)})
}

func eventKind(k string) string {
	switch k {
	case "web_vitals", "navigation", "soft_navigation", "engagement":
		return k
	default:
		return "unknown"
	}
}

// ---- label sanitizers: clamp client values to bounded sets ----

func inSet(v string, set map[string]struct{}, fallback string) string {
	if _, ok := set[v]; ok {
		return v
	}
	return fallback
}

var deviceSet = map[string]struct{}{"mobile": {}, "desktop": {}, "tablet": {}}

func sanitizeDevice(v string) string { return inSet(v, deviceSet, "unknown") }

var navTypeSet = map[string]struct{}{
	"navigate": {}, "reload": {}, "back_forward": {}, "prerender": {},
}

func sanitizeNavType(v string) string { return inSet(v, navTypeSet, "unknown") }

// navTypeSoft labels a soft_navigation pageview. It is deliberately not in
// navTypeSet: only the soft_navigation kind may produce it, so a web_vitals or
// navigation event claiming nav_type "soft" still clamps to "unknown".
const navTypeSoft = "soft"

var authSet = map[string]struct{}{"in": {}, "out": {}}

// sanitizeAuth clamps the page-declared login state; a page that did not set
// data-rum-auth (or set anything else) is "unknown".
func sanitizeAuth(v string) string { return inSet(v, authSet, "unknown") }

var ratingSet = map[string]struct{}{
	"good": {}, "needs-improvement": {}, "poor": {},
}

func sanitizeRating(v string) string { return inSet(v, ratingSet, "unknown") }

var osSet = map[string]struct{}{
	"windows": {}, "macos": {}, "ios": {}, "android": {},
	"linux": {}, "chromeos": {}, "other": {},
}

func sanitizeOS(v string) string {
	if v == "" {
		return "unknown"
	}
	return inSet(strings.ToLower(v), osSet, "other")
}

var browserSet = map[string]struct{}{
	"chrome": {}, "chromium": {}, "edge": {}, "firefox": {},
	"safari": {}, "opera": {}, "brave": {}, "samsung": {}, "other": {},
}

func sanitizeBrowser(v string) string {
	if v == "" {
		return "unknown"
	}
	return inSet(strings.ToLower(v), browserSet, "other")
}

// sanitizeCountry accepts a 2-letter ISO 3166-1 alpha-2 code from Cloudflare.
// Cloudflare overwrites any client-supplied value, so this mainly normalizes
// case and maps CF's special markers. Anything else clamps to "unknown".
func sanitizeCountry(v string) string {
	v = strings.ToUpper(strings.TrimSpace(v))
	switch v {
	case "", "XX": // CF: geolocation unavailable
		return "unknown"
	case "T1": // CF: Tor exit node
		return "tor"
	}
	if len(v) == 2 && v[0] >= 'A' && v[0] <= 'Z' && v[1] >= 'A' && v[1] <= 'Z' {
		return v
	}
	return "unknown"
}

// sanitizeASN accepts a numeric AS number (optionally "AS"-prefixed). A 32-bit
// ASN is at most 10 digits; anything non-numeric or longer clamps to "unknown".
func sanitizeASN(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(strings.ToUpper(v), "AS")
	if v == "" || len(v) > 10 {
		return "unknown"
	}
	for i := 0; i < len(v); i++ {
		if v[i] < '0' || v[i] > '9' {
			return "unknown"
		}
	}
	return v
}

var protocolSet = map[string]struct{}{
	"h2": {}, "h3": {}, "http/1.1": {}, "http/1.0": {}, "h2c": {}, "": {},
}

func sanitizeProtocol(v string) string {
	v = strings.ToLower(v)
	if r := inSet(v, protocolSet, "other"); r != "other" {
		if r == "" {
			return "unknown"
		}
		return r
	}
	return "other"
}

var cfCacheSet = map[string]struct{}{
	"HIT": {}, "MISS": {}, "EXPIRED": {}, "BYPASS": {}, "DYNAMIC": {},
	"REVALIDATED": {}, "UPDATING": {}, "STALE": {}, "NONE": {}, "UNKNOWN": {},
}

func sanitizeCFCache(v string) string {
	v = strings.ToUpper(strings.TrimSpace(v))
	if v == "" {
		return "NONE"
	}
	return inSet(v, cfCacheSet, "UNKNOWN")
}

// ---- values of the opt-in signals ----

const (
	maxLoadMs           = 120000 // a load time past 2 min is a stuck page, not a measurement
	maxBrowserMajor     = 299
	maxOSMajor          = 99
	maxEngagementReport = 3600  // seconds in one report (one visible stretch of a tab)
	maxEngagementTotal  = 86400 // seconds in one page view
	maxUTMLen           = 64
	maxSelectorLen      = 100
	maxAttributionMs    = 600000
)

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func clamp(v, lo, hi float64) float64 {
	if !finite(v) || v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// sanitizeLoadMs keeps a DOMContentLoaded / load time in range; 0 means the
// page was hidden before that point (or the value is unusable).
func sanitizeLoadMs(v float64) float64 {
	if !finite(v) || v <= 0 || v > maxLoadMs {
		return 0
	}
	return math.Round(v)
}

var viewportSet = map[string]struct{}{"0-639": {}, "640-1023": {}, "1024-1439": {}, "1440+": {}}

func sanitizeViewport(v string) string { return inSet(v, viewportSet, "unknown") }

// sanitizeMajor accepts a major version number 1..max (a metric label, so a
// client cannot mint more than max values per browser or OS); anything else is
// "unknown".
func sanitizeMajor(v string, max int) string {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 1 || n > max {
		return "unknown"
	}
	return strconv.Itoa(n)
}

var utmDisallowed = regexp.MustCompile(`[^a-z0-9._-]+`)

// sanitizeUTM keeps lower-case letters, digits and ._- of a campaign
// parameter, cut to 64 characters. beacon.js applies the same reduction, so
// this only catches a client that does not.
func sanitizeUTM(v string) string {
	v = utmDisallowed.ReplaceAllString(strings.ToLower(v), "")
	if len(v) > maxUTMLen {
		v = v[:maxUTMLen]
	}
	return v
}

var (
	selectorID         = regexp.MustCompile(`#[^>]*`)
	selectorDigits     = regexp.MustCompile(`[0-9]{4,}`)
	selectorDisallowed = regexp.MustCompile(`[^A-Za-z0-9_.>:#-]+`)
)

// sanitizeSelector reduces a web-vitals CSS selector to tag names and classes:
// an id (which often holds a post or user id) becomes a bare "#", runs of four
// or more digits become "N", characters outside a plain selector are dropped,
// and the result is cut to 100 characters. beacon.js applies the same rules.
func sanitizeSelector(v string) string {
	v = selectorID.ReplaceAllString(v, "#")
	v = selectorDigits.ReplaceAllString(v, "N")
	v = selectorDisallowed.ReplaceAllString(v, "")
	if len(v) > maxSelectorLen {
		v = v[:maxSelectorLen]
	}
	return v
}

var (
	interactionSet = map[string]struct{}{"pointer": {}, "keyboard": {}}
	loadStateSet   = map[string]struct{}{
		"loading": {}, "dom-interactive": {}, "dom-content-loaded": {}, "complete": {},
	}
	lcpTypeSet = map[string]struct{}{"resource": {}, "text": {}}
)

// attributionFields returns the Loki fields of a Web Vitals breakdown, only
// the ones that belong to the metric.
func attributionFields(metric string, a *attribution) map[string]any {
	out := map[string]any{}
	ms := func(k string, v float64) {
		if finite(v) && v >= 0 && v <= maxAttributionMs {
			out[k] = math.Round(v*10) / 10
		}
	}
	str := func(k, v string, set map[string]struct{}) {
		if _, ok := set[v]; ok {
			out[k] = v
		}
	}
	target := func() {
		if t := sanitizeSelector(a.Target); t != "" {
			out["target"] = t
		}
	}
	switch metric {
	case "LCP":
		target()
		str("lcp_type", a.LCPType, lcpTypeSet)
		ms("ttfb", a.TTFB)
		ms("load_delay", a.LoadDelay)
		ms("load_duration", a.LoadDuration)
		ms("render_delay", a.RenderDelay)
	case "INP":
		target()
		str("interaction", a.Interaction, interactionSet)
		ms("input_delay", a.InputDelay)
		ms("processing", a.Processing)
		ms("presentation", a.Presentation)
		str("load_state", a.LoadState, loadStateSet)
	case "CLS":
		target()
		if finite(a.ShiftValue) && a.ShiftValue >= 0 && a.ShiftValue <= 100 {
			out["shift_value"] = math.Round(a.ShiftValue*10000) / 10000
		}
		str("load_state", a.LoadState, loadStateSet)
	case "FCP":
		ms("ttfb", a.TTFB)
		ms("first_byte_to_fcp", a.FirstByteToFCP)
		str("load_state", a.LoadState, loadStateSet)
	case "TTFB":
		ms("waiting", a.Waiting)
		ms("cache", a.Cache)
		ms("dns", a.DNS)
		ms("connection", a.Connection)
		ms("request", a.Request)
	}
	return out
}
