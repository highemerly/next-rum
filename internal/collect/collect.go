// Package collect implements POST /collect: it parses a single RUM event,
// validates client-supplied labels against rum-config.json, records bounded
// metrics, and pushes a detailed line to Loki. Client IP is never read or stored.
package collect

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/highemerly/rum-collector/internal/config"
	"github.com/highemerly/rum-collector/internal/loki"
	"github.com/highemerly/rum-collector/internal/metrics"
)

const maxBodyBytes = 16 << 10 // 16 KiB; a single event is tiny

// event is the union of fields beacon.js may send for either kind.
type event struct {
	Kind      string `json:"kind"`
	Service   string `json:"service"`
	PathGroup string `json:"path_group"`
	Path      string `json:"path"`
	Device    string `json:"device"`
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

// Handler serves /collect.
type Handler struct {
	cfg  *config.Loader
	mx   *metrics.Metrics
	loki *loki.Client
}

// NewHandler wires the collect handler to its dependencies.
func NewHandler(cfg *config.Loader, mx *metrics.Metrics, lk *loki.Client) *Handler {
	return &Handler{cfg: cfg, mx: mx, loki: lk}
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

	var ev event
	if err := json.Unmarshal(body, &ev); err != nil {
		h.mx.IncReceived("unknown", "parse_error")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	cfg := h.cfg.Get()
	service, svcRejected := cfg.ValidateService(ev.Service)
	pathGroup, pgRejected := cfg.ValidatePathGroup(service, ev.PathGroup)
	device := sanitizeDevice(ev.Device)
	navType := sanitizeNavType(ev.NavType)

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
			"device": device, "conn": ev.Conn,
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
		h.pushLoki(service, "navigation", ev.TS, map[string]any{
			"service": service, "kind": "navigation",
			"path_group": pathGroup, "path": ev.Path,
			"dns_ms": ev.DNSMs, "tcp_ms": ev.TCPMs, "tls_ms": ev.TLSMs,
			"request_ms": ev.RequestMs, "response_ms": ev.ResponseMs,
			"transfer_size": ev.TransferSize, "encoded_size": ev.EncodedSize,
			"decoded_size": ev.DecodedSize, "protocol": protocol,
			"cf_cache": cfCache, "cf_colo": ev.CFColo,
			"device": device, "conn": ev.Conn,
		})

	default:
		recorded = false
	}

	if !recorded {
		status = "parse_error"
	}
	h.mx.IncReceived(eventKind(ev.Kind), status)
	h.mx.ObserveParse(time.Since(start).Seconds())

	w.WriteHeader(http.StatusNoContent)
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
	case "web_vitals", "navigation":
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

var ratingSet = map[string]struct{}{
	"good": {}, "needs-improvement": {}, "poor": {},
}

func sanitizeRating(v string) string { return inSet(v, ratingSet, "unknown") }

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
