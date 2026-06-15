package promql

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"time"

	"github.com/pod32g/omni-metrics/internal/model"
)

// evalCall dispatches a function call. Functions fall into a few shapes: ones
// taking a range vector, simple element-wise instant-vector math, time helpers,
// label rewriters, and a handful of special cases.
func (ev *evaluator) evalCall(c *Call, ts int64) (evalResult, error) {
	switch c.Func {
	case "rate", "irate", "increase", "delta", "idelta", "deriv", "changes", "resets",
		"sum_over_time", "avg_over_time", "min_over_time", "max_over_time", "count_over_time",
		"stddev_over_time", "stdvar_over_time", "last_over_time", "present_over_time":
		return ev.callRangeFunc(c, ts)
	case "predict_linear":
		return ev.callPredictLinear(c, ts)
	case "quantile_over_time":
		return ev.callQuantileOverTime(c, ts)
	case "histogram_quantile":
		return ev.callHistogramQuantile(c, ts)
	case "label_replace":
		return ev.callLabelReplace(c, ts)
	case "label_join":
		return ev.callLabelJoin(c, ts)
	case "scalar":
		return ev.callScalar(c, ts)
	case "vector":
		return ev.callVector(c, ts)
	case "time":
		return evalResult{kind: ValueScalar, scalar: float64(ts) / 1000}, nil
	case "timestamp":
		return ev.callTimestamp(c, ts)
	case "sort", "sort_desc":
		return ev.callSort(c, ts)
	case "absent":
		return ev.callAbsent(c, ts)
	case "absent_over_time":
		return ev.callAbsentOverTime(c, ts)
	case "clamp":
		return ev.callClamp(c, ts)
	case "clamp_max", "clamp_min":
		return ev.callClampMaxMin(c, ts)
	case "round":
		return ev.callRound(c, ts)
	case "day_of_month", "day_of_week", "day_of_year", "days_in_month",
		"hour", "minute", "month", "year":
		return ev.callTimeFunc(c, ts)
	default:
		return ev.callSimple(c, ts)
	}
}

func (ev *evaluator) evalArg(c *Call, i int, ts int64) (evalResult, error) {
	if i >= len(c.Args) {
		return evalResult{}, fmt.Errorf("%s: missing argument %d", c.Func, i+1)
	}
	return ev.evalExpr(c.Args[i], ts)
}

func (ev *evaluator) argVector(c *Call, i int, ts int64) (Vector, error) {
	r, err := ev.evalArg(c, i, ts)
	if err != nil {
		return nil, err
	}
	if r.kind != ValueVector {
		return nil, fmt.Errorf("%s: argument %d must be an instant vector", c.Func, i+1)
	}
	return r.vector, nil
}

func (ev *evaluator) argScalar(c *Call, i int, ts int64) (float64, error) {
	r, err := ev.evalArg(c, i, ts)
	if err != nil {
		return 0, err
	}
	if r.kind != ValueScalar {
		return 0, fmt.Errorf("%s: argument %d must be a scalar", c.Func, i+1)
	}
	return r.scalar, nil
}

func (ev *evaluator) argString(c *Call, i int) (string, error) {
	if i >= len(c.Args) {
		return "", fmt.Errorf("%s: missing string argument %d", c.Func, i+1)
	}
	s, ok := c.Args[i].(*StringLiteral)
	if !ok {
		return "", fmt.Errorf("%s: argument %d must be a string literal", c.Func, i+1)
	}
	return s.Val, nil
}

func (ev *evaluator) callRangeFunc(c *Call, ts int64) (evalResult, error) {
	r, err := ev.evalArg(c, 0, ts)
	if err != nil {
		return evalResult{}, err
	}
	if r.kind != ValueMatrix {
		return evalResult{}, fmt.Errorf("%s expects a range vector argument", c.Func)
	}
	return evalResult{kind: ValueVector, vector: applyRangeFunc(c.Func, r.matrix, ts)}, nil
}

// simpleFuncs are element-wise instant-vector → instant-vector math functions.
var simpleFuncs = map[string]func(float64) float64{
	"abs":   math.Abs,
	"ceil":  math.Ceil,
	"floor": math.Floor,
	"exp":   math.Exp,
	"ln":    math.Log,
	"log2":  math.Log2,
	"log10": math.Log10,
	"sqrt":  math.Sqrt,
	"sgn": func(v float64) float64 {
		if math.IsNaN(v) {
			return v
		}
		if v > 0 {
			return 1
		}
		if v < 0 {
			return -1
		}
		return 0
	},
}

