package promql_test

import (
	"context"
	"math"
	"testing"

	"github.com/pod32g/omni-metrics/internal/model"
	"github.com/pod32g/omni-metrics/internal/promql"
	"github.com/pod32g/omni-metrics/internal/tsdb"
)

// buildExtDB seeds data for the extended PromQL features: a counter, two
// metrics for vector matching, and a histogram.
func buildExtDB(t *testing.T) *tsdb.DB {
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
	// m: three series (job=a:3, job=a/inst=2:5, job=b:30 at t=3000)
	add(model.FromStrings(model.MetricName, "m", "job", "a"), [2]float64{1000, 1}, [2]float64{2000, 2}, [2]float64{3000, 3})
	add(model.FromStrings(model.MetricName, "m", "job", "a", "inst", "2"), [2]float64{3000, 5})
	add(model.FromStrings(model.MetricName, "m", "job", "b"), [2]float64{1000, 10}, [2]float64{2000, 20}, [2]float64{3000, 30})
	// counter
	add(model.FromStrings(model.MetricName, "c", "job", "a"), [2]float64{1000, 0}, [2]float64{2000, 100}, [2]float64{3000, 250})
	// vector matching: a{x,y} many to b{x} one
	add(model.FromStrings(model.MetricName, "a", "x", "1", "y", "p"), [2]float64{3000, 10})
	add(model.FromStrings(model.MetricName, "a", "x", "1", "y", "q"), [2]float64{3000, 20})
	add(model.FromStrings(model.MetricName, "b", "x", "1"), [2]float64{3000, 2})
	// set ops
	add(model.FromStrings(model.MetricName, "up", "job", "a"), [2]float64{3000, 1})
	add(model.FromStrings(model.MetricName, "up", "job", "b"), [2]float64{3000, 1})
	add(model.FromStrings(model.MetricName, "down", "job", "a"), [2]float64{3000, 1})
	// histogram buckets: cumulative 1,3,4,5
	hb := func(le string, v float64) {
		add(model.FromStrings(model.MetricName, "h_bucket", "le", le), [2]float64{3000, v})
	}
	hb("0.1", 1)
	hb("0.5", 3)
	hb("1", 4)
	hb("+Inf", 5)
	if err := app.Commit(); err != nil {
		t.Fatal(err)
	}
	return db
}

// one returns the single value of a one-element vector result.
func one(t *testing.T, r promql.Result) float64 {
	t.Helper()
	if r.Type != promql.ValueVector || len(r.Vector) != 1 {
		t.Fatalf("want 1-element vector, got type=%v len=%d (%v)", r.Type, len(r.Vector), r.Vector)
	}
	return r.Vector[0].V
}

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func TestExtMathFuncs(t *testing.T) {
	eng := promql.NewEngine(buildExtDB(t))
	cases := map[string]float64{
		"abs(vector(-4))":         4,
		"ceil(vector(1.2))":       2,
		"floor(vector(1.8))":      1,
		"sqrt(vector(9))":         3,
		"round(vector(2.4))":      2,
		"round(vector(2.6))":      3,
		"clamp(vector(15),0,10)":  10,
		"clamp_max(vector(15),9)": 9,
		"clamp_min(vector(3),5)":  5,
		"exp(vector(0))":          1,
		"ln(vector(1))":           0,
		"sgn(vector(-7))":         -1,
	}
	for q, want := range cases {
		if got := one(t, instant(t, eng, q, 3000)); !approx(got, want) {
			t.Errorf("%s = %v, want %v", q, got, want)
		}
	}
}

func TestExtTimeFuncs(t *testing.T) {
	eng := promql.NewEngine(buildExtDB(t))
	cases := map[string]float64{
		"year(vector(0))":         1970,
		"month(vector(0))":        1,
		"day_of_month(vector(0))": 1,
		"day_of_week(vector(0))":  4, // 1970-01-01 was a Thursday
		"hour(vector(0))":         0,
	}
	for q, want := range cases {
		if got := one(t, instant(t, eng, q, 3000)); got != want {
			t.Errorf("%s = %v, want %v", q, got, want)
		}
	}
}

