package collect

import (
	"encoding/json"
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
  "features": { "hosteka": ["referrer", "auth"] }
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
	  {"kind":"navigation","service":"hosteka","path":"/events/1","path_group":"event","device":"mobile","nav_type":"navigate","referrer":"www.google.co.jp"},
	  {"kind":"navigation","service":"hosteka","path":"/events/2","path_group":"event","device":"mobile","nav_type":"navigate","referrer":"Alice.Example"},
	  {"kind":"navigation","service":"hosteka","path":"/events/3","path_group":"event","device":"mobile","nav_type":"navigate","referrer":"handon.club"},
	  {"kind":"navigation","service":"hosteka","path":"/events/4","path_group":"event","device":"mobile","nav_type":"navigate","referrer":"hosteka.piyo.me"},
	  {"kind":"navigation","service":"hosteka","path":"/events/5","path_group":"event","device":"mobile","nav_type":"navigate"},
	  {"kind":"navigation","service":"hosteka","path":"/events/6","path_group":"event","device":"mobile","nav_type":"navigate","referrer":"bad host/x"},
	  {"kind":"navigation","service":"hosteka","path":"/events/7","path_group":"event","device":"mobile","nav_type":"reload","referrer":"www.google.co.jp"}
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

	// The Loki line keeps the normalized host next to the label.
	want := map[string][2]any{ // path -> {referrer, referrer_host}
		"/events/1": {"search", "www.google.co.jp"},
		"/events/2": {"other", "alice.example"},
		"/events/3": {"other", "handon.club"},
		"/events/4": {"internal", "hosteka.piyo.me"},
		"/events/5": {"direct", nil},
		"/events/6": {"other", nil}, // not a host name: never stored
		"/events/7": {nil, nil},     // reload: no referrer at all
	}
	got := lines()
	if len(got) != len(want) {
		t.Fatalf("loki lines = %d, want %d", len(got), len(want))
	}
	for _, line := range got {
		path, _ := line["path"].(string)
		w, ok := want[path]
		if !ok {
			t.Errorf("unexpected loki line for %q", path)
			continue
		}
		if line["referrer"] != w[0] || line["referrer_host"] != w[1] {
			t.Errorf("%s: referrer=%v referrer_host=%v, want %v %v", path, line["referrer"], line["referrer_host"], w[0], w[1])
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
