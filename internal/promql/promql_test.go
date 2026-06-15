package promql_test

import (
	"context"
	"math"
	"testing"

	"github.com/pod32g/omni-metrics/internal/model"
	"github.com/pod32g/omni-metrics/internal/promql"
	"github.com/pod32g/omni-metrics/internal/tsdb"
)

// buildDB returns a storage populated with deterministic test data.
func buildDB(t *testing.T) *tsdb.DB {
	t.Helper()
	db, err := tsdb.Open(tsdb.Options{})
	if err != nil {
		t.Fatal(err)
	}
	app := db.Appender()
	add := func(l model.Labels, pts ...[2]float64) {
		for _, p := range pts {
			if _, err := app.Append(l, int64(p[0]), p[1]); err != nil {
				t.Fatal(err)
			}
		}
	}
	add(model.FromStrings(model.MetricName, "m", "job", "a"), [2]float64{1000, 1}, [2]float64{2000, 2}, [2]float64{3000, 3})
	add(model.FromStrings(model.MetricName, "m", "job", "a", "inst", "2"), [2]float64{3000, 5})
	add(model.FromStrings(model.MetricName, "m", "job", "b"), [2]float64{1000, 10}, [2]float64{2000, 20}, [2]float64{3000, 30})
	add(model.FromStrings(model.MetricName, "c", "job", "a"), [2]float64{1000, 0}, [2]float64{2000, 100}, [2]float64{3000, 250})
	if err := app.Commit(); err != nil {
		t.Fatal(err)
	}
	return db
}

func instant(t *testing.T, eng *promql.Engine, q string, ts int64) promql.Result {
	t.Helper()
	res, err := eng.InstantQuery(context.Background(), q, ts)
	if err != nil {
		t.Fatalf("InstantQuery(%q): %v", q, err)
	}
	return res
}

// vectorMap reduces a vector result to labels-string -> value.
func vectorMap(t *testing.T, r promql.Result) map[string]float64 {
	t.Helper()
	if r.Type != promql.ValueVector {
		t.Fatalf("result type = %v, want vector", r.Type)
	}
	out := map[string]float64{}
	for _, s := range r.Vector {
		out[s.Metric.String()] = s.V
	}
	return out
}

func TestScalarArithmetic(t *testing.T) {
	eng := promql.NewEngine(buildDB(t))
	r := instant(t, eng, "2 * 3 + 1", 3000)
	if r.Type != promql.ValueScalar || r.Scalar.V != 7 {
		t.Fatalf("got %v %v, want scalar 7", r.Type, r.Scalar.V)
	}
	r = instant(t, eng, "2 ^ 3 ^ 2", 3000) // right assoc => 2^9 = 512
	if r.Scalar.V != 512 {
		t.Errorf("pow assoc = %v, want 512", r.Scalar.V)
	}
	r = instant(t, eng, "-(4)", 3000)
	if r.Scalar.V != -4 {
		t.Errorf("unary = %v, want -4", r.Scalar.V)
	}
}

func TestInstantVectorSelector(t *testing.T) {
	eng := promql.NewEngine(buildDB(t))
	got := vectorMap(t, instant(t, eng, `m{job="b"}`, 3000))
	if v, ok := got[`m{job="b"}`]; !ok || v != 30 {
		t.Fatalf("m{job=b} = %v, want 30", got)
	}
}

func TestVectorSelectorAll(t *testing.T) {
	eng := promql.NewEngine(buildDB(t))
	got := vectorMap(t, instant(t, eng, `m`, 3000))
	if len(got) != 3 {
		t.Fatalf("m returned %d series, want 3: %v", len(got), got)
	}
}

func TestAggregationSum(t *testing.T) {
	eng := promql.NewEngine(buildDB(t))
	r := instant(t, eng, "sum(m)", 3000)
	got := vectorMap(t, r)
	if len(got) != 1 || got["{}"] != 38 { // 3 + 5 + 30
		t.Fatalf("sum(m) = %v, want {}=38", got)
	}
}

func TestAggregationSumBy(t *testing.T) {
	eng := promql.NewEngine(buildDB(t))
	got := vectorMap(t, instant(t, eng, "sum by (job) (m)", 3000))
	if got[`{job="a"}`] != 8 || got[`{job="b"}`] != 30 {
		t.Fatalf("sum by(job)(m) = %v, want a=8 b=30", got)
	}
}

func TestAggregationCountAvg(t *testing.T) {
	eng := promql.NewEngine(buildDB(t))
	if got := vectorMap(t, instant(t, eng, "count(m)", 3000)); got["{}"] != 3 {
		t.Errorf("count(m) = %v, want 3", got)
	}
	if got := vectorMap(t, instant(t, eng, `avg by (job) (m)`, 3000)); got[`{job="a"}`] != 4 {
		t.Errorf("avg by(job)(m) a = %v, want 4", got) // (3+5)/2
	}
}

func TestRate(t *testing.T) {
	eng := promql.NewEngine(buildDB(t))
	got := vectorMap(t, instant(t, eng, "rate(c[5m])", 3000))
	// delta = 250 over (3000-1000)/1000 = 2s => 125/s. __name__ dropped.
	var v float64
	for _, x := range got {
		v = x
	}
	if len(got) != 1 || math.Abs(v-125) > 1e-9 {
		t.Fatalf("rate(c[5m]) = %v, want ~125", got)
	}
}