func TestExtScalarVector(t *testing.T) {
	eng := promql.NewEngine(buildExtDB(t))
	r := instant(t, eng, `scalar(m{job="b"})`, 3000)
	if r.Type != promql.ValueScalar || r.Scalar.V != 30 {
		t.Errorf("scalar(m{job=b}) = %v, want 30", r.Scalar.V)
	}
	r = instant(t, eng, "scalar(m)", 3000) // multiple elements => NaN
	if !math.IsNaN(r.Scalar.V) {
		t.Errorf("scalar(m) = %v, want NaN", r.Scalar.V)
	}
}

func TestExtBoolModifier(t *testing.T) {
	eng := promql.NewEngine(buildExtDB(t))
	if got := one(t, instant(t, eng, `m{job="b"} > bool 20`, 3000)); got != 1 {
		t.Errorf("m>bool 20 = %v, want 1", got)
	}
	if got := one(t, instant(t, eng, `m{job="b"} < bool 20`, 3000)); got != 0 {
		t.Errorf("m<bool 20 = %v, want 0", got)
	}
	r := instant(t, eng, "1 == bool 1", 3000)
	if r.Type != promql.ValueScalar || r.Scalar.V != 1 {
		t.Errorf("1==bool 1 = %v, want 1", r.Scalar.V)
	}
}

func TestExtAggregations(t *testing.T) {
	eng := promql.NewEngine(buildExtDB(t))
	// topk(1, m) keeps the original (named) series with the highest value.
	top := vectorMap(t, instant(t, eng, "topk(1, m)", 3000))
	if len(top) != 1 || top[`m{job="b"}`] != 30 {
		t.Errorf("topk(1,m) = %v, want m{job=b}=30", top)
	}
	bot := vectorMap(t, instant(t, eng, "bottomk(1, m)", 3000))
	if len(bot) != 1 || bot[`m{job="a"}`] != 3 {
		t.Errorf("bottomk(1,m) = %v, want m{job=a}=3", bot)
	}
	if got := one(t, instant(t, eng, "quantile(0.5, m)", 3000)); got != 5 {
		t.Errorf("quantile(0.5,m) = %v, want 5", got) // median of [3,5,30]
	}
	if got := one(t, instant(t, eng, "group(m)", 3000)); got != 1 {
		t.Errorf("group(m) = %v, want 1", got)
	}
	// count_values produces one series per distinct value.
	cv := instant(t, eng, `count_values("val", m)`, 3000)
	if cv.Type != promql.ValueVector || len(cv.Vector) != 3 {
		t.Errorf("count_values(m) = %v, want 3 series", cv.Vector)
	}
	// stdvar of [3,5,30]: mean 12.667, population variance ~ 150.889
	if got := one(t, instant(t, eng, "stdvar(m)", 3000)); !approx(got, 150.8888889) {
		t.Errorf("stdvar(m) = %v, want ~150.89", got)
	}
}

func TestExtVectorMatchingGroupLeft(t *testing.T) {
	eng := promql.NewEngine(buildExtDB(t))
	// a / on(x) group_left b : many a's matched to one b on label x.
	got := vectorMap(t, instant(t, eng, "a / on(x) group_left b", 3000))
	if got[`{x="1",y="p"}`] != 5 || got[`{x="1",y="q"}`] != 10 {
		t.Errorf("a/on(x)group_left b = %v, want p=5 q=10", got)
	}
}

func TestExtSetOps(t *testing.T) {
	eng := promql.NewEngine(buildExtDB(t))
	and := vectorMap(t, instant(t, eng, "up and on(job) down", 3000))
	if len(and) != 1 || and[`up{job="a"}`] != 1 {
		t.Errorf("up and on(job) down = %v, want up{job=a}", and)
	}
	unless := vectorMap(t, instant(t, eng, "up unless on(job) down", 3000))
	if len(unless) != 1 || unless[`up{job="b"}`] != 1 {
		t.Errorf("up unless on(job) down = %v, want up{job=b}", unless)
	}
	or := vectorMap(t, instant(t, eng, "up or down", 3000))
	if len(or) != 3 { // up{a}, up{b}, down{a}
		t.Errorf("up or down = %v, want 3 series", or)
	}
}

