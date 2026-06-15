// Package scrape implements Prometheus-style pull collection: on an interval it
// fetches each target's /metrics endpoint, parses the exposition format, and
// appends the samples to storage with injected job/instance labels. It
// synthesizes the conventional up, scrape_duration_seconds, and
// scrape_samples_scraped series, and enforces a per-scrape sample limit to bound
// cardinality.
package scrape

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pod32g/omni-metrics/internal/exposition"
	"github.com/pod32g/omni-metrics/internal/model"
	"github.com/pod32g/omni-metrics/internal/tsdb"
)

// defaultMaxBodyBytes bounds a scrape response body to protect against a target
// serving an unbounded /metrics payload.
const defaultMaxBodyBytes = 16 << 20

// Appendable is the storage dependency: a source of appenders.
type Appendable interface {
	Appender() tsdb.Appender
}

// Target is a single resolved scrape endpoint.
type Target struct {
	Job      string
	Instance string
	URL      string
}

// TargetHealth is a snapshot of a target's last scrape, for /api/v1/targets.
type TargetHealth struct {
	Job             string    `json:"job"`
	Instance        string    `json:"instance"`
	URL             string    `json:"scrapeUrl"`
	Up              bool      `json:"up"`
	LastScrape      time.Time `json:"lastScrape"`
	LastError       string    `json:"lastError"`
	DurationSeconds float64   `json:"lastScrapeDuration"`
	SamplesScraped  int       `json:"samplesScraped"`
}

// ScrapeConfig is one job: a set of targets sharing an interval and timeout.
type ScrapeConfig struct {
	JobName  string
	Interval time.Duration
	Timeout  time.Duration
	Targets  []string
}

// Manager runs scrape loops and tracks target health.
type Manager struct {
	app          Appendable
	client       *http.Client
	sampleLimit  int
	maxBodyBytes int64

	mu     sync.RWMutex
	health map[string]*TargetHealth
}

// NewManager builds a Manager. sampleLimit caps the number of series accepted per
// scrape (0 = unlimited).
func NewManager(app Appendable, sampleLimit int) *Manager {
	return &Manager{
		app:          app,
		client:       &http.Client{},
		sampleLimit:  sampleLimit,
		maxBodyBytes: defaultMaxBodyBytes,
		health:       map[string]*TargetHealth{},
	}
}

// Run starts one goroutine per target and blocks until ctx is cancelled.
func (m *Manager) Run(ctx context.Context, configs []ScrapeConfig) {
	var wg sync.WaitGroup
	for _, cfg := range configs {
		interval := cfg.Interval
		if interval <= 0 {
			interval = 15 * time.Second
		}
		timeout := cfg.Timeout
		if timeout <= 0 || timeout > interval {
			timeout = interval
		}
		for _, raw := range cfg.Targets {
			tgt, err := normalizeTarget(cfg.JobName, raw)
			if err != nil {
				m.recordError(cfg.JobName, raw, fmt.Sprintf("invalid target: %v", err))
				continue
			}
			wg.Add(1)
			go func(tgt Target) {
				defer wg.Done()
				m.loop(ctx, tgt, interval, timeout)
			}(tgt)
		}
	}
	wg.Wait()
}

// loop scrapes a target immediately and then on each tick until ctx is done.
func (m *Manager) loop(ctx context.Context, tgt Target, interval, timeout time.Duration) {
	m.scrapeOnce(ctx, tgt, timeout)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.scrapeOnce(ctx, tgt, timeout)
		}
	}
}