func (ev *evaluator) callSimple(c *Call, ts int64) (evalResult, error) {
	fn, ok := simpleFuncs[c.Func]
	if !ok {
		return evalResult{}, fmt.Errorf("unknown function %q", c.Func)
	}
	vec, err := ev.argVector(c, 0, ts)
	if err != nil {
		return evalResult{}, err
	}
	out := make(Vector, 0, len(vec))
	for _, s := range vec {
		out = append(out, VectorSample{Metric: dropMetricName(s.Metric), T: ts, V: fn(s.V)})
	}
	return evalResult{kind: ValueVector, vector: out}, nil
}

// timeFuncs map a UTC time to the integer component PromQL reports.
var timeFuncs = map[string]func(time.Time) float64{
	"day_of_month":  func(t time.Time) float64 { return float64(t.Day()) },
	"day_of_week":   func(t time.Time) float64 { return float64(int(t.Weekday())) },
	"day_of_year":   func(t time.Time) float64 { return float64(t.YearDay()) },
	"days_in_month": func(t time.Time) float64 { return float64(daysInMonth(t.Year(), int(t.Month()))) },
	"hour":          func(t time.Time) float64 { return float64(t.Hour()) },
	"minute":        func(t time.Time) float64 { return float64(t.Minute()) },
	"month":         func(t time.Time) float64 { return float64(int(t.Month())) },
	"year":          func(t time.Time) float64 { return float64(t.Year()) },
}

