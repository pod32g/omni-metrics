package promql

import (
	"fmt"
	"math"

	"github.com/pod32g/omni-metrics/internal/model"
)

// applyRangeFunc applies a range-vector function to each series, producing an
// instant vector. The metric name is dropped from the result (the output is a
// different quantity than the input). Series with too few points are skipped.
func applyRangeFunc(name string, m Matrix, ts int64) Vector {
	var out Vector
	for _, s := range m {
		v, ok := rangeFuncValue(name, s.Points)
		if !ok {
			continue
		}
		out = append(out, VectorSample{Metric: dropMetricName(s.Metric), T: ts, V: v})
	}
	return out
}

func rangeFuncValue(name string, pts []Point) (float64, bool) {
	switch name {
	case "rate":
		return rateValue(pts, true)
	case "increase":
		d, ok := counterDelta(pts)
		return d, ok
	case "irate":
		return irateValue(pts)
	case "sum_over_time":
		return overTime(pts, "sum")
	case "avg_over_time":
		return overTime(pts, "avg")
	case "min_over_time":
		return overTime(pts, "min")
	case "max_over_time":
		return overTime(pts, "max")
	case "count_over_time":
		return overTime(pts, "count")
	default:
		return 0, false
	}
}

// counterDelta sums positive increments, treating any decrease as a counter
// reset (the post-reset value is the increase since the reset).
func counterDelta(pts []Point) (float64, bool) {
	if len(pts) < 2 {
		return 0, false
	}
	delta := 0.0
	for i := 1; i < len(pts); i++ {
		cur, prev := pts[i].V, pts[i-1].V
		if cur >= prev {
			delta += cur - prev
		} else {
			delta += cur // reset
		}
	}
	return delta, true
}

// rateValue is counterDelta per second over the sampled window.
func rateValue(pts []Point, _ bool) (float64, bool) {
	delta, ok := counterDelta(pts)
	if !ok {
		return 0, false
	}
	dtSec := float64(pts[len(pts)-1].T-pts[0].T) / 1000
	if dtSec <= 0 {
		return 0, false
	}
	return delta / dtSec, true
}

// irateValue is the per-second rate over the final two samples.
func irateValue(pts []Point) (float64, bool) {
	n := len(pts)
	if n < 2 {
		return 0, false
	}
	cur, prev := pts[n-1], pts[n-2]
	d := cur.V - prev.V
	if d < 0 {
		d = cur.V // reset
	}
	dtSec := float64(cur.T-prev.T) / 1000
	if dtSec <= 0 {
		return 0, false
	}
	return d / dtSec, true
}

func overTime(pts []Point, kind string) (float64, bool) {
	if len(pts) == 0 {
		return 0, false
	}
	switch kind {
	case "count":
		return float64(len(pts)), true
	case "sum", "avg":
		sum := 0.0
		for _, p := range pts {
			sum += p.V
		}
		if kind == "avg" {
			return sum / float64(len(pts)), true
		}
		return sum, true
	case "min":
		m := math.Inf(1)
		for _, p := range pts {
			if p.V < m {
				m = p.V
			}
		}
		return m, true
	case "max":
		m := math.Inf(-1)
		for _, p := range pts {
			if p.V > m {
				m = p.V
			}
		}
		return m, true
	default:
		return 0, false
	}
}

// aggregate groups an instant vector and reduces each group with op.
func aggregate(op string, in Vector, grouping []string, without bool, ts int64) (Vector, error) {
	type group struct {
		labels model.Labels
		count  int
		sum    float64
		min    float64
		max    float64
	}
	order := []string{}
	groups := map[string]*group{}
	set := map[string]bool{}
	for _, g := range grouping {
		set[g] = true
	}

	for _, s := range in {
		key := groupLabels(s.Metric, set, without)
		ks := key.String()
		g := groups[ks]
		if g == nil {
			g = &group{labels: key, min: s.V, max: s.V}
			groups[ks] = g
			order = append(order, ks)
		}
		g.count++
		g.sum += s.V
		if s.V < g.min {
			g.min = s.V
		}
		if s.V > g.max {
			g.max = s.V
		}
	}

	out := make(Vector, 0, len(order))
	for _, ks := range order {
		g := groups[ks]
		var v float64
		switch op {
		case "sum":
			v = g.sum
		case "avg":
			v = g.sum / float64(g.count)
		case "min":
			v = g.min
		case "max":
			v = g.max
		case "count":
			v = float64(g.count)
		default:
			return nil, fmt.Errorf("unknown aggregation %q", op)
		}
		out = append(out, VectorSample{Metric: g.labels, T: ts, V: v})
	}
	return out, nil
}

