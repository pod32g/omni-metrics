package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pod32g/omni-metrics/internal/api"
	"github.com/pod32g/omni-metrics/internal/exposition"
	"github.com/pod32g/omni-metrics/internal/model"
	"github.com/pod32g/omni-metrics/internal/promql"
	"github.com/pod32g/omni-metrics/internal/scrape"
	"github.com/pod32g/omni-metrics/internal/tsdb"
)

type stubTargets struct{ items []scrape.TargetHealth }

func (s stubTargets) Targets() []scrape.TargetHealth { return s.items }

func buildAPI(t *testing.T) http.Handler {
	t.Helper()
	db, err := tsdb.Open(tsdb.Options{})
	if err != nil {
		t.Fatal(err)
	}
	app := db.Appender()
	add := func(l model.Labels, t int64, v float64) { app.Append(l, t, v) }
	add(model.FromStrings(model.MetricName, "m", "job", "a"), 3000, 3)
	add(model.FromStrings(model.MetricName, "m", "job", "a", "inst", "2"), 3000, 5)
	add(model.FromStrings(model.MetricName, "m", "job", "b"), 1000, 10)
	add(model.FromStrings(model.MetricName, "m", "job", "b"), 2000, 20)
	add(model.FromStrings(model.MetricName, "m", "job", "b"), 3000, 30)
	app.Commit()

	return api.New(api.Options{
		Engine:     promql.NewEngine(db),
		Storage:    db,
		Targets:    stubTargets{items: []scrape.TargetHealth{{Job: "omni", Instance: "localhost:9090", Up: true}}},
		Web:        http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("<html>ui</html>")) }),
		Version:    "test",
		HeadSeries: db.HeadSeries,
	})
}

func getJSON(t *testing.T, h http.Handler, path string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v (body=%s)", path, err, rec.Body.String())
	}
	return rec.Code, body
}

func TestQueryInstant(t *testing.T) {
	h := buildAPI(t)
	code, body := getJSON(t, h, "/api/v1/query?query=m&time=3")
	if code != 200 || body["status"] != "success" {
		t.Fatalf("status %d %v", code, body["status"])
	}
	data := body["data"].(map[string]any)
	if data["resultType"] != "vector" {
		t.Errorf("resultType = %v", data["resultType"])
	}
	if got := len(data["result"].([]any)); got != 3 {
		t.Errorf("result count = %d, want 3", got)
	}
}

func TestQueryRange(t *testing.T) {
	h := buildAPI(t)
	code, body := getJSON(t, h, `/api/v1/query_range?query=m{job="b"}&start=1&end=3&step=1`)
	if code != 200 || body["status"] != "success" {
		t.Fatalf("status %d %v", code, body)
	}
	data := body["data"].(map[string]any)
	if data["resultType"] != "matrix" {
		t.Fatalf("resultType = %v", data["resultType"])
	}
	res := data["result"].([]any)
	if len(res) != 1 {
		t.Fatalf("series = %d, want 1", len(res))
	}
	vals := res[0].(map[string]any)["values"].([]any)
	if len(vals) != 3 {
		t.Errorf("points = %d, want 3", len(vals))
	}
}

func TestQueryParseErrorIs400(t *testing.T) {
	h := buildAPI(t)
	code, body := getJSON(t, h, "/api/v1/query?query=m{")
	if code != http.StatusBadRequest || body["status"] != "error" {
		t.Fatalf("expected 400 error, got %d %v", code, body)
	}
	if body["errorType"] != "bad_data" {
		t.Errorf("errorType = %v, want bad_data", body["errorType"])
	}
}

func TestSeries(t *testing.T) {
	h := buildAPI(t)
	code, body := getJSON(t, h, "/api/v1/series?match[]=m")
	if code != 200 || body["status"] != "success" {
		t.Fatalf("status %d %v", code, body)
	}
	res := body["data"].([]any)
	if len(res) != 3 { // m{job=a}, m{job=a,inst=2}, m{job=b}
		t.Errorf("series = %d, want 3", len(res))
	}
}

func TestLabelsAndValues(t *testing.T) {
	h := buildAPI(t)
	_, body := getJSON(t, h, "/api/v1/labels")
	names := body["data"].([]any)
	found := false
	for _, n := range names {
		if n == "job" {
			found = true
		}
	}
	if !found {
		t.Errorf("labels missing 'job': %v", names)
	}

	_, vb := getJSON(t, h, "/api/v1/label/job/values")
	vals := vb["data"].([]any)
	if len(vals) != 2 {
		t.Errorf("job values = %v, want 2", vals)
	}
}

func TestTargets(t *testing.T) {
	h := buildAPI(t)
	code, body := getJSON(t, h, "/api/v1/targets")
	if code != 200 || body["status"] != "success" {
		t.Fatalf("status %d %v", code, body)
	}
	res := body["data"].([]any)
	if len(res) != 1 {
		t.Fatalf("targets = %d, want 1", len(res))
	}
}

func TestMetricsEndpointSelfParses(t *testing.T) {
	h := buildAPI(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != 200 {
		t.Fatalf("/metrics status %d", rec.Code)
	}
	bodyText := rec.Body.String()
	if !strings.Contains(bodyText, "omni_") {
		t.Errorf("/metrics missing omni_ metrics:\n%s", bodyText)
	}
	// Parity self-check: our own exposition must parse with our own parser.
	res, err := exposition.Parse(strings.NewReader(bodyText))
	if err != nil {
		t.Errorf("/metrics body did not parse cleanly: %v", err)
	}
	if len(res.Series) == 0 {
		t.Errorf("/metrics produced no parseable series")
	}
}

func TestInvalidTimeParamsRejected(t *testing.T) {
	h := buildAPI(t)
	for _, p := range []string{
		"/api/v1/query?query=m&time=NaN",
		"/api/v1/query?query=m&time=Inf",
		"/api/v1/query_range?query=m&start=0&end=Inf&step=1",
	} {
		code, body := getJSON(t, h, p)
		if code != http.StatusBadRequest || body["status"] != "error" {
			t.Errorf("%s: expected 400 error, got %d %v", p, code, body["status"])
		}
	}
}

func TestSeriesSurfacesBadTime(t *testing.T) {
	h := buildAPI(t)
	code, body := getJSON(t, h, "/api/v1/series?match[]=m&end=oops")
	if code != http.StatusBadRequest || body["status"] != "error" {
		t.Errorf("bad end on /series should be 400, got %d %v", code, body)
	}
}

func TestRangeEndBeforeStartRejected(t *testing.T) {
	h := buildAPI(t)
	code, body := getJSON(t, h, "/api/v1/query_range?query=m&start=100&end=10&step=1")
	if code != http.StatusBadRequest || body["status"] != "error" {
		t.Errorf("end<start should be 400, got %d %v", code, body)
	}
}

func TestRangeExcessivePointsRejected(t *testing.T) {
	h := buildAPI(t)
	code, body := getJSON(t, h, "/api/v1/query_range?query=m&start=0&end=100000000&step=1")
	if code != http.StatusBadRequest || body["status"] != "error" {
		t.Errorf("excessive resolution should be 400, got %d %v", code, body)
	}
}

func TestWebFallback(t *testing.T) {
	h := buildAPI(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "ui") {
		t.Errorf("web fallback failed: %d %s", rec.Code, rec.Body.String())
	}
}