func daysInMonth(year, month int) int {
	return time.Date(year, time.Month(month)+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// callTimeFunc implements hour()/day_of_week()/etc.: with no argument they use the
// evaluation time; with a vector each value is read as a Unix-seconds timestamp.
func (ev *evaluator) callTimeFunc(c *Call, ts int64) (evalResult, error) {
	compute := timeFuncs[c.Func]
	if len(c.Args) == 0 {
		tm := time.Unix(ts/1000, 0).UTC()
		return evalResult{kind: ValueVector, vector: Vector{{Metric: model.Labels{}, T: ts, V: compute(tm)}}}, nil
	}
	vec, err := ev.argVector(c, 0, ts)
	if err != nil {
		return evalResult{}, err
	}
	out := make(Vector, 0, len(vec))
	for _, s := range vec {
		tm := time.Unix(int64(s.V), 0).UTC()
		out = append(out, VectorSample{Metric: dropMetricName(s.Metric), T: ts, V: compute(tm)})
	}
	return evalResult{kind: ValueVector, vector: out}, nil
}

func (ev *evaluator) callClamp(c *Call, ts int64) (evalResult, error) {
	vec, err := ev.argVector(c, 0, ts)
	if err != nil {
		return evalResult{}, err
	}
	mn, err := ev.argScalar(c, 1, ts)
	if err != nil {
		return evalResult{}, err
	}
	mx, err := ev.argScalar(c, 2, ts)
	if err != nil {
		return evalResult{}, err
	}
	out := make(Vector, 0, len(vec))
	for _, s := range vec {
		out = append(out, VectorSample{Metric: dropMetricName(s.Metric), T: ts, V: math.Max(mn, math.Min(mx, s.V))})
	}
	return evalResult{kind: ValueVector, vector: out}, nil
}

func (ev *evaluator) callClampMaxMin(c *Call, ts int64) (evalResult, error) {
	vec, err := ev.argVector(c, 0, ts)
	if err != nil {
		return evalResult{}, err
	}
	lim, err := ev.argScalar(c, 1, ts)
	if err != nil {
		return evalResult{}, err
	}
	out := make(Vector, 0, len(vec))
	for _, s := range vec {
		v := s.V
		if c.Func == "clamp_max" {
			v = math.Min(v, lim)
		} else {
			v = math.Max(v, lim)
		}
		out = append(out, VectorSample{Metric: dropMetricName(s.Metric), T: ts, V: v})
	}
	return evalResult{kind: ValueVector, vector: out}, nil
}

func (ev *evaluator) callRound(c *Call, ts int64) (evalResult, error) {
	vec, err := ev.argVector(c, 0, ts)
	if err != nil {
		return evalResult{}, err
	}
	toNearest := 1.0
	if len(c.Args) > 1 {
		if toNearest, err = ev.argScalar(c, 1, ts); err != nil {
			return evalResult{}, err
		}
	}
	if toNearest == 0 {
		toNearest = 1
	}
	out := make(Vector, 0, len(vec))
	for _, s := range vec {
		out = append(out, VectorSample{Metric: dropMetricName(s.Metric), T: ts, V: math.Floor(s.V/toNearest+0.5) * toNearest})
	}
	return evalResult{kind: ValueVector, vector: out}, nil
}

func (ev *evaluator) callScalar(c *Call, ts int64) (evalResult, error) {
	vec, err := ev.argVector(c, 0, ts)
	if err != nil {
		return evalResult{}, err
	}
	if len(vec) != 1 {
		return evalResult{kind: ValueScalar, scalar: math.NaN()}, nil
	}
	return evalResult{kind: ValueScalar, scalar: vec[0].V}, nil
}

func (ev *evaluator) callVector(c *Call, ts int64) (evalResult, error) {
	s, err := ev.argScalar(c, 0, ts)
	if err != nil {
		return evalResult{}, err
	}
	return evalResult{kind: ValueVector, vector: Vector{{Metric: model.Labels{}, T: ts, V: s}}}, nil
}

func (ev *evaluator) callTimestamp(c *Call, ts int64) (evalResult, error) {
	vec, err := ev.argVector(c, 0, ts)
	if err != nil {
		return evalResult{}, err
	}
	out := make(Vector, 0, len(vec))
	for _, s := range vec {
		out = append(out, VectorSample{Metric: dropMetricName(s.Metric), T: ts, V: float64(s.T) / 1000})
	}
	return evalResult{kind: ValueVector, vector: out}, nil
}

func (ev *evaluator) callSort(c *Call, ts int64) (evalResult, error) {
	vec, err := ev.argVector(c, 0, ts)
	if err != nil {
		return evalResult{}, err
	}
	out := append(Vector(nil), vec...)
	desc := c.Func == "sort_desc"
	sort.SliceStable(out, func(i, j int) bool {
		if desc {
			return out[i].V > out[j].V
		}
		return out[i].V < out[j].V
	})
	return evalResult{kind: ValueVector, vector: out}, nil
}

func (ev *evaluator) callAbsent(c *Call, ts int64) (evalResult, error) {
	vec, err := ev.argVector(c, 0, ts)
	if err != nil {
		return evalResult{}, err
	}
	if len(vec) > 0 {
		return evalResult{kind: ValueVector, vector: Vector{}}, nil
	}
	return evalResult{kind: ValueVector, vector: Vector{{Metric: absentLabels(c.Args[0]), T: ts, V: 1}}}, nil
}

func (ev *evaluator) callAbsentOverTime(c *Call, ts int64) (evalResult, error) {
	r, err := ev.evalArg(c, 0, ts)
	if err != nil {
		return evalResult{}, err
	}
	if r.kind != ValueMatrix {
		return evalResult{}, fmt.Errorf("absent_over_time expects a range vector")
	}
	has := false
	for _, s := range r.matrix {
		if len(s.Points) > 0 {
			has = true
			break
		}
	}
	if has {
		return evalResult{kind: ValueVector, vector: Vector{}}, nil
	}
	return evalResult{kind: ValueVector, vector: Vector{{Metric: absentLabels(c.Args[0]), T: ts, V: 1}}}, nil
}

// absentLabels synthesizes the label set absent()/absent_over_time() returns,
// carrying the equality matchers of the (possibly range) selector argument.
func absentLabels(arg Expr) model.Labels {
	var vs *VectorSelector
	switch a := arg.(type) {
	case *VectorSelector:
		vs = a
	case *MatrixSelector:
		vs = a.VS
	default:
		return model.Labels{}
	}
	m := map[string]string{}
	for _, mt := range vs.Matchers {
		if mt.Type == model.MatchEqual && mt.Name != model.MetricName {
			m[mt.Name] = mt.Value
		}
	}
	return model.FromMap(m)
}

func (ev *evaluator) callPredictLinear(c *Call, ts int64) (evalResult, error) {
	r, err := ev.evalArg(c, 0, ts)
	if err != nil {
		return evalResult{}, err
	}
	if r.kind != ValueMatrix {
		return evalResult{}, fmt.Errorf("predict_linear expects a range vector")
	}
	secs, err := ev.argScalar(c, 1, ts)
	if err != nil {
		return evalResult{}, err
	}
	var out Vector
	for _, s := range r.matrix {
		if len(s.Points) < 2 {
			continue
		}
		slope, intercept := linearRegression(s.Points, ts)
		out = append(out, VectorSample{Metric: dropMetricName(s.Metric), T: ts, V: slope*secs + intercept})
	}
	return evalResult{kind: ValueVector, vector: out}, nil
}

func (ev *evaluator) callQuantileOverTime(c *Call, ts int64) (evalResult, error) {
	phi, err := ev.argScalar(c, 0, ts)
	if err != nil {
		return evalResult{}, err
	}
	r, err := ev.evalArg(c, 1, ts)
	if err != nil {
		return evalResult{}, err
	}
	if r.kind != ValueMatrix {
		return evalResult{}, fmt.Errorf("quantile_over_time expects a range vector")
	}
	var out Vector
	for _, s := range r.matrix {
		if len(s.Points) == 0 {
			continue
		}
		vals := make([]float64, len(s.Points))
		for i, p := range s.Points {
			vals[i] = p.V
		}
		out = append(out, VectorSample{Metric: dropMetricName(s.Metric), T: ts, V: quantile(phi, vals)})
	}
	return evalResult{kind: ValueVector, vector: out}, nil
}

func (ev *evaluator) callLabelReplace(c *Call, ts int64) (evalResult, error) {
	vec, err := ev.argVector(c, 0, ts)
	if err != nil {
		return evalResult{}, err
	}
	dst, err := ev.argString(c, 1)
	if err != nil {
		return evalResult{}, err
	}
	repl, err := ev.argString(c, 2)
	if err != nil {
		return evalResult{}, err
	}
	src, err := ev.argString(c, 3)
	if err != nil {
		return evalResult{}, err
	}
	rxStr, err := ev.argString(c, 4)
	if err != nil {
		return evalResult{}, err
	}
	rx, err := regexp.Compile("^(?:" + rxStr + ")$")
	if err != nil {
		return evalResult{}, fmt.Errorf("label_replace: invalid regex %q: %w", rxStr, err)
	}
	out := make(Vector, 0, len(vec))
	for _, s := range vec {
		srcVal := s.Metric.Get(src)
		m := s.Metric
		if idx := rx.FindStringSubmatchIndex(srcVal); idx != nil {
			res := string(rx.ExpandString(nil, repl, srcVal, idx))
			m = setOrDeleteLabel(s.Metric, dst, res)
		}
		out = append(out, VectorSample{Metric: m, T: ts, V: s.V})
	}
	return evalResult{kind: ValueVector, vector: out}, nil
}

func (ev *evaluator) callLabelJoin(c *Call, ts int64) (evalResult, error) {
	vec, err := ev.argVector(c, 0, ts)
	if err != nil {
		return evalResult{}, err
	}
	dst, err := ev.argString(c, 1)
	if err != nil {
		return evalResult{}, err
	}
	sep, err := ev.argString(c, 2)
	if err != nil {
		return evalResult{}, err
	}
	srcLabels := make([]string, 0, len(c.Args)-3)
	for i := 3; i < len(c.Args); i++ {
		s, err := ev.argString(c, i)
		if err != nil {
			return evalResult{}, err
		}
		srcLabels = append(srcLabels, s)
	}
	out := make(Vector, 0, len(vec))
	for _, s := range vec {
		parts := make([]string, len(srcLabels))
		for i, l := range srcLabels {
			parts[i] = s.Metric.Get(l)
		}
		out = append(out, VectorSample{Metric: setOrDeleteLabel(s.Metric, dst, joinStrings(parts, sep)), T: ts, V: s.V})
	}
	return evalResult{kind: ValueVector, vector: out}, nil
}

// callHistogramQuantile computes the phi-quantile from a set of le-bucketed
// series (the conventional <metric>_bucket{le="..."} layout).
func (ev *evaluator) callHistogramQuantile(c *Call, ts int64) (evalResult, error) {
	phi, err := ev.argScalar(c, 0, ts)
	if err != nil {
		return evalResult{}, err
	}
	vec, err := ev.argVector(c, 1, ts)
	if err != nil {
		return evalResult{}, err
	}
	type grp struct {
		labels  model.Labels
		buckets []bucket
	}
	order := []string{}
	groups := map[string]*grp{}
	for _, s := range vec {
		le := s.Metric.Get("le")
		if le == "" {
			continue // not a bucket series
		}
		upper, perr := parseLe(le)
		if perr != nil {
			continue
		}
		key := dropLabels(s.Metric, model.MetricName, "le")
		ks := key.String()
		g := groups[ks]
		if g == nil {
			g = &grp{labels: key}
			groups[ks] = g
			order = append(order, ks)
		}
		g.buckets = append(g.buckets, bucket{upperBound: upper, count: s.V})
	}
	out := make(Vector, 0, len(order))
	for _, ks := range order {
		g := groups[ks]
		out = append(out, VectorSample{Metric: g.labels, T: ts, V: bucketQuantile(phi, g.buckets)})
	}
	return evalResult{kind: ValueVector, vector: out}, nil
}
