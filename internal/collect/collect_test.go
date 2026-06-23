package collect

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
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
	return NewHandler(loader, mx, lk), mx
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

func TestCollectRejectsGET(t *testing.T) {
	h, _ := newTestHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/collect", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %d, want 405", rr.Code)
	}
}