// groupLabels projects a label set onto the grouping key. by() keeps only the
// listed labels; without() keeps everything except the listed labels and the
// metric name.
func groupLabels(l model.Labels, set map[string]bool, without bool) model.Labels {
	var out model.Labels
	for _, lbl := range l {
		if without {
			if set[lbl.Name] || lbl.Name == model.MetricName {
				continue
			}
		} else {
			if !set[lbl.Name] {
				continue
			}
		}
		out = append(out, lbl)
	}
	return out
}

func dropMetricName(l model.Labels) model.Labels {
	var out model.Labels
	for _, lbl := range l {
		if lbl.Name == model.MetricName {
			continue
		}
		out = append(out, lbl)
	}
	return out
}

// applyBinary implements scalar/vector arithmetic and comparison.
func applyBinary(op tokenType, l, r evalResult) (evalResult, error) {
	switch {
	case l.kind == ValueScalar && r.kind == ValueScalar:
		if isComparison(op) {
			// A bool-less comparison between two scalars has no defined value in
			// this subset (the bool modifier is deferred); reject it rather than
			// silently returning the left operand.
			return evalResult{}, fmt.Errorf("comparisons between two scalars must use a BOOL modifier (unsupported)")
		}
		v, _ := scalarBinop(op, l.scalar, r.scalar)
		return evalResult{kind: ValueScalar, scalar: v}, nil
	case l.kind == ValueScalar && r.kind == ValueVector:
		return evalResult{kind: ValueVector, vector: scalarVectorOp(op, l.scalar, r.vector, true)}, nil
	case l.kind == ValueVector && r.kind == ValueScalar:
		return evalResult{kind: ValueVector, vector: scalarVectorOp(op, r.scalar, l.vector, false)}, nil
	case l.kind == ValueVector && r.kind == ValueVector:
		return evalResult{kind: ValueVector, vector: vectorVectorOp(op, l.vector, r.vector)}, nil
	default:
		return evalResult{}, fmt.Errorf("binary operator not supported between %v and %v", l.kind, r.kind)
	}
}

// scalarBinop computes scalar (op) scalar. For comparisons it returns the lhs
// value and whether the comparison held.
func scalarBinop(op tokenType, a, b float64) (float64, bool) {
	switch op {
	case tAdd:
		return a + b, true
	case tSub:
		return a - b, true
	case tMul:
		return a * b, true
	case tDiv:
		return a / b, true
	case tMod:
		return math.Mod(a, b), true
	case tPow:
		return math.Pow(a, b), true
	case tEQLCmp:
		return a, a == b
	case tNEQ:
		return a, a != b
	case tGTR:
		return a, a > b
	case tLSS:
		return a, a < b
	case tGTE:
		return a, a >= b
	case tLTE:
		return a, a <= b
	default:
		return 0, false
	}
}

// scalarVectorOp applies a scalar against each vector element. scalarLeft marks
// whether the scalar is the left operand. Comparisons filter the vector.
func scalarVectorOp(op tokenType, scalar float64, vec Vector, scalarLeft bool) Vector {
	cmp := isComparison(op)
	var out Vector
	for _, s := range vec {
		var v float64
		var keep bool
		if scalarLeft {
			v, keep = scalarBinop(op, scalar, s.V)
		} else {
			v, keep = scalarBinop(op, s.V, scalar)
		}
		if cmp {
			if keep {
				out = append(out, VectorSample{Metric: s.Metric, T: s.T, V: s.V})
			}
			continue
		}
		out = append(out, VectorSample{Metric: s.Metric, T: s.T, V: v})
	}
	return out
}

// vectorVectorOp matches elements by their labels excluding __name__. Arithmetic
// results carry the matched labels (no __name__); comparisons keep the lhs
// element when the comparison holds.
func vectorVectorOp(op tokenType, lhs, rhs Vector) Vector {
	cmp := isComparison(op)
	rIndex := map[string]VectorSample{}
	for _, s := range rhs {
		rIndex[dropMetricName(s.Metric).String()] = s
	}
	var out Vector
	for _, ls := range lhs {
		key := dropMetricName(ls.Metric).String()
		rs, ok := rIndex[key]
		if !ok {
			continue
		}
		v, keep := scalarBinop(op, ls.V, rs.V)
		if cmp {
			if keep {
				out = append(out, VectorSample{Metric: ls.Metric, T: ls.T, V: ls.V})
			}
			continue
		}
		out = append(out, VectorSample{Metric: dropMetricName(ls.Metric), T: ls.T, V: v})
	}
	return out
}

func isComparison(op tokenType) bool {
	switch op {
	case tEQLCmp, tNEQ, tGTR, tLSS, tGTE, tLTE:
		return true
	}
	return false
}
