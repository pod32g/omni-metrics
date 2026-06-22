// Command omni is the omni-metrics server: it scrapes targets, stores samples in
// a head block backed by a write-ahead log, evaluates PromQL, and serves a
// Prometheus-compatible HTTP API plus an embedded web console.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/pod32g/omni-metrics/internal/alerts"
	"github.com/pod32g/omni-metrics/internal/alerts/models"
	"github.com/pod32g/omni-metrics/internal/api"
	"github.com/pod32g/omni-metrics/internal/config"
	"github.com/pod32g/omni-metrics/internal/logship"
	"github.com/pod32g/omni-metrics/internal/promql"
	"github.com/pod32g/omni-metrics/internal/push"
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
	// `omni healthcheck -url <url>` is a self-probe used by the container
	// healthcheck and the deploy smoke test; it exits 0 on a 2xx response.
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheckCmd(os.Args[2:]))
	}

	// Optionally tee log output to an omnilog server (LOGSHIP_* env). Best-effort.
	if w := setupLogShipping(); w != nil {
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = w.Close(ctx)
		}()
	}

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
			StaticConfigs:  []config.StaticConfig{{Targets: []string{selfScrapeTarget(cfg.Web.Listen)}}},
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
	scrapeCfgs, err := toScrapeConfigs(cfg)
	if err != nil {
		log.Fatalf("scrape config: %v", err)
	}
	mgr := scrape.NewManager(db, 0)
	go mgr.Run(ctx, scrapeCfgs)

	// Push ingestion: clients that cannot be scraped POST samples here.
	ingester := push.NewIngester(db, cfg.Push.SampleLimit)

	// Retention enforcement.
	if ret := cfg.Storage.Retention.D(); ret > 0 {
		go retentionLoop(ctx, db, ret)
	}

	// Alerting engine (optional). Self-contained: queries datasources over HTTP,
	// persists to its own SQLite store, and runs a per-rule scheduler.
	var alertSvc *alerts.Service
	if cfg.Alerting.IsEnabled() {
		dss, defaultDS := buildAlertDatasources(cfg)
		svc, err := alerts.NewService(alerts.Options{
			StorePath:         alertStorePath(cfg),
			Datasources:       dss,
			DefaultDatasource: defaultDS,
			Now:               time.Now,
		})
		if err != nil {
			log.Fatalf("alerting: %v", err)
		}
		alertSvc = svc
		go svc.Start(ctx)
		defer svc.Stop()
		log.Printf("alerting engine enabled (store: %s, %d datasource(s))", alertStorePath(cfg), len(dss))
	}

	// HTTP server: API + embedded console.
	apiOpts := api.Options{
		Engine:      promql.NewEngine(db),
		Storage:     db,
		Targets:     mgr,
		Web:         web.Handler(),
		Version:     version,
		HeadSeries:  db.HeadSeries,
		Push:        ingester,
		PushSources: ingester,
		PushConfig: api.PushConfig{
			Enabled:      cfg.Push.IsEnabled(),
			MaxBodyBytes: cfg.Push.BodyLimit(),
			AuthToken:    cfg.Push.AuthToken,
		},
	}
	if alertSvc != nil {
		apiOpts.AlertHandler = alertSvc.Handler()
		apiOpts.ExtraCollectors = []func(io.Writer){alertSvc.Collector()}
	}
	handler := api.New(apiOpts)
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