// scrapeOnce performs a single scrape: fetch, parse, enforce limits, and append
// the samples plus the synthesized up/duration/count series. A fatal error
// (fetch, HTTP, size, sample-limit, or a storage cardinality-cap rejection)
// fails the scrape (up=0); a non-fatal parse warning is still recorded against
// the target without failing it.
func (m *Manager) scrapeOnce(ctx context.Context, tgt Target, timeout time.Duration) {
	start := time.Now()
	ts := start.UnixMilli()

	series, fetchErr, parseWarn := m.fetchAndParse(ctx, tgt, timeout)
	scrapeErr := fetchErr
	if scrapeErr == nil && m.sampleLimit > 0 && len(series) > m.sampleLimit {
		scrapeErr = fmt.Errorf("sample limit exceeded: %d > %d", len(series), m.sampleLimit)
		series = nil
	}

	stored := 0
	app := m.app.Appender()
	if scrapeErr == nil {
		ok := true
		for _, s := range series {
			if _, err := app.Append(injectTargetLabels(s.Labels, tgt), ts, s.Value); err != nil {
				// e.g. the head cardinality cap: fail the scrape rather than
				// silently dropping samples while reporting up=1.
				scrapeErr = fmt.Errorf("append failed: %w", err)
				ok = false
				break
			}
		}
		if ok {
			stored = len(series)
		} else {
			_ = app.Rollback()
			app = m.app.Appender()
		}
	}

	duration := time.Since(start).Seconds()
	up := 1.0
	if scrapeErr != nil {
		up = 0.0
	}
	// Synthesized series are best-effort under a saturated head, but target
	// health (below) is in-memory and always reflects the true outcome.
	_, _ = app.Append(syntheticLabels("up", tgt), ts, up)
	_, _ = app.Append(syntheticLabels("scrape_duration_seconds", tgt), ts, duration)
	_, _ = app.Append(syntheticLabels("scrape_samples_scraped", tgt), ts, float64(stored))
	if err := app.Commit(); err != nil && scrapeErr == nil {
		scrapeErr = fmt.Errorf("storage commit: %w", err)
	}

	healthErr := scrapeErr
	if healthErr == nil {
		healthErr = parseWarn // surface partial-parse problems without failing the target
	}
	m.updateHealth(tgt, scrapeErr == nil, start, duration, stored, healthErr)
}

// fetchAndParse returns the parsed series, a fatal error (fetch/HTTP/size), and a
// non-fatal parse warning (malformed lines were skipped but some series parsed).
func (m *Manager) fetchAndParse(ctx context.Context, tgt Target, timeout time.Duration) (series []exposition.Series, fatal error, parseWarn error) {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, tgt.URL, nil)
	if err != nil {
		return nil, err, nil
	}
	req.Header.Set("Accept", "text/plain;version=0.0.4")
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, err, nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("server returned HTTP %d", resp.StatusCode), nil
	}
	// Read one byte past the limit so we can detect (rather than silently
	// truncate) an over-large body.
	body, err := io.ReadAll(io.LimitReader(resp.Body, m.maxBodyBytes+1))
	if err != nil {
		return nil, err, nil
	}
	if int64(len(body)) > m.maxBodyBytes {
		return nil, fmt.Errorf("response body exceeds %d byte limit", m.maxBodyBytes), nil
	}
	res, perr := exposition.Parse(bytes.NewReader(body))
	if res == nil {
		return nil, perr, nil
	}
	return res.Series, nil, perr
}

// injectTargetLabels adds job and instance to a scraped series' labels.
func injectTargetLabels(l model.Labels, tgt Target) model.Labels {
	m := l.Map()
	m["job"] = tgt.Job
	m["instance"] = tgt.Instance
	return model.FromMap(m)
}

func syntheticLabels(name string, tgt Target) model.Labels {
	return model.FromStrings(model.MetricName, name, "job", tgt.Job, "instance", tgt.Instance)
}

func (m *Manager) updateHealth(tgt Target, up bool, start time.Time, dur float64, samples int, err error) {
	h := &TargetHealth{
		Job:             tgt.Job,
		Instance:        tgt.Instance,
		URL:             tgt.URL,
		Up:              up,
		LastScrape:      start,
		DurationSeconds: dur,
		SamplesScraped:  samples,
	}
	if err != nil {
		h.LastError = err.Error()
	}
	m.mu.Lock()
	m.health[healthKey(tgt.Job, tgt.Instance)] = h
	m.mu.Unlock()
}

func (m *Manager) recordError(job, raw, msg string) {
	m.mu.Lock()
	m.health[healthKey(job, raw)] = &TargetHealth{Job: job, Instance: raw, URL: raw, LastError: msg}
	m.mu.Unlock()
}

// Targets returns a snapshot of target health, sorted by job then instance.
func (m *Manager) Targets() []TargetHealth {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]TargetHealth, 0, len(m.health))
	for _, h := range m.health {
		out = append(out, *h)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Job != out[j].Job {
			return out[i].Job < out[j].Job
		}
		return out[i].Instance < out[j].Instance
	})
	return out
}

func healthKey(job, instance string) string { return job + "\x00" + instance }

// normalizeTarget resolves a raw target string into a Target. A bare host:port
// gets an http scheme and the default /metrics path; the instance is the
// host:port authority.
func normalizeTarget(job, raw string) (Target, error) {
	s := raw
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return Target{}, err
	}
	if u.Host == "" {
		return Target{}, fmt.Errorf("missing host in %q", raw)
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = "/metrics"
	}
	return Target{Job: job, Instance: u.Host, URL: u.String()}, nil
}