func TestExtOffsetAndAt(t *testing.T) {
	eng := promql.NewEngine(buildExtDB(t))
	// offset 1s shifts selection back; latest sample <= 2999 for m{job=b} is 20.
	if got := one(t, instant(t, eng, `m{job="b"} offset 1s`, 3000)); got != 20 {
		t.Errorf("m{job=b} offset 1s = %v, want 20", got)
	}
	// @ 2 pins selection to t=2000 => value 20.
	if got := one(t, instant(t, eng, `m{job="b"} @ 2.000`, 3000)); got != 20 {
		t.Errorf("m{job=b} @ 2 = %v, want 20", got)
	}
}

func TestExtSubquery(t *testing.T) {
	eng := promql.NewEngine(buildExtDB(t))
	// max over a 3s window sampled each 1s => max of {10,20,30} = 30.
	if got := one(t, instant(t, eng, `max_over_time(m{job="b"}[3s:1s])`, 3000)); got != 30 {
		t.Errorf("max_over_time(m[3s:1s]) = %v, want 30", got)
	}
}

func TestExtRangeFuncs(t *testing.T) {
	eng := promql.NewEngine(buildExtDB(t))
	cases := map[string]float64{
		"delta(c[5m])":   250, // 250-0
		"idelta(c[5m])":  150, // 250-100
		"changes(c[5m])": 2,
		"resets(c[5m])":  0,
	}
	for q, want := range cases {
		if got := one(t, instant(t, eng, q, 3000)); got != want {
			t.Errorf("%s = %v, want %v", q, got, want)
		}
	}
	// deriv slope is positive for an increasing counter.
	if got := one(t, instant(t, eng, "deriv(c[5m])", 3000)); got <= 0 {
		t.Errorf("deriv(c[5m]) = %v, want > 0", got)
	}
}

func TestExtHistogramQuantile(t *testing.T) {
	eng := promql.NewEngine(buildExtDB(t))
	// buckets cumulative 1,3,4,5; q=0.5 => rank 2.5 in (0.1,0.5] => 0.1+0.4*1.5/2 = 0.4
	if got := one(t, instant(t, eng, "histogram_quantile(0.5, h_bucket)", 3000)); !approx(got, 0.4) {
		t.Errorf("histogram_quantile(0.5,h_bucket) = %v, want 0.4", got)
	}
}

func TestExtLabelFuncs(t *testing.T) {
	eng := promql.NewEngine(buildExtDB(t))
	got := vectorMap(t, instant(t, eng, `label_replace(m{job="b"}, "dst", "$1", "job", "(.*)")`, 3000))
	if got[`m{dst="b",job="b"}`] != 30 {
		t.Errorf("label_replace = %v, want m{dst=b,job=b}=30", got)
	}
	got = vectorMap(t, instant(t, eng, `label_join(m{job="b"}, "j", "-", "job")`, 3000))
	if got[`m{j="b",job="b"}`] != 30 {
		t.Errorf("label_join = %v, want j=b", got)
	}
}

func TestExtAbsent(t *testing.T) {
	eng := promql.NewEngine(buildExtDB(t))
	if got := one(t, instant(t, eng, `absent(nonexistent{foo="bar"})`, 3000)); got != 1 {
		t.Errorf("absent(nonexistent) = %v, want 1", got)
	}
	r := instant(t, eng, "absent(m)", 3000)
	if r.Type != promql.ValueVector || len(r.Vector) != 0 {
		t.Errorf("absent(m) = %v, want empty", r.Vector)
	}
}

func TestExtRangeQueryWithFuncs(t *testing.T) {
	eng := promql.NewEngine(buildExtDB(t))
	// A range query over rate() must produce a matrix without error.
	res, err := eng.RangeQuery(context.Background(), "rate(c[5m])", 1000, 3000, 1000)
	if err != nil {
		t.Fatalf("range rate: %v", err)
	}
	if res.Type != promql.ValueMatrix {
		t.Errorf("range rate type = %v", res.Type)
	}
}
