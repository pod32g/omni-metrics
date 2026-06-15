// Command omni is the omni-metrics server: it scrapes targets, stores samples in
// a head block backed by a write-ahead log, evaluates PromQL, and serves a
// Prometheus-compatible HTTP API plus an embedded web console.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/pod32g/omni-metrics/internal/api"
	"github.com/pod32g/omni-metrics/internal/config"
	"github.com/pod32g/omni-metrics/internal/promql"
	"github.com/pod32g/omni-metrics/internal/scrape"
	"github.com/pod32g/omni-metrics/internal/tsdb"
	"github.com/pod32g/omni-metrics/web"
)

// version is the build version, overridable via -ldflags "-X main.version=...".
var version = "0.1.0-m1"

// defaultMaxSeries caps head cardinality as a safety valve against runaway
// label cardinality (a metrics-cardinality DoS).
const defaultMaxSeries = 1_000_000

func main() {
	var (
		configPath  string
		listen      string
		storagePath string
	)
	flag.StringVar(&configPath, "config", "", "path to a YAML config file (optional; defaults to a self-scrape config)")
	flag.StringVar(&listen, "listen", "", "override the web/API listen address")
	flag.StringVar(&storagePath, "storage", "", "override the storage (WAL) directory; empty = in-memory only")
	flag.Parse()

	cfg, err := loadConfig(configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if listen != "" {
		cfg.Web.Listen = listen
	}
	if storagePath != "" {
		cfg.Storage.Path = storagePath
	}
	// When running the default (config-less) setup, keep the self-scrape job
	// pointed at the actual listen address even if -listen overrode it.
	if configPath == "" {
		cfg.ScrapeConfigs = []config.ScrapeConfig{{
			JobName:        "omni",
			ScrapeInterval: cfg.Global.ScrapeInterval,
			ScrapeTimeout:  cfg.Global.ScrapeTimeout,
			StaticConfigs:  []config.StaticConfig{{Targets: []string{cfg.Web.Listen}}},
		}}
	}

	// Storage: open and replay the WAL. A non-nil error with a non-nil DB means a
	// partial recovery — log it but keep serving the recovered data.
	db, err := tsdb.Open(tsdb.Options{
		Dir:       cfg.Storage.Path,
		Retention: cfg.Storage.Retention.D(),
		MaxSeries: defaultMaxSeries,
	})
	if db == nil {
		log.Fatalf("storage: %v", err)
	}
	if err != nil {
		log.Printf("WARN: storage recovered with issues: %v", err)
	}
	defer db.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Scraper.
	mgr := scrape.NewManager(db, 0)
	go mgr.Run(ctx, toScrapeConfigs(cfg))

	// Retention enforcement.
	if ret := cfg.Storage.Retention.D(); ret > 0 {
		go retentionLoop(ctx, db, ret)
	}

	// HTTP server: API + embedded console.
	handler := api.New(api.Options{
		Engine:     promql.NewEngine(db),
		Storage:    db,
		Targets:    mgr,
		Web:        web.Handler(),
		Version:    version,
		HeadSeries: db.HeadSeries,
	})
	srv := &http.Server{
		Addr:              cfg.Web.Listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		log.Printf("shutting down...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("omni-metrics %s listening on http://%s (storage: %s)", version, cfg.Web.Listen, storageDesc(cfg.Storage.Path))
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http server: %v", err)
	}
	log.Printf("stopped")
}

func loadConfig(path string) (*config.Config, error) {
	if path == "" {
		return config.Default(), nil
	}
	return config.Load(path)
}

func toScrapeConfigs(cfg *config.Config) []scrape.ScrapeConfig {
	out := make([]scrape.ScrapeConfig, 0, len(cfg.ScrapeConfigs))
	for _, sc := range cfg.ScrapeConfigs {
		out = append(out, scrape.ScrapeConfig{
			JobName:  sc.JobName,
			Interval: sc.ScrapeInterval.D(),
			Timeout:  sc.ScrapeTimeout.D(),
			Targets:  sc.AllTargets(),
		})
	}
	return out
}

func retentionLoop(ctx context.Context, db *tsdb.DB, retention time.Duration) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			db.Truncate(time.Now().Add(-retention).UnixMilli())
		}
	}
}

func storageDesc(path string) string {
	if path == "" {
		return "in-memory"
	}
	return path
}
