// Package server wires the HTTP routes for the collector: it serves the embedded
// client assets and rum-config.json with permissive CORS, accepts beacons on
// /collect with an origin allowlist, and exposes /metrics and /healthz.
package server

import (
	"net/http"
	"strconv"
	"time"

	rumcollector "github.com/highemerly/rum-collector"
	"github.com/highemerly/rum-collector/internal/config"
	"github.com/highemerly/rum-collector/internal/metrics"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Options configures the HTTP server.
type Options struct {
	Config      *config.Loader
	Metrics     *metrics.Metrics
	Collect     http.Handler
	AssetMaxAge time.Duration // Cache-Control max-age for static assets
}

// New returns the root handler mounting every route.
func New(o Options) http.Handler {
	mux := http.NewServeMux()

	// Static assets: long-cached, CORS `*` (module import requires ACAO).
	mux.HandleFunc("GET /beacon.js", func(w http.ResponseWriter, r *http.Request) {
		serveAsset(w, rumcollector.BeaconJS, "application/javascript; charset=utf-8", o.AssetMaxAge)
	})
	mux.HandleFunc("GET /web-vitals.js", func(w http.ResponseWriter, r *http.Request) {
		serveAsset(w, rumcollector.WebVitalsJS, "application/javascript; charset=utf-8", o.AssetMaxAge)
	})
	mux.HandleFunc("GET /rum-config.json", func(w http.ResponseWriter, r *http.Request) {
		// Serve the live config so client and server validate against the same bytes.
		serveAsset(w, o.Config.Get().Raw(), "application/json; charset=utf-8", o.AssetMaxAge)
	})

	// Beacon intake: origin allowlist derived from configured service hosts.
	allow := newOriginAllowlist(o.Config)
	mux.Handle("POST /collect", corsCollect(allow, o.Collect))
	mux.Handle("OPTIONS /collect", corsPreflight(allow))

	// Internal scrape endpoint (excluded from the public IngressRoute).
	mux.Handle("GET /metrics", promhttp.HandlerFor(o.Metrics.Registry, promhttp.HandlerOpts{}))

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	return mux
}

func serveAsset(w http.ResponseWriter, body []byte, contentType string, maxAge time.Duration) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if maxAge > 0 {
		w.Header().Set("Cache-Control", "public, max-age="+strconv.Itoa(int(maxAge.Seconds())))
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// originAllowlist holds the set of origins permitted to POST /collect.
type originAllowlist struct {
	cfg *config.Loader
}

func newOriginAllowlist(cfg *config.Loader) *originAllowlist {
	return &originAllowlist{cfg: cfg}
}

func (a *originAllowlist) allowed(origin string) bool {
	if origin == "" {
		return false
	}
	for _, o := range a.cfg.Get().AllowedOrigins() {
		if o == origin {
			return true
		}
	}
	return false
}

func corsCollect(allow *originAllowlist, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); allow.allowed(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
		}
		next.ServeHTTP(w, r)
	})
}

func corsPreflight(allow *originAllowlist) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if allow.allowed(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			w.Header().Set("Access-Control-Max-Age", "86400")
			w.Header().Set("Vary", "Origin")
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
