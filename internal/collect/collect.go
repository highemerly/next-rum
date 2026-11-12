// Package collect implements POST /collect: it parses a single RUM event,
// validates client-supplied labels against rum-config.json, records bounded
// metrics, and pushes a detailed line to Loki. Client IP is never read or stored.
package collect

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/highemerly/rum-collector/internal/config"
	"github.com/highemerly/rum-collector/internal/loki"
	"github.com/highemerly/rum-collector/internal/metrics"
)

const maxBodyBytes = 64 << 10 // 64 KiB; a batched beacon holds a handful of events

// event is the union of fields beacon.js may send for any kind.
type event struct {
	Kind      string `json:"kind"`
	Service   string `json:"service"`
	PathGroup string `json:"path_group"`
	Path      string `json:"path"`
	Device    string `json:"device"`
	OS        string `json:"os"`
	Browser   string `json:"browser"`
	Conn      string `json:"conn"`
	TS        int64  `json:"ts"` // unix milliseconds
	NavType   string `json:"nav_type"`

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
	TransferSize float64 `json:"transfer_size"`
	EncodedSize  float64 `json:"encoded_size"`
	DecodedSize  float64 `json:"decoded_size"`
	Protocol     string  `json:"protocol"`
	CFCache      string  `json:"cf_cache"`
	CFColo       string  `json:"cf_colo"`
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
	country := sanitizeCountry(r.Header.Get(h.countryHeader))
	asn := sanitizeASN(r.Header.Get(h.asnHeader))

	cfg := h.cfg.Get()
	for i := range events {
		h.record(cfg, events[i], country, asn)
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
// country and asn are the per-request geo signals (already sanitized).
func (h *Handler) record(cfg *config.Config, ev event, country, asn string) {
	service, svcRejected := cfg.ValidateService(ev.Service)
	pathGroup, pgRejected := cfg.ValidatePathGroup(service, ev.PathGroup)
	device := sanitizeDevice(ev.Device)
	navType := sanitizeNavType(ev.NavType)
	os := sanitizeOS(ev.OS)
	browser := sanitizeBrowser(ev.Browser)

	status := "ok"
	if svcRejected || pgRejected {
		status = "label_rejected"
	}

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
		h.pushLoki(service, "web_vitals", ev.TS, map[string]any{
			"service": service, "kind": "web_vitals", "metric": ev.Name,
			"value": ev.Value, "rating": rating,
			"path_group": pathGroup, "path": ev.Path,
			"metric_id": ev.MetricID, "nav_type": navType,
			"device": device, "os": os, "browser": browser, "conn": ev.Conn,
		})

	case "navigation":
		protocol := sanitizeProtocol(ev.Protocol)
		cfCache := sanitizeCFCache(ev.CFCache)
		h.mx.ObserveNav(metrics.NavLabels{
			Service:   service,
			PathGroup: pathGroup,
			Device:    device,
			Protocol:  protocol,
			CFCache:   cfCache,
		}, ev.DNSMs, ev.TCPMs, ev.TLSMs, ev.RequestMs, ev.ResponseMs, ev.TransferSize)
		h.mx.IncCFCache(service, pathGroup, cfCache, protocol, device)
		// One navigation == one page load: carry the OS/browser/geo distribution
		// here, each in its own counter to keep cardinality isolated.
		h.mx.IncPageview(service, pathGroup, device, os, browser, navType)
		h.mx.IncCountry(service, country)
		h.mx.IncASN(service, asn)
		h.pushLoki(service, "navigation", ev.TS, map[string]any{
			"service": service, "kind": "navigation",
			"path_group": pathGroup, "path": ev.Path,
			"dns_ms": ev.DNSMs, "tcp_ms": ev.TCPMs, "tls_ms": ev.TLSMs,
			"request_ms": ev.RequestMs, "response_ms": ev.ResponseMs,
			"transfer_size": ev.TransferSize, "encoded_size": ev.EncodedSize,
			"decoded_size": ev.DecodedSize, "protocol": protocol,
			"cf_cache": cfCache, "cf_colo": ev.CFColo,
			"device": device, "os": os, "browser": browser, "conn": ev.Conn,
			"country": country, "asn": asn,
		})

	case "soft_navigation":
		// A client-side route change in a SPA (history.pushState / popstate):
		// no document load happened, so there is no Navigation Timing or cache
		// status to record — only the screen view. Counted as a pageview with
		// nav_type="soft" (set here, not taken from the client) so panels can
		// tell route changes from document loads. country/asn stay per document
		// load, so they are not incremented again for each route change.
		h.mx.IncPageview(service, pathGroup, device, os, browser, navTypeSoft)
		h.pushLoki(service, "soft_navigation", ev.TS, map[string]any{
			"service": service, "kind": "soft_navigation",
			"path_group": pathGroup, "path": ev.Path, "nav_type": navTypeSoft,
			"device": device, "os": os, "browser": browser, "conn": ev.Conn,
			"country": country, "asn": asn,
		})

	default:
		recorded = false
	}

	if !recorded {
		status = "parse_error"
	}
	h.mx.IncReceived(eventKind(ev.Kind), status)
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
	case "web_vitals", "navigation", "soft_navigation":
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
