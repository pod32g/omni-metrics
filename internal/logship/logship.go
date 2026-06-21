// Package logship tees the process's log output to an omnilog server. It wraps
// log lines as NDJSON and POSTs them in batches from a background worker,
// non-blocking and best-effort: if omnilog is slow or down, lines are dropped
// rather than ever blocking the program. Use it as an io.Writer alongside the
// normal stderr writer (e.g. via io.MultiWriter).
package logship

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Config configures the shipper. Zero batch/flush/buffer fields use defaults.
type Config struct {
	URL     string
	APIKey  string
	Service string

	BatchSize     int
	FlushInterval time.Duration
	BufferSize    int
	Timeout       time.Duration
}

// Writer is an io.Writer that ships whole log lines to omnilog.
type Writer struct {
	endpoint string
	apiKey   string
	service  string
	client   *http.Client

	batchSize int
	flush     time.Duration

	ch      chan map[string]any
	dropped atomic.Int64
	done    chan struct{}
	wg      sync.WaitGroup
	once    sync.Once
	buf     []byte
	mu      sync.Mutex
}

// NewWriter validates cfg and starts the background worker.
func NewWriter(cfg Config) (*Writer, error) {
	if cfg.URL == "" || cfg.APIKey == "" {
		return nil, errors.New("logship: url and api_key are required")
	}
	if cfg.Service == "" {
		cfg.Service = "omni-metrics"
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 100
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = 2 * time.Second
	}
	if cfg.BufferSize <= 0 {
		cfg.BufferSize = 2048
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 5 * time.Second
	}
	w := &Writer{
		endpoint:  strings.TrimRight(cfg.URL, "/") + "/api/v1/ingest",
		apiKey:    cfg.APIKey,
		service:   cfg.Service,
		client:    &http.Client{Timeout: cfg.Timeout},
		batchSize: cfg.BatchSize,
		flush:     cfg.FlushInterval,
		ch:        make(chan map[string]any, cfg.BufferSize),
		done:      make(chan struct{}),
	}
	w.wg.Add(1)
	go w.run()
	return w, nil
}

// Write accepts log output, splits it into complete lines, and enqueues each.
// It always reports success for the full input so logging never errors.
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.buf = append(w.buf, p...)
	var lines []string
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		lines = append(lines, string(bytes.TrimRight(w.buf[:i], "\r")))
		w.buf = w.buf[i+1:]
	}
	w.mu.Unlock()

	for _, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		rec := map[string]any{
			"time":    nowRFC3339(),
			"service": w.service,
			"level":   levelOf(ln),
			"message": ln,
		}
		select {
		case w.ch <- rec:
		default:
			w.dropped.Add(1)
		}
	}
	return len(p), nil
}

// Dropped returns the number of lines dropped due to a full buffer.
func (w *Writer) Dropped() int64 { return w.dropped.Load() }

// Close flushes pending lines and stops the worker, bounded by ctx.
func (w *Writer) Close(ctx context.Context) error {
	w.once.Do(func() { close(w.done) })
	done := make(chan struct{})
	go func() { w.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w *Writer) run() {
	defer w.wg.Done()
	ticker := time.NewTicker(w.flush)
	defer ticker.Stop()
	batch := make([]map[string]any, 0, w.batchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		w.post(batch)
		batch = batch[:0]
	}
	for {
		select {
		case rec := <-w.ch:
			batch = append(batch, rec)
			if len(batch) >= w.batchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-w.done:
			for {
				select {
				case rec := <-w.ch:
					batch = append(batch, rec)
					if len(batch) >= w.batchSize {
						flush()
					}
					continue
				default:
				}
				break
			}
			flush()
			return
		}
	}
}

func (w *Writer) post(batch []map[string]any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, rec := range batch {
		if err := enc.Encode(rec); err != nil {
			continue
		}
	}
	body := buf.Bytes()
	for attempt := 0; attempt < 3; attempt++ {
		req, err := http.NewRequest(http.MethodPost, w.endpoint, bytes.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/x-ndjson")
		req.Header.Set("X-Api-Key", w.apiKey)
		resp, err := w.client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode/100 == 2 {
				return
			}
		}
		time.Sleep(time.Duration(attempt+1) * 200 * time.Millisecond)
	}
	w.dropped.Add(int64(len(batch)))
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// levelOf does a cheap severity guess from the line text.
func levelOf(line string) string {
	u := strings.ToUpper(line)
	switch {
	case strings.Contains(u, "FATAL"):
		return "error"
	case strings.Contains(u, "ERROR"):
		return "error"
	case strings.Contains(u, "WARN"):
		return "warn"
	default:
		return "info"
	}
}
