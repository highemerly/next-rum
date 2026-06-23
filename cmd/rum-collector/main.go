// Command rum-collector is the RUM ingestion service. It serves the client
// assets and rum-config.json, ingests beacons on /collect, records metrics for
// VictoriaMetrics to scrape at /metrics, and pushes detailed logs to Loki.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/highemerly/rum-collector/internal/collect"
	"github.com/highemerly/rum-collector/internal/config"
	"github.com/highemerly/rum-collector/internal/loki"
	"github.com/highemerly/rum-collector/internal/metrics"
	"github.com/highemerly/rum-collector/internal/server"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	cfg := loadEnv()

	loader, err := config.NewLoader(cfg.configPath)
	if err != nil {
		slog.Error("load config", "err", err)
		os.Exit(1)
	}
	slog.Info("config loaded", "path", cfg.configPath)

	mx := metrics.New()

	lk := loki.New(loki.Options{
		URL:         cfg.lokiURL,
		Job:         "rum-collector",
		OnPushError: mx.IncLokiPushError,
	})
	defer lk.Close()

	collector := collect.NewHandler(loader, mx, lk)

	handler := server.New(server.Options{
		Config:      loader,
		Metrics:     mx,
		Collect:     collector,
		AssetMaxAge: cfg.assetMaxAge,
	})

	srv := &http.Server{
		Addr:              cfg.listenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Periodic config hot-reload so ConfigMap edits apply without a restart.
	stopReload := startConfigReload(loader, cfg.reloadInterval)
	defer stopReload()

	// Graceful shutdown on SIGINT/SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		slog.Info("listening", "addr", cfg.listenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("shutdown", "err", err)
	}
}

func startConfigReload(loader *config.Loader, interval time.Duration) func() {
	if interval <= 0 {
		return func() {}
	}
	ticker := time.NewTicker(interval)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-ticker.C:
				if err := loader.Reload(); err != nil {
					slog.Warn("config reload failed; keeping previous", "err", err)
				}
			case <-done:
				return
			}
		}
	}()
	return func() {
		ticker.Stop()
		close(done)
	}
}

type envConfig struct {
	listenAddr     string
	configPath     string
	lokiURL        string
	assetMaxAge    time.Duration
	reloadInterval time.Duration
}

func loadEnv() envConfig {
	return envConfig{
		listenAddr:     getenv("LISTEN_ADDR", ":8080"),
		configPath:     getenv("CONFIG_PATH", "assets/rum-config.json"),
		lokiURL:        os.Getenv("LOKI_URL"), // empty disables Loki push
		assetMaxAge:    getdur("ASSET_MAX_AGE_SECONDS", 300*time.Second),
		reloadInterval: getdur("CONFIG_RELOAD_SECONDS", 60*time.Second),
	}
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getdur(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		slog.Warn("invalid duration env; using default", "key", key, "value", v)
		return def
	}
	return time.Duration(n) * time.Second
}
