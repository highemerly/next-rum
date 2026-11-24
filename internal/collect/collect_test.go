package collect

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/highemerly/rum-collector/internal/config"
	"github.com/highemerly/rum-collector/internal/loki"
	"github.com/highemerly/rum-collector/internal/metrics"
	dto "github.com/prometheus/client_model/go"
)

const cfgJSON = `{
  "services": { "handon.club": "handon" },
  "default_service": "unknown",
  "default_path_group": "other",
  "path_groups": {
    "handon": [ { "pattern": "/web/**", "group": "web_ui" } ]
  }
}`

func newTestHandler(t *testing.T) (*Handler, *metrics.Metrics) {
	t.Helper()
	path := t.TempDir() + "/rum-config.json"
	if err := os.WriteFile(path, []byte(cfgJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	loader, err := config.NewLoader(path)
	if err != nil {
		t.Fatal(err)
	}
	mx := metrics.New()
	lk := loki.New(loki.Options{}) // disabled (no URL)
	return NewHandler(loader, mx, lk, Options{}), mx
}

func post(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/collect", strings.NewReader(body))
	req.Header.Set("Content-Type", "text/plain")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func counterValue(t *testing.T, mx *metrics.Metrics, name string, labels map[string]string) float64 {
	t.Helper()
	fams, err := mx.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, fam := range fams {
		if fam.GetName() != name {
			continue
		}
		for _, m := range fam.GetMetric() {
			if labelsMatch(m, labels) {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// counterSum adds up every series of a counter (counterValue returns the first match).
func counterSum(t *testing.T, mx *metrics.Metrics, name string) float64 {
	t.Helper()
	fams, err := mx.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var sum float64
	for _, fam := range fams {
		if fam.GetName() == name {
			for _, m := range fam.GetMetric() {
				sum += m.GetCounter().GetValue()
			}
		}
	}
	return sum
}

func labelsMatch(m *dto.Metric, want map[string]string) bool {
	got := map[string]string{}
	for _, lp := range m.GetLabel() {
		got[lp.GetName()] = lp.GetValue()
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

func TestCollectWebVitalsOK(t *testing.T) {
	h, mx := newTestHandler(t)
	rr := post(t, h, `{"kind":"web_vitals","service":"handon","path_group":"web_ui","name":"LCP","value":2000,"rating":"good","device":"mobile","nav_type":"navigate"}`)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}
	if v := counterValue(t, mx, "rum_collector_received_total", map[string]string{"kind": "web_vitals", "status": "ok"}); v != 1 {
		t.Errorf("received ok = %v, want 1", v)
	}
}

func TestCollectLabelRejected(t *testing.T) {
	h, mx := newTestHandler(t)
	post(t, h, `{"kind":"web_vitals","service":"EVIL","path_group":"HAXX","name":"LCP","value":99,"rating":"poor","device":"mobile","nav_type":"navigate"}`)
	if v := counterValue(t, mx, "rum_collector_received_total", map[string]string{"kind": "web_vitals", "status": "label_rejected"}); v != 1 {
		t.Errorf("label_rejected = %v, want 1", v)
	}
}

func TestCollectUnknownKindParseError(t *testing.T) {
	h, mx := newTestHandler(t)
	post(t, h, `{"kind":"garbage","service":"handon"}`)
	if v := counterValue(t, mx, "rum_collector_received_total", map[string]string{"kind": "unknown", "status": "parse_error"}); v != 1 {
		t.Errorf("parse_error = %v, want 1", v)
	}
}

func TestCollectBatchArray(t *testing.T) {
	h, mx := newTestHandler(t)
	// One batched beacon carrying a navigation event and two web_vitals.
	batch := `[
	  {"kind":"navigation","service":"handon","path_group":"web_ui","device":"desktop","os":"macos","browser":"chrome","nav_type":"navigate","protocol":"h2","cf_cache":"HIT"},
	  {"kind":"web_vitals","service":"handon","path_group":"web_ui","name":"FCP","value":800,"rating":"good","device":"desktop","nav_type":"navigate"},
	  {"kind":"web_vitals","service":"handon","path_group":"web_ui","name":"TTFB","value":120,"rating":"good","device":"desktop","nav_type":"navigate"}
	]`
	rr := post(t, h, batch)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}
	if v := counterValue(t, mx, "rum_collector_received_total", map[string]string{"kind": "navigation", "status": "ok"}); v != 1 {
		t.Errorf("navigation ok = %v, want 1", v)
	}
	if v := counterValue(t, mx, "rum_collector_received_total", map[string]string{"kind": "web_vitals", "status": "ok"}); v != 2 {
		t.Errorf("web_vitals ok = %v, want 2", v)
	}
	// Pageview counter carries OS/browser for the navigation event.
	if v := counterValue(t, mx, "rum_pageview_total", map[string]string{"service": "handon", "os": "macos", "browser": "chrome"}); v != 1 {
		t.Errorf("pageview macos/chrome = %v, want 1", v)
	}
}

func TestCollectSanitizesUnknownOSBrowser(t *testing.T) {
	h, mx := newTestHandler(t)
	post(t, h, `{"kind":"navigation","service":"handon","path_group":"web_ui","device":"desktop","os":"BeOS","browser":"NetscapeNavigator","nav_type":"navigate"}`)
	// Unknown OS/browser clamp to "other".
	if v := counterValue(t, mx, "rum_pageview_total", map[string]string{"os": "other", "browser": "other"}); v != 1 {
		t.Errorf("pageview other/other = %v, want 1", v)
	}
}

func TestCollectGeoFromHeaders(t *testing.T) {
	h, mx := newTestHandler(t)
	req := httptest.NewRequest(http.MethodPost, "/collect",
		strings.NewReader(`{"kind":"navigation","service":"handon","path_group":"web_ui","device":"desktop","nav_type":"navigate"}`))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("CF-IPCountry", "jp")   // lowercase -> normalized to JP
	req.Header.Set("HAN-CF-ASN", "AS2516") // AS-prefixed -> 2516
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}
	if v := counterValue(t, mx, "rum_country_total", map[string]string{"service": "handon", "country": "JP"}); v != 1 {
		t.Errorf("country JP = %v, want 1", v)
	}
	if v := counterValue(t, mx, "rum_asn_total", map[string]string{"service": "handon", "asn": "2516"}); v != 1 {
		t.Errorf("asn 2516 = %v, want 1", v)
	}
}

func TestSanitizeCountry(t *testing.T) {
	cases := map[string]string{
		"JP": "JP", "jp": "JP", "": "unknown",
		"XX": "unknown", "T1": "tor", "USA": "unknown", "1A": "unknown",
	}
	for in, want := range cases {
		if got := sanitizeCountry(in); got != want {
			t.Errorf("sanitizeCountry(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeASN(t *testing.T) {
	cases := map[string]string{
		"2516": "2516", "AS2516": "2516", "as13335": "13335",
		"": "unknown", "12.3": "unknown", "99999999999": "unknown", "abc": "unknown",
	}
	for in, want := range cases {
		if got := sanitizeASN(in); got != want {
			t.Errorf("sanitizeASN(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCollectRejectsGET(t *testing.T) {
	h, _ := newTestHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/collect", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %d, want 405", rr.Code)
	}
}

func TestCollectSoftNavigation(t *testing.T) {
	h, mx := newTestHandler(t)
	req := httptest.NewRequest(http.MethodPost, "/collect",
		strings.NewReader(`{"kind":"soft_navigation","service":"handon","path_group":"web_ui","path":"/web/home","device":"desktop","os":"macos","browser":"chrome","nav_type":"soft"}`))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("CF-IPCountry", "JP")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}
	if v := counterValue(t, mx, "rum_collector_received_total", map[string]string{"kind": "soft_navigation", "status": "ok"}); v != 1 {
		t.Errorf("soft_navigation ok = %v, want 1", v)
	}
	if v := counterValue(t, mx, "rum_pageview_total", map[string]string{"service": "handon", "path_group": "web_ui", "nav_type": "soft", "os": "macos", "browser": "chrome"}); v != 1 {
		t.Errorf("pageview soft = %v, want 1", v)
	}
	// A route change is not a document load: no Navigation Timing, cache status
	// or geo counts.
	for _, name := range []string{"rum_cf_cache_total", "rum_country_total", "rum_asn_total"} {
		if v := counterValue(t, mx, name, nil); v != 0 {
			t.Errorf("%s = %v, want 0", name, v)
		}
	}
}

func TestCollectSoftNavTypeOnlyFromSoftNavigation(t *testing.T) {
	h, mx := newTestHandler(t)
	// nav_type "soft" is produced server-side for soft_navigation only; a
	// navigation event claiming it clamps to "unknown".
	post(t, h, `{"kind":"navigation","service":"handon","path_group":"web_ui","device":"desktop","nav_type":"soft"}`)
	if v := counterValue(t, mx, "rum_pageview_total", map[string]string{"nav_type": "unknown"}); v != 1 {
		t.Errorf("pageview unknown = %v, want 1", v)
	}
	if v := counterValue(t, mx, "rum_pageview_total", map[string]string{"nav_type": "soft"}); v != 0 {
		t.Errorf("pageview soft = %v, want 0", v)
	}
}

const featuresCfgJSON = `{
  "services": { "hosteka.piyo.me": "hosteka", "handon.club": "handon" },
  "default_service": "unknown",
  "default_path_group": "other",
  "path_groups": {
    "hosteka": [ { "pattern": "/events/*", "group": "event" } ],
    "handon":  [ { "pattern": "/web/**", "group": "web_ui" } ]
  },
  "features": {
    "hosteka": ["referrer", "auth", "attribution", "versions", "viewport", "utm", "engagement", "gpc_counts"]
  }
}`

func newFeaturesHandler(t *testing.T) (*Handler, *metrics.Metrics) {
	t.Helper()
	return newFeaturesHandlerWithLoki(t, loki.New(loki.Options{}))
}

func newFeaturesHandlerWithLoki(t *testing.T, lk *loki.Client) (*Handler, *metrics.Metrics) {
	t.Helper()
	path := t.TempDir() + "/rum-config.json"
	if err := os.WriteFile(path, []byte(featuresCfgJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	loader, err := config.NewLoader(path)
	if err != nil {
		t.Fatal(err)
	}
	mx := metrics.New()
	return NewHandler(loader, mx, lk, Options{}), mx
}

// captureLoki starts a fake Loki push endpoint. Call the returned function
// after the posts: it drains the client and returns the JSON lines received.
func captureLoki(t *testing.T) (*loki.Client, func() []map[string]any) {
	t.Helper()
	var (
		mu    sync.Mutex
		lines []map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Streams []struct {
				Values [][2]string `json:"values"`
			} `json:"streams"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode loki push: %v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		for _, st := range body.Streams {
			for _, v := range st.Values {
				var line map[string]any
				if err := json.Unmarshal([]byte(v[1]), &line); err != nil {
					t.Errorf("decode loki line: %v", err)
				}
				lines = append(lines, line)
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	lk := loki.New(loki.Options{URL: srv.URL})
	return lk, func() []map[string]any {
		lk.Close()
		mu.Lock()
		defer mu.Unlock()
		return lines
	}
}

func TestCollectReferrerOnFreshNavigation(t *testing.T) {
	lk, lines := captureLoki(t)
	h, mx := newFeaturesHandlerWithLoki(t, lk)
	post(t, h, `[
	  {"kind":"navigation","service":"hosteka","ts":1000,"path_group":"event","device":"mobile","nav_type":"navigate","referrer":"www.google.co.jp"},
	  {"kind":"navigation","service":"hosteka","ts":2000,"path_group":"event","device":"mobile","nav_type":"navigate","referrer":"Alice.Example"},
	  {"kind":"navigation","service":"hosteka","ts":3000,"path_group":"event","device":"mobile","nav_type":"navigate","referrer":"handon.club"},
	  {"kind":"navigation","service":"hosteka","ts":4000,"path_group":"event","device":"mobile","nav_type":"navigate","referrer":"hosteka.piyo.me"},
	  {"kind":"navigation","service":"hosteka","ts":5000,"path_group":"event","device":"mobile","nav_type":"navigate"},
	  {"kind":"navigation","service":"hosteka","ts":6000,"path_group":"event","device":"mobile","nav_type":"navigate","referrer":"bad host/x"},
	  {"kind":"navigation","service":"hosteka","ts":7000,"path_group":"event","device":"mobile","nav_type":"reload","referrer":"www.google.co.jp"}
	]`)
	// The metric only ever carries the four labels: another of the operator's
	// services (handon) and an unparseable host are both "other".
	for ref, want := range map[string]float64{"search": 1, "other": 3, "internal": 1, "direct": 1} {
		if v := counterValue(t, mx, "rum_referrer_total", map[string]string{"service": "hosteka", "path_group": "event", "referrer": ref}); v != want {
			t.Errorf("referrer %s = %v, want %v", ref, v, want)
		}
	}
	// The reload keeps the original referrer; counting it would count the arrival twice.
	if v := counterSum(t, mx, "rum_referrer_total"); v != 6 {
		t.Errorf("referrer total = %v, want 6 (reload not counted)", v)
	}

	// The Loki line keeps the normalized host next to the label. Lines are
	// told apart by their ts (the beacon's, in seconds here).
	want := map[string][2]any{ // ts -> {referrer, referrer_host}
		"1970-01-01T00:00:01Z": {"search", "www.google.co.jp"},
		"1970-01-01T00:00:02Z": {"other", "alice.example"},
		"1970-01-01T00:00:03Z": {"other", "handon.club"},
		"1970-01-01T00:00:04Z": {"internal", "hosteka.piyo.me"},
		"1970-01-01T00:00:05Z": {"direct", nil},
		"1970-01-01T00:00:06Z": {"other", nil}, // not a host name: never stored
		"1970-01-01T00:00:07Z": {nil, nil},     // reload: no referrer at all
	}
	got := lines()
	if len(got) != len(want) {
		t.Fatalf("loki lines = %d, want %d", len(got), len(want))
	}
	for _, line := range got {
		ts, _ := line["ts"].(string)
		w, ok := want[ts]
		if !ok {
			t.Errorf("unexpected loki line for ts %q", ts)
			continue
		}
		if line["referrer"] != w[0] || line["referrer_host"] != w[1] {
			t.Errorf("%s: referrer=%v referrer_host=%v, want %v %v", ts, line["referrer"], line["referrer_host"], w[0], w[1])
		}
	}
}

func TestCollectAuthOnPageviews(t *testing.T) {
	h, mx := newFeaturesHandler(t)
	post(t, h, `[
	  {"kind":"navigation","service":"hosteka","path_group":"event","device":"mobile","nav_type":"navigate","auth":"in"},
	  {"kind":"soft_navigation","service":"hosteka","path_group":"event","path":"/events/1","device":"mobile","auth":"out"},
	  {"kind":"soft_navigation","service":"hosteka","path_group":"event","path":"/events/2","device":"mobile","auth":"admin"},
	  {"kind":"web_vitals","service":"hosteka","path_group":"event","name":"LCP","value":1000,"rating":"good","device":"mobile","nav_type":"navigate","auth":"in"}
	]`)
	cases := []struct {
		auth, navType string
		want          float64
	}{
		{"in", "navigate", 1},
		{"out", "soft", 1},
		{"unknown", "soft", 1}, // anything but in/out clamps
	}
	for _, tc := range cases {
		if v := counterValue(t, mx, "rum_pageview_auth_total", map[string]string{"service": "hosteka", "auth": tc.auth, "nav_type": tc.navType}); v != tc.want {
			t.Errorf("auth %s/%s = %v, want %v", tc.auth, tc.navType, v, tc.want)
		}
	}
	// web_vitals is not a page view.
	if v := counterSum(t, mx, "rum_pageview_auth_total"); v != 3 {
		t.Errorf("auth total = %v, want 3", v)
	}
}

func TestCollectOptInSignalsIgnoredWithoutFeature(t *testing.T) {
	h, mx := newFeaturesHandler(t)
	// handon has not opted in, and EVIL is rejected to the fallback service:
	// whatever the client sends, neither counter moves.
	post(t, h, `[
	  {"kind":"navigation","service":"handon","path_group":"web_ui","device":"desktop","nav_type":"navigate","referrer":"mastodon.social","auth":"in"},
	  {"kind":"soft_navigation","service":"handon","path_group":"web_ui","path":"/web/x","device":"desktop","auth":"in"},
	  {"kind":"navigation","service":"EVIL","path_group":"x","device":"desktop","nav_type":"navigate","referrer":"mastodon.social","auth":"in"}
	]`)
	for _, name := range []string{"rum_referrer_total", "rum_pageview_auth_total"} {
		if v := counterSum(t, mx, name); v != 0 {
			t.Errorf("%s = %v, want 0", name, v)
		}
	}
}

func TestSanitizeAuth(t *testing.T) {
	cases := map[string]string{"in": "in", "out": "out", "": "unknown", "IN": "unknown", "admin": "unknown"}
	for in, want := range cases {
		if got := sanitizeAuth(in); got != want {
			t.Errorf("sanitizeAuth(%q) = %q, want %q", in, got, want)
		}
	}
}

// histogramCount returns the sample count of the first histogram series of
// name whose labels include labels (0 when there is none).
func histogramCount(t *testing.T, mx *metrics.Metrics, name string, labels map[string]string) uint64 {
	t.Helper()
	fams, err := mx.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var n uint64
	for _, fam := range fams {
		if fam.GetName() != name {
			continue
		}
		for _, m := range fam.GetMetric() {
			if labelsMatch(m, labels) {
				n += m.GetHistogram().GetSampleCount()
			}
		}
	}
	return n
}

// postWith posts body with extra request headers.
func postWith(t *testing.T, h *Handler, headers map[string]string, body string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/collect", strings.NewReader(body))
	req.Header.Set("Content-Type", "text/plain")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	h.ServeHTTP(httptest.NewRecorder(), req)
}

func TestCollectNeverLogsPath(t *testing.T) {
	lk, lines := captureLoki(t)
	h, _ := newFeaturesHandlerWithLoki(t, lk)
	// An older beacon.js (cached by a browser) may still send the path.
	post(t, h, `[
	  {"kind":"navigation","service":"hosteka","path":"/events/1","path_group":"event","device":"mobile","nav_type":"navigate"},
	  {"kind":"soft_navigation","service":"hosteka","path":"/events/2","path_group":"event","device":"mobile"},
	  {"kind":"web_vitals","service":"hosteka","path":"/events/1","path_group":"event","name":"LCP","value":900,"rating":"good","device":"mobile","nav_type":"navigate"}
	]`)
	got := lines()
	if len(got) != 3 {
		t.Fatalf("loki lines = %d, want 3", len(got))
	}
	for _, line := range got {
		if _, ok := line["path"]; ok {
			t.Errorf("%v line carries path %v", line["kind"], line["path"])
		}
	}
}

// fullBatch is what a beacon sends from a browser without GPC; the GPC tests
// post it with Sec-GPC (a browser that sends the header but does not expose
// navigator.globalPrivacyControl) to show the collector keeps only what the
// GPC path allows.
const fullBatch = `[
  {"kind":"navigation","service":"%s","path_group":"%s","device":"mobile","os":"ios","browser":"safari","conn":"4g","nav_type":"navigate","dns_ms":3,"tcp_ms":10,"tls_ms":12,"request_ms":80,"response_ms":20,"dcl_ms":900,"load_ms":1500,"transfer_size":9000,"protocol":"h3","cf_cache":"HIT","cf_colo":"NRT","referrer":"www.google.com","auth":"in","viewport":"0-639","browser_major":"26","os_major":"26","utm_source":"news"},
  {"kind":"web_vitals","service":"%s","path_group":"%s","name":"LCP","value":1200,"rating":"good","device":"mobile","os":"ios","browser":"safari","nav_type":"navigate","metric_id":"v4-1","attribution":{"target":"img.hero"}},
  {"kind":"soft_navigation","service":"%s","path_group":"%s","device":"mobile","os":"ios","browser":"safari","auth":"in"},
  {"kind":"engagement","service":"%s","path_group":"%s","device":"mobile","visible_ms":40000,"total_ms":40000}
]`

func batchFor(service, group string) string {
	args := make([]any, 0, 8)
	for i := 0; i < 4; i++ {
		args = append(args, service, group)
	}
	return fmt.Sprintf(fullBatch, args...)
}

// attributeCounters are the counters a GPC request must never move.
var attributeCounters = []string{
	"rum_pageview_total", "rum_country_total", "rum_asn_total", "rum_cf_cache_total",
	"rum_referrer_total", "rum_pageview_auth_total", "rum_viewport_total",
	"rum_browser_version_total", "rum_os_version_total",
	"rum_engagement_seconds_total", "rum_engagement_reached_total",
}

func TestCollectGPCReducedToCounts(t *testing.T) {
	lk, lines := captureLoki(t)
	h, mx := newFeaturesHandlerWithLoki(t, lk)
	postWith(t, h, map[string]string{"Sec-GPC": "1", "CF-IPCountry": "JP", "HAN-CF-ASN": "2516"}, batchFor("hosteka", "event"))

	for nav, want := range map[string]float64{"navigate": 1, "soft": 1} {
		if v := counterValue(t, mx, "rum_pageview_gpc_total", map[string]string{"service": "hosteka", "path_group": "event", "nav_type": nav}); v != want {
			t.Errorf("gpc pageview %s = %v, want %v", nav, v, want)
		}
	}
	// The performance histograms take the GPC samples, with the same labels.
	if n := histogramCount(t, mx, "web_vitals_lcp_milliseconds", map[string]string{"service": "hosteka", "device": "mobile", "nav_type": "navigate", "rating": "good"}); n != 1 {
		t.Errorf("LCP samples = %d, want 1", n)
	}
	if n := histogramCount(t, mx, "rum_nav_response_milliseconds", map[string]string{"service": "hosteka", "protocol": "h3", "cf_cache": "HIT"}); n != 1 {
		t.Errorf("nav response samples = %d, want 1", n)
	}
	// DOMContentLoaded / load are not part of the GPC set.
	for _, name := range []string{"rum_nav_dcl_milliseconds", "rum_nav_load_milliseconds"} {
		if n := histogramCount(t, mx, name, nil); n != 0 {
			t.Errorf("%s samples = %d, want 0", name, n)
		}
	}
	for _, name := range attributeCounters {
		if v := counterSum(t, mx, name); v != 0 {
			t.Errorf("%s = %v, want 0", name, v)
		}
	}
	if v := counterValue(t, mx, "rum_collector_received_total", map[string]string{"kind": "engagement", "status": "gpc_dropped"}); v != 1 {
		t.Errorf("engagement gpc_dropped = %v, want 1", v)
	}
	if got := lines(); len(got) != 0 {
		t.Errorf("loki lines = %d, want 0: %v", len(got), got)
	}
}

func TestCollectGPCFlagInEvent(t *testing.T) {
	h, mx := newFeaturesHandler(t)
	// What beacon.js sends when it sees navigator.globalPrivacyControl.
	post(t, h, `[
	  {"kind":"navigation","gpc":true,"service":"hosteka","path_group":"event","device":"desktop","nav_type":"navigate","protocol":"h2","cf_cache":"MISS"},
	  {"kind":"web_vitals","gpc":true,"service":"hosteka","path_group":"event","name":"TTFB","value":200,"rating":"good","device":"desktop","nav_type":"navigate"},
	  {"kind":"navigation","service":"hosteka","path_group":"event","device":"desktop","os":"windows","browser":"edge","nav_type":"navigate"}
	]`)
	if v := counterSum(t, mx, "rum_pageview_gpc_total"); v != 1 {
		t.Errorf("gpc pageviews = %v, want 1", v)
	}
	// The event without the flag in the same batch takes the normal path.
	if v := counterValue(t, mx, "rum_pageview_total", map[string]string{"os": "windows", "browser": "edge"}); v != 1 {
		t.Errorf("pageview windows/edge = %v, want 1", v)
	}
	if v := counterValue(t, mx, "rum_collector_received_total", map[string]string{"kind": "web_vitals", "status": "ok"}); v != 1 {
		t.Errorf("web_vitals ok = %v, want 1", v)
	}
}

func TestCollectGPCDroppedWithoutFeature(t *testing.T) {
	lk, lines := captureLoki(t)
	h, mx := newFeaturesHandlerWithLoki(t, lk)
	// handon has not opted into gpc_counts: a GPC browser sends nothing there.
	postWith(t, h, map[string]string{"Sec-GPC": "1"}, batchFor("handon", "web_ui"))
	post(t, h, `{"kind":"soft_navigation","gpc":true,"service":"handon","path_group":"web_ui","device":"desktop"}`)

	if v := counterSum(t, mx, "rum_pageview_gpc_total"); v != 0 {
		t.Errorf("gpc pageviews = %v, want 0", v)
	}
	for _, name := range []string{"web_vitals_lcp_milliseconds", "rum_nav_dns_milliseconds"} {
		if n := histogramCount(t, mx, name, nil); n != 0 {
			t.Errorf("%s samples = %d, want 0", name, n)
		}
	}
	for _, name := range attributeCounters {
		if v := counterSum(t, mx, name); v != 0 {
			t.Errorf("%s = %v, want 0", name, v)
		}
	}
	if v := counterValue(t, mx, "rum_collector_received_total", map[string]string{"kind": "soft_navigation", "status": "gpc_dropped"}); v != 2 {
		t.Errorf("soft_navigation gpc_dropped = %v, want 2", v)
	}
	if got := lines(); len(got) != 0 {
		t.Errorf("loki lines = %d, want 0", len(got))
	}
}

func TestCollectNewSignalsWithFeatures(t *testing.T) {
	lk, lines := captureLoki(t)
	h, mx := newFeaturesHandlerWithLoki(t, lk)
	post(t, h, `[
	  {"kind":"navigation","service":"hosteka","ts":1000,"path_group":"event","device":"mobile","os":"android","browser":"chrome","nav_type":"navigate","protocol":"h2","dcl_ms":850.4,"load_ms":1720,"viewport":"0-639","browser_major":"142","os_major":"16","utm_source":"Mastodon","utm_medium":"social post!","utm_campaign":"winter-2026"},
	  {"kind":"navigation","service":"hosteka","ts":2000,"path_group":"event","device":"desktop","os":"windows","browser":"firefox","nav_type":"reload","protocol":"h2","dcl_ms":0,"load_ms":0,"viewport":"9999","browser_major":"9999","os_major":"x","utm_source":"news"},
	  {"kind":"web_vitals","service":"hosteka","ts":3000,"path_group":"event","name":"INP","value":240,"rating":"needs-improvement","device":"mobile","nav_type":"navigate","attribution":{"target":"#status-1234567>div.content>button.icon-button","interaction":"pointer","input_delay":12.34,"processing":180,"presentation":47.7,"load_state":"complete","ttfb":5}},
	  {"kind":"web_vitals","service":"hosteka","ts":4000,"path_group":"event","name":"LCP","value":1800,"rating":"good","device":"mobile","nav_type":"navigate","attribution":{"target":"html>body>img.hero[src=x]","lcp_type":"resource","ttfb":300,"load_delay":100,"load_duration":900,"render_delay":-5}},
	  {"kind":"engagement","service":"hosteka","path_group":"event","visible_ms":25000,"total_ms":25000},
	  {"kind":"engagement","service":"hosteka","path_group":"event","visible_ms":40000,"total_ms":65000}
	]`)

	if n := histogramCount(t, mx, "rum_nav_dcl_milliseconds", map[string]string{"service": "hosteka"}); n != 1 {
		t.Errorf("dcl samples = %d, want 1 (0 = page hidden first, skipped)", n)
	}
	if n := histogramCount(t, mx, "rum_nav_load_milliseconds", map[string]string{"service": "hosteka"}); n != 1 {
		t.Errorf("load samples = %d, want 1", n)
	}
	for _, c := range []struct {
		name   string
		labels map[string]string
	}{
		{"rum_viewport_total", map[string]string{"service": "hosteka", "device": "mobile", "viewport": "0-639"}},
		{"rum_viewport_total", map[string]string{"service": "hosteka", "device": "desktop", "viewport": "unknown"}},
		{"rum_browser_version_total", map[string]string{"service": "hosteka", "browser": "chrome", "major": "142"}},
		{"rum_browser_version_total", map[string]string{"service": "hosteka", "browser": "firefox", "major": "unknown"}},
		{"rum_os_version_total", map[string]string{"service": "hosteka", "os": "android", "major": "16"}},
		{"rum_os_version_total", map[string]string{"service": "hosteka", "os": "windows", "major": "unknown"}},
		{"rum_engagement_reached_total", map[string]string{"service": "hosteka", "path_group": "event", "ge": "10"}},
		{"rum_engagement_reached_total", map[string]string{"service": "hosteka", "path_group": "event", "ge": "30"}},
		{"rum_engagement_reached_total", map[string]string{"service": "hosteka", "path_group": "event", "ge": "60"}},
	} {
		if v := counterValue(t, mx, c.name, c.labels); v != 1 {
			t.Errorf("%s%v = %v, want 1", c.name, c.labels, v)
		}
	}
	// 25s then 40s more: each threshold counted once, 180s not reached.
	if v := counterSum(t, mx, "rum_engagement_reached_total"); v != 3 {
		t.Errorf("reached total = %v, want 3", v)
	}
	if v := counterValue(t, mx, "rum_engagement_seconds_total", map[string]string{"service": "hosteka", "path_group": "event"}); v != 65 {
		t.Errorf("engagement seconds = %v, want 65", v)
	}

	byTS := map[string]map[string]any{}
	for _, line := range lines() {
		ts, _ := line["ts"].(string)
		byTS[ts] = line
	}
	if len(byTS) != 4 {
		t.Fatalf("loki lines = %d, want 4 (engagement has none)", len(byTS))
	}
	nav := byTS["1970-01-01T00:00:01Z"]
	for k, want := range map[string]any{
		"dcl_ms": 850.0, "load_ms": 1720.0, "viewport": "0-639", "browser_major": "142", "os_major": "16",
		"utm_source": "mastodon", "utm_medium": "socialpost", "utm_campaign": "winter-2026",
	} {
		if nav[k] != want {
			t.Errorf("navigation %s = %v, want %v", k, nav[k], want)
		}
	}
	reload := byTS["1970-01-01T00:00:02Z"]
	for _, k := range []string{"dcl_ms", "load_ms", "utm_source"} {
		if _, ok := reload[k]; ok {
			t.Errorf("reload line has %s (0 load times are left out; UTM is for fresh navigations)", k)
		}
	}
	inp, _ := byTS["1970-01-01T00:00:03Z"]["attribution"].(map[string]any)
	wantINP := map[string]any{
		"target": "#>div.content>button.icon-button", "interaction": "pointer",
		"input_delay": 12.3, "processing": 180.0, "presentation": 47.7, "load_state": "complete",
	}
	if len(inp) != len(wantINP) {
		t.Errorf("INP attribution = %v, want %v (ttfb does not belong to INP)", inp, wantINP)
	}
	for k, want := range wantINP {
		if inp[k] != want {
			t.Errorf("INP attribution %s = %v, want %v", k, inp[k], want)
		}
	}
	lcp, _ := byTS["1970-01-01T00:00:04Z"]["attribution"].(map[string]any)
	if lcp["target"] != "html>body>img.herosrcx" || lcp["lcp_type"] != "resource" || lcp["load_duration"] != 900.0 {
		t.Errorf("LCP attribution = %v", lcp)
	}
	if _, ok := lcp["render_delay"]; ok {
		t.Errorf("negative render_delay kept: %v", lcp)
	}
}

func TestCollectNewSignalsIgnoredWithoutFeatures(t *testing.T) {
	lk, lines := captureLoki(t)
	h, mx := newFeaturesHandlerWithLoki(t, lk)
	post(t, h, `[
	  {"kind":"navigation","service":"handon","path_group":"web_ui","device":"mobile","nav_type":"navigate","dcl_ms":800,"load_ms":1200,"viewport":"0-639","browser_major":"142","os_major":"16","utm_source":"news"},
	  {"kind":"web_vitals","service":"handon","path_group":"web_ui","name":"CLS","value":0.2,"rating":"needs-improvement","device":"mobile","nav_type":"navigate","attribution":{"target":"div.banner","shift_value":0.18}},
	  {"kind":"engagement","service":"handon","path_group":"web_ui","visible_ms":90000,"total_ms":90000}
	]`)
	for _, name := range []string{"rum_viewport_total", "rum_browser_version_total", "rum_os_version_total", "rum_engagement_seconds_total", "rum_engagement_reached_total"} {
		if v := counterSum(t, mx, name); v != 0 {
			t.Errorf("%s = %v, want 0", name, v)
		}
	}
	// Load timing is not an opt-in signal.
	if n := histogramCount(t, mx, "rum_nav_load_milliseconds", map[string]string{"service": "handon"}); n != 1 {
		t.Errorf("load samples = %d, want 1", n)
	}
	if v := counterValue(t, mx, "rum_collector_received_total", map[string]string{"kind": "engagement", "status": "feature_off"}); v != 1 {
		t.Errorf("engagement feature_off = %v, want 1", v)
	}
	for _, line := range lines() {
		for _, k := range []string{"viewport", "browser_major", "os_major", "utm_source", "attribution"} {
			if _, ok := line[k]; ok {
				t.Errorf("%v line has %s without the feature", line["kind"], k)
			}
		}
	}
}

func TestSanitizeSelector(t *testing.T) {
	cases := map[string]string{
		"html>body>main.timeline>img":             "html>body>main.timeline>img",
		"#status-109876543>div.status__content>p": "#>div.status__content>p",
		"div.post-12345678>a":                     "div.post-N>a",
		"h1.title":                                "h1.title",
		`div.w-[10px]>span[data-x="a b"]`:         "div.w-10px>spandata-xab",
		strings.Repeat("div.a>", 30) + "span":     strings.Repeat("div.a>", 16) + "div.",
		"button.hover:bg-blue-500.focus:ring":     "button.hover:bg-blue-500.focus:ring",
		"#app":                                    "#",
	}
	for in, want := range cases {
		if got := sanitizeSelector(in); got != want {
			t.Errorf("sanitizeSelector(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeMajor(t *testing.T) {
	cases := map[string]string{"142": "142", " 26 ": "26", "0": "unknown", "300": "unknown", "1e2": "unknown", "": "unknown", "-3": "unknown"}
	for in, want := range cases {
		if got := sanitizeMajor(in, maxBrowserMajor); got != want {
			t.Errorf("sanitizeMajor(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeUTM(t *testing.T) {
	cases := map[string]string{
		"Mastodon": "mastodon", "spring_sale-2026": "spring_sale-2026", "a b/c?d": "abcd",
		"メール": "", strings.Repeat("x", 80): strings.Repeat("x", 64),
	}
	for in, want := range cases {
		if got := sanitizeUTM(in); got != want {
			t.Errorf("sanitizeUTM(%q) = %q, want %q", in, got, want)
		}
	}
}