// setupLogShipping tees the standard logger to omnilog when LOGSHIP_ENABLED is
// set. It returns the writer (to be closed on shutdown) or nil when disabled.
func setupLogShipping() *logship.Writer {
	if v := os.Getenv("LOGSHIP_ENABLED"); v != "true" && v != "1" {
		return nil
	}
	w, err := logship.NewWriter(logship.Config{
		URL:     os.Getenv("LOGSHIP_URL"),
		APIKey:  os.Getenv("LOGSHIP_API_KEY"),
		Service: envOr("LOGSHIP_SERVICE", "omni-metrics"),
	})
	if err != nil {
		log.Printf("logship: disabled (%v)", err)
		return nil
	}
	log.SetOutput(io.MultiWriter(os.Stderr, w))
	return w
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func loadConfig(path string) (*config.Config, error) {
	if path == "" {
		return config.Default(), nil
	}
	return config.Load(path)
}

func toScrapeConfigs(cfg *config.Config) ([]scrape.ScrapeConfig, error) {
	out := make([]scrape.ScrapeConfig, 0, len(cfg.ScrapeConfigs))
	for _, sc := range cfg.ScrapeConfigs {
		tlsCfg, err := sc.TLS.Build()
		if err != nil {
			return nil, fmt.Errorf("scrape config %q: %w", sc.JobName, err)
		}
		out = append(out, scrape.ScrapeConfig{
			JobName:  sc.JobName,
			Scheme:   sc.Scheme,
			Interval: sc.ScrapeInterval.D(),
			Timeout:  sc.ScrapeTimeout.D(),
			Targets:  sc.AllTargets(),
			Auth:     toAuth(sc),
			TLS:      tlsCfg,
		})
	}
	return out, nil
}

// toAuth maps a config scrape job's auth block to the scrape layer's resolved
// Auth. Credentials take precedence; basic auth is used when present.
func toAuth(sc config.ScrapeConfig) scrape.Auth {
	if a := sc.Authorization; a != nil && a.Credentials != "" {
		return scrape.Auth{Type: a.Type, Credentials: a.Credentials}
	}
	if b := sc.BasicAuth; b != nil && (b.Username != "" || b.Password != "") {
		return scrape.Auth{BasicUser: b.Username, BasicPass: b.Password, HasBasic: true}
	}
	return scrape.Auth{}
}

// alertStorePath resolves where the alerting SQLite database lives: an explicit
// alerting.storage_path wins; otherwise it sits alongside the WAL data dir, or
// is in-memory when storage is in-memory.
func alertStorePath(cfg *config.Config) string {
	if cfg.Alerting.StoragePath != "" {
		return cfg.Alerting.StoragePath
	}
	if cfg.Storage.Path != "" {
		return filepath.Join(cfg.Storage.Path, "alerts.db")
	}
	return ":memory:"
}

// buildAlertDatasources maps the config datasources to the engine's model and
// prepends a builtin "local" datasource (pointing at omni's own listen address)
// unless the config already defines one named "local". It returns the
// datasources and the resolved default datasource name.
func buildAlertDatasources(cfg *config.Config) ([]models.Datasource, string) {
	defaultName := cfg.Alerting.DefaultDatasource
	if defaultName == "" {
		defaultName = "local"
	}
	out := make([]models.Datasource, 0, len(cfg.Alerting.Datasources)+1)
	haveLocal := false
	for _, d := range cfg.Alerting.Datasources {
		if d.Name == "local" {
			haveLocal = true
		}
		out = append(out, alertDatasourceFromConfig(d))
	}
	if !haveLocal {
		out = append([]models.Datasource{builtinLocalDatasource(cfg.Web.Listen)}, out...)
	}
	return out, defaultName
}

func builtinLocalDatasource(listen string) models.Datasource {
	return models.Datasource{
		ID:        "local",
		Name:      "local",
		Type:      "prometheus",
		BaseURL:   "http://" + selfScrapeTarget(listen),
		AuthType:  models.AuthNone,
		TimeoutMS: int(config.DefaultAlertDatasourceTimeout / time.Millisecond),
		Enabled:   true,
		Source:    models.SourceBuiltin,
	}
}

func alertDatasourceFromConfig(d config.AlertDatasourceConfig) models.Datasource {
	out := models.Datasource{
		ID:        d.Name, // config datasource ids are their (unique) names; stable across boots
		Name:      d.Name,
		Type:      d.Type,
		BaseURL:   d.URL,
		Headers:   d.Headers,
		TimeoutMS: int(d.Timeout.D() / time.Millisecond),
		Enabled:   d.IsEnabled(),
		Source:    models.SourceConfig,
		AuthType:  models.AuthNone,
	}
	switch {
	case d.Authorization != nil && d.Authorization.Credentials != "":
		out.AuthType = models.AuthBearer
		out.Credentials = d.Authorization.Credentials
	case d.BasicAuth != nil && (d.BasicAuth.Username != "" || d.BasicAuth.Password != ""):
		out.AuthType = models.AuthBasic
		out.BasicUser = d.BasicAuth.Username
		out.BasicPass = d.BasicAuth.Password
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

// selfScrapeTarget turns a listen address into one the server can scrape itself
// on: a wildcard bind (0.0.0.0 / :: / empty host) is rewritten to loopback so the
// in-container self-scrape connects cleanly.
func selfScrapeTarget(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return listen
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// healthcheckCmd parses `healthcheck -url <url>` and returns a process exit code.
func healthcheckCmd(args []string) int {
	fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	url := fs.String("url", "", "health endpoint URL to probe")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *url == "" {
		fmt.Fprintln(os.Stderr, "healthcheck: -url is required")
		return 2
	}
	if err := doHealthcheck(*url, 5*time.Second); err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck failed: %v\n", err)
		return 1
	}
	return 0
}

// doHealthcheck performs a single GET and returns an error unless the response is
// a 2xx. It carries no other dependencies so a distroless image can run it.
func doHealthcheck(url string, timeout time.Duration) error {
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("unhealthy: HTTP %d", resp.StatusCode)
	}
	return nil
}