func TestOverTime(t *testing.T) {
	eng := promql.NewEngine(buildDB(t))
	cases := map[string]float64{
		`avg_over_time(m{job="b"}[5m])`:   20,
		`sum_over_time(m{job="b"}[5m])`:   60,
		`count_over_time(m{job="b"}[5m])`: 3,
		`max_over_time(m{job="b"}[5m])`:   30,
		`min_over_time(m{job="b"}[5m])`:   10,
	}
	for q, want := range cases {
		got := vectorMap(t, instant(t, eng, q, 3000))
		var v float64
		for _, x := range got {
			v = x
		}
		if math.Abs(v-want) > 1e-9 {
			t.Errorf("%s = %v, want %v", q, v, want)
		}
	}
}

func TestVectorScalarArithmeticAndCompare(t *testing.T) {
	eng := promql.NewEngine(buildDB(t))
	got := vectorMap(t, instant(t, eng, "m * 2", 3000))
	if got[`m{job="b"}`] != 60 {
		t.Errorf("m*2 b = %v, want 60", got)
	}
	filtered := vectorMap(t, instant(t, eng, "m > 20", 3000))
	if len(filtered) != 1 || filtered[`m{job="b"}`] != 30 {
		t.Errorf("m>20 = %v, want only b=30", filtered)
	}
}

func TestRangeQueryMatrix(t *testing.T) {
	eng := promql.NewEngine(buildDB(t))
	res, err := eng.RangeQuery(context.Background(), `m{job="b"}`, 1000, 3000, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if res.Type != promql.ValueMatrix || len(res.Matrix) != 1 {
		t.Fatalf("range result = %v with %d series, want 1-series matrix", res.Type, len(res.Matrix))
	}
	pts := res.Matrix[0].Points
	if len(pts) != 3 || pts[0].V != 10 || pts[2].V != 30 {
		t.Fatalf("range points = %v, want 10,20,30", pts)
	}
}

func TestBareRangeVector(t *testing.T) {
	eng := promql.NewEngine(buildDB(t))
	res := instant(t, eng, `m{job="b"}[5m]`, 3000)
	if res.Type != promql.ValueMatrix || len(res.Matrix) != 1 || len(res.Matrix[0].Points) != 3 {
		t.Fatalf("bare range vector = %v", res)
	}
}

// TestQueryStorageParity asserts the query engine's instant selector agrees with
// a direct storage Select for the latest sample — two paths that must match.
func TestQueryStorageParity(t *testing.T) {
	db := buildDB(t)
	eng := promql.NewEngine(db)
	ts := int64(3000)

	got := vectorMap(t, instant(t, eng, `m`, ts))

	// Independently compute via storage.
	m, _ := model.NewMatcher(model.MatchEqual, model.MetricName, "m")
	ss := db.Querier().Select(ts-300000, ts, *m)
	want := map[string]float64{}
	for ss.Next() {
		s := ss.At()
		samples := s.Samples()
		want[s.Labels().String()] = samples[len(samples)-1].V
	}
	if len(got) != len(want) {
		t.Fatalf("parity: engine %v vs storage %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("parity mismatch for %s: engine %v storage %v", k, got[k], v)
		}
	}
}

func TestUnaryMinusPowPrecedence(t *testing.T) {
	eng := promql.NewEngine(buildDB(t))
	cases := map[string]float64{
		"-2 ^ 2": -4,   // unary minus binds looser than ^ : -(2^2)
		"-2 * 3": -6,   // but tighter than * : (-2)*3
		"2 ^ -2": 0.25, // negative exponent
	}
	for q, want := range cases {
		r := instant(t, eng, q, 3000)
		if r.Scalar.V != want {
			t.Errorf("%s = %v, want %v", q, r.Scalar.V, want)
		}
	}
}

func TestScalarComparisonRejected(t *testing.T) {
	eng := promql.NewEngine(buildDB(t))
	for _, q := range []string{"1 == 2", "5 > 10", "3 != 3"} {
		if _, err := eng.InstantQuery(context.Background(), q, 0); err == nil {
			t.Errorf("scalar-scalar comparison %q should error (bool modifier unsupported)", q)
		}
	}
}

func TestRangeQueryGuards(t *testing.T) {
	eng := promql.NewEngine(buildDB(t))
	// Excessive resolution must be rejected (DoS guard).
	if _, err := eng.RangeQuery(context.Background(), "1", 0, 100_000_000, 1); err == nil {
		t.Error("expected error for excessive step count")
	}
	// end before start must be rejected.
	if _, err := eng.RangeQuery(context.Background(), "1", 100, 10, 1); err == nil {
		t.Error("expected error for end before start")
	}
	// A reasonable range still works.
	if _, err := eng.RangeQuery(context.Background(), "1", 0, 1000, 100); err != nil {
		t.Errorf("reasonable range query failed: %v", err)
	}
}

func TestParseErrors(t *testing.T) {
	eng := promql.NewEngine(buildDB(t))
	for _, q := range []string{`m{`, `sum(`, `* 3`, `rate()`, `m[5m`} {
		if _, err := eng.InstantQuery(context.Background(), q, 0); err == nil {
			t.Errorf("expected parse error for %q", q)
		}
	}
}
