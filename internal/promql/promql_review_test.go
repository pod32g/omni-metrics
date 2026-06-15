package promql_test

import (
	"math"
	"testing"

	"github.com/pod32g/omni-metrics/internal/model"
	"github.com/pod32g/omni-metrics/internal/promql"
	"github.com/pod32g/omni-metrics/internal/tsdb"
)

// clamp(v, min, max) with min > max returns an empty vector (Prometheus).
func TestClampMinGreaterThanMaxIsEmpty(t *testing.T) {
	eng := promql.NewEngine(buildExtDB(t))
	r := instant(t, eng, "clamp(m, 10, 0)", 3000)
	if r.Type != promql.ValueVector || len(r.Vector) != 0 {
		t.Errorf("clamp(m,10,0) = %v, want empty vector", r.Vector)
	}
}

// sort()/sort_desc() order finite values and push NaN to the end.
func TestSortNaNLast(t *testing.T) {
	db, err := tsdb.Open(tsdb.Options{})
	if err != nil {
		t.Fatal(err)
	}
	app := db.Appender()
	app.Append(model.FromStrings(model.MetricName, "g", "k", "a"), 3000, 5)
	app.Append(model.FromStrings(model.MetricName, "g", "k", "b"), 3000, math.NaN())
	app.Append(model.FromStrings(model.MetricName, "g", "k", "c"), 3000, 2)
	if err := app.Commit(); err != nil {
		t.Fatal(err)
	}
	eng := promql.NewEngine(db)

	r := instant(t, eng, "sort(g)", 3000)
	if len(r.Vector) != 3 {
		t.Fatalf("sort(g) len = %d, want 3", len(r.Vector))
	}
	if r.Vector[0].V != 2 || r.Vector[1].V != 5 || !math.IsNaN(r.Vector[2].V) {
		t.Errorf("sort(g) = %v,%v,%v, want 2,5,NaN", r.Vector[0].V, r.Vector[1].V, r.Vector[2].V)
	}

	rd := instant(t, eng, "sort_desc(g)", 3000)
	if rd.Vector[0].V != 5 || rd.Vector[1].V != 2 || !math.IsNaN(rd.Vector[2].V) {
		t.Errorf("sort_desc(g) = %v,%v,%v, want 5,2,NaN", rd.Vector[0].V, rd.Vector[1].V, rd.Vector[2].V)
	}
}

// histogram_quantile repairs non-monotonic cumulative bucket counts (scrape-race
// artifacts) before interpolating, matching Prometheus.
func TestHistogramQuantileMonotonic(t *testing.T) {
	db, err := tsdb.Open(tsdb.Options{})
	if err != nil {
		t.Fatal(err)
	}
	app := db.Appender()
	hb := func(le string, v float64) {
		app.Append(model.FromStrings(model.MetricName, "nm", "le", le), 3000, v)
	}
	hb("1", 2)
	hb("2", 1) // non-monotonic: drops below the previous bucket
	hb("3", 9)
	hb("+Inf", 9)
	if err := app.Commit(); err != nil {
		t.Fatal(err)
	}
	eng := promql.NewEngine(db)
	got := one(t, instant(t, eng, "histogram_quantile(0.3, nm)", 3000))
	if !approx(got, 2.1) {
		t.Errorf("histogram_quantile(0.3,nm) = %v, want 2.1 (after monotonic repair)", got)
	}
}

// timestamp() returns the sample's own timestamp, not the evaluation time.
func TestTimestampReturnsSampleTime(t *testing.T) {
	eng := promql.NewEngine(buildExtDB(t))
	// m{job=b}'s latest sample is at t=3000; evaluate at 3500 (within lookback).
	got := one(t, instant(t, eng, `timestamp(m{job="b"})`, 3500))
	if got != 3 {
		t.Errorf("timestamp(m{job=b}) @3500 = %v, want 3 (sample time)", got)
	}
}
