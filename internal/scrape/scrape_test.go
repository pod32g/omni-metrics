package scrape

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pod32g/omni-metrics/internal/model"
	"github.com/pod32g/omni-metrics/internal/tsdb"
)

func newDB(t *testing.T) *tsdb.DB {
	t.Helper()
	db, err := tsdb.Open(tsdb.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// latest returns the most recent value for the series matching name+job, or
// (0,false) if absent.
func latest(t *testing.T, db *tsdb.DB, name, method string) (float64, bool) {
	t.Helper()
	nm, _ := model.NewMatcher(model.MatchEqual, model.MetricName, name)
	var ms []model.Matcher
	ms = append(ms, *nm)
	if method != "" {
		mm, _ := model.NewMatcher(model.MatchEqual, "method", method)
		ms = append(ms, *mm)
	}
	ss := db.Querier().Select(0, time.Now().UnixMilli()+1000, ms...)
	for ss.Next() {
		s := ss.At().Samples()
		return s[len(s)-1].V, true
	}
	return 0, false
}

func TestScrapeOnceIngests(t *testing.T) {
	body := `# HELP http_requests_total Total
# TYPE http_requests_total counter
http_requests_total{method="get"} 100
http_requests_total{method="post"} 5
go_goroutines 42
`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}))
	defer srv.Close()

	db := newDB(t)
	m := NewManager(db, 0)
	tgt, err := normalizeTarget("test", srv.URL+"/metrics")
	if err != nil {
		t.Fatal(err)
	}
	m.scrapeOnce(context.Background(), tgt, time.Second)

	if v, ok := latest(t, db, "http_requests_total", "get"); !ok || v != 100 {
		t.Errorf("http_requests_total{get} = %v %v, want 100", v, ok)
	}
	if v, ok := latest(t, db, "up", ""); !ok || v != 1 {
		t.Errorf("up = %v %v, want 1", v, ok)
	}
	if _, ok := latest(t, db, "scrape_samples_scraped", ""); !ok {
		t.Errorf("scrape_samples_scraped missing")
	}

	health := m.Targets()
	if len(health) != 1 || !health[0].Up || health[0].Job != "test" {
		t.Fatalf("health = %+v", health)
	}
	if health[0].SamplesScraped != 3 {
		t.Errorf("samples scraped = %d, want 3", health[0].SamplesScraped)
	}
	// Injected target labels must be present.
	if health[0].Instance == "" {
		t.Errorf("instance label not derived")
	}
}

func TestScrapeDownTargetMarksUpZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	db := newDB(t)
	m := NewManager(db, 0)
	tgt, _ := normalizeTarget("test", srv.URL+"/metrics")
	m.scrapeOnce(context.Background(), tgt, time.Second)

	if v, ok := latest(t, db, "up", ""); !ok || v != 0 {
		t.Errorf("up = %v %v, want 0 for failed scrape", v, ok)
	}
	h := m.Targets()
	if len(h) != 1 || h[0].Up || h[0].LastError == "" {
		t.Fatalf("expected down target with error, got %+v", h)
	}
}

func TestSampleLimitRejectsScrape(t *testing.T) {
	body := "a 1\nb 2\nc 3\nd 4\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}))
	defer srv.Close()

	db := newDB(t)
	m := NewManager(db, 2) // limit below the 4 exposed series
	tgt, _ := normalizeTarget("test", srv.URL+"/metrics")
	m.scrapeOnce(context.Background(), tgt, time.Second)

	if _, ok := latest(t, db, "a", ""); ok {
		t.Errorf("series should not be ingested when over sample_limit")
	}
	if v, ok := latest(t, db, "up", ""); !ok || v != 0 {
		t.Errorf("up = %v, want 0 when sample_limit exceeded", v)
	}
	if h := m.Targets(); len(h) == 0 || h[0].Up {
		t.Errorf("target should be marked down on limit breach: %+v", h)
	}
}

func TestHeadCapMarksTargetDown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("a 1\nb 2\nc 3\nd 4\n"))
	}))
	defer srv.Close()

	db, _ := tsdb.Open(tsdb.Options{MaxSeries: 2}) // head fills before all series fit
	m := NewManager(db, 0)
	tgt, _ := normalizeTarget("test", srv.URL+"/metrics")
	m.scrapeOnce(context.Background(), tgt, time.Second)

	h := m.Targets()
	if len(h) != 1 || h[0].Up || h[0].LastError == "" {
		t.Fatalf("head cardinality cap breach must mark the target down with an error: %+v", h)
	}
	if h[0].SamplesScraped != 0 {
		t.Errorf("no samples should be reported as stored when the scrape failed, got %d", h[0].SamplesScraped)
	}
}

func TestScrapeSurfacesParseError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("good 1\nthis is a garbage line nope\nanother 2\n"))
	}))
	defer srv.Close()

	db := newDB(t)
	m := NewManager(db, 0)
	tgt, _ := normalizeTarget("test", srv.URL+"/metrics")
	m.scrapeOnce(context.Background(), tgt, time.Second)

	h := m.Targets()
	if h[0].LastError == "" {
		t.Errorf("parse error must be surfaced, not silently dropped")
	}
	if _, ok := latest(t, db, "good", ""); !ok {
		t.Errorf("good series should still be ingested on partial parse")
	}
	if v, _ := latest(t, db, "up", ""); v != 1 {
		t.Errorf("up should stay 1 on partial parse (target reachable), got %v", v)
	}
}

func TestScrapeBodyTooLarge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i < 200; i++ {
			w.Write([]byte("metric_xxxxxxxx 1\n"))
		}
	}))
	defer srv.Close()

	db := newDB(t)
	m := NewManager(db, 0)
	m.maxBodyBytes = 100 // far below the served body
	tgt, _ := normalizeTarget("test", srv.URL+"/metrics")
	m.scrapeOnce(context.Background(), tgt, time.Second)

	h := m.Targets()
	if h[0].Up || h[0].LastError == "" {
		t.Errorf("oversized body must fail the scrape rather than silently truncate: %+v", h)
	}
}

func TestNormalizeTarget(t *testing.T) {
	cases := []struct {
		in       string
		wantInst string
	}{
		{"http://localhost:9090/metrics", "localhost:9090"},
		{"localhost:9100", "localhost:9100"},
		{"http://node-01:9100/custom", "node-01:9100"},
	}
	for _, tc := range cases {
		tg, err := normalizeTarget("job", tc.in)
		if err != nil {
			t.Fatalf("normalizeTarget(%q): %v", tc.in, err)
		}
		if tg.Instance != tc.wantInst {
			t.Errorf("normalizeTarget(%q).Instance = %q, want %q", tc.in, tg.Instance, tc.wantInst)
		}
	}
}

func TestManagerRunCancels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("x 1\n"))
	}))
	defer srv.Close()

	db := newDB(t)
	m := NewManager(db, 0)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.Run(ctx, []ScrapeConfig{{JobName: "j", Interval: 20 * time.Millisecond, Timeout: time.Second, Targets: []string{srv.URL + "/metrics"}}})
		close(done)
	}()
	time.Sleep(60 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}
	if v, ok := latest(t, db, "x", ""); !ok || v != 1 {
		t.Errorf("expected at least one scrape to have ingested x=1, got %v %v", v, ok)
	}
}
