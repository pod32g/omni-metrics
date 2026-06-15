package promql

import (
	"fmt"
	"math"
	"sort"
	"strconv"

	"github.com/pod32g/omni-metrics/internal/model"
)

// applyRangeFunc applies a range-vector function to each series, producing an
// instant vector. The metric name is dropped (the output is a different
// quantity) except for last_over_time, which preserves it.
func applyRangeFunc(name string, m Matrix, ts int64) Vector {
	keepName := name == "last_over_time"
	var out Vector
	for _, s := range m {
		v, ok := rangeFuncValue(name, s.Points, ts)
		if !ok {
			continue
		}
		metric := s.Metric
		if !keepName {
			metric = dropMetricName(metric)
		}
		out = append(out, VectorSample{Metric: metric, T: ts, V: v})
	}
	return out
}

func rangeFuncValue(name string, pts []Point, ts int64) (float64, bool) {
	switch name {
	case "rate":
		return rateValue(pts)
	case "increase":
		return counterDelta(pts)
	case "irate":
		return irateValue(pts)
	case "delta":
		return simpleDelta(pts)
	case "idelta":
		return idelta(pts)
	case "deriv":
		if len(pts) < 2 {
			return 0, false
		}
		slope, _ := linearRegression(pts, ts)
		return slope, true
	case "changes":
		return changes(pts)
	case "resets":
		return resets(pts)
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
	case "stddev_over_time":
		return stddevOverTime(pts, false)
	case "stdvar_over_time":
		return stddevOverTime(pts, true)
	case "last_over_time":
		if len(pts) == 0 {
			return 0, false
		}
		return pts[len(pts)-1].V, true
	case "present_over_time":
		if len(pts) == 0 {
			return 0, false
		}
		return 1, true
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

func rateValue(pts []Point) (float64, bool) {
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

// simpleDelta is last-minus-first (gauge delta, no counter-reset handling).
func simpleDelta(pts []Point) (float64, bool) {
	if len(pts) < 2 {
		return 0, false
	}
	return pts[len(pts)-1].V - pts[0].V, true
}

func idelta(pts []Point) (float64, bool) {
	n := len(pts)
	if n < 2 {
		return 0, false
	}
	return pts[n-1].V - pts[n-2].V, true
}

func changes(pts []Point) (float64, bool) {
	if len(pts) == 0 {
		return 0, false
	}
	c := 0.0
	for i := 1; i < len(pts); i++ {
		if pts[i].V != pts[i-1].V && !(math.IsNaN(pts[i].V) && math.IsNaN(pts[i-1].V)) {
			c++
		}
	}
	return c, true
}

func resets(pts []Point) (float64, bool) {
	if len(pts) == 0 {
		return 0, false
	}
	r := 0.0
	for i := 1; i < len(pts); i++ {
		if pts[i].V < pts[i-1].V {
			r++
		}
	}
	return r, true
}

func stddevOverTime(pts []Point, wantVariance bool) (float64, bool) {
	if len(pts) == 0 {
		return 0, false
	}
	vals := make([]float64, len(pts))
	for i, p := range pts {
		vals[i] = p.V
	}
	v := variance(vals)
	if wantVariance {
		return v, true
	}
	return math.Sqrt(v), true
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

func variance(vals []float64) float64 {
	n := float64(len(vals))
	if n == 0 {
		return 0
	}
	mean := 0.0
	for _, x := range vals {
		mean += x
	}
	mean /= n
	v := 0.0
	for _, x := range vals {
		d := x - mean
		v += d * d
	}
	return v / n
}

// aggregate groups an instant vector and reduces each group with op.
func aggregate(op string, in Vector, grouping []string, without bool, ts int64, param float64, paramStr string) (Vector, error) {
	switch op {
	case "count_values":
		return aggregateCountValues(in, grouping, without, ts, paramStr), nil
	case "topk", "bottomk":
		return aggregateTopk(op, in, grouping, without, ts, int(param)), nil
	}

	set := map[string]bool{}
	for _, g := range grouping {
		set[g] = true
	}
	type group struct {
		labels model.Labels
		values []float64
	}
	order := []string{}
	groups := map[string]*group{}
	for _, s := range in {
		key := groupLabels(s.Metric, set, without)
		ks := key.String()
		g := groups[ks]
		if g == nil {
			g = &group{labels: key}
			groups[ks] = g
			order = append(order, ks)
		}
		g.values = append(g.values, s.V)
	}

	out := make(Vector, 0, len(order))
	for _, ks := range order {
		g := groups[ks]
		var v float64
		switch op {
		case "sum":
			for _, x := range g.values {
				v += x
			}
		case "avg":
			for _, x := range g.values {
				v += x
			}
			v /= float64(len(g.values))
		case "min":
			v = g.values[0]
			for _, x := range g.values {
				if x < v {
					v = x
				}
			}
		case "max":
			v = g.values[0]
			for _, x := range g.values {
				if x > v {
					v = x
				}
			}
		case "count":
			v = float64(len(g.values))
		case "group":
			v = 1
		case "stddev":
			v = math.Sqrt(variance(g.values))
		case "stdvar":
			v = variance(g.values)
		case "quantile":
			v = quantile(param, g.values)
		default:
			return nil, fmt.Errorf("unknown aggregation %q", op)
		}
		out = append(out, VectorSample{Metric: g.labels, T: ts, V: v})
	}
	return out, nil
}

// aggregateTopk returns the k highest (topk) or lowest (bottomk) samples per
// group, preserving each sample's original labels (including __name__).
func aggregateTopk(op string, in Vector, grouping []string, without bool, ts int64, k int) Vector {
	set := map[string]bool{}
	for _, g := range grouping {
		set[g] = true
	}
	order := []string{}
	groups := map[string][]VectorSample{}
	for _, s := range in {
		key := groupLabels(s.Metric, set, without).String()
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], s)
	}
	var out Vector
	for _, key := range order {
		members := groups[key]
		sort.SliceStable(members, func(i, j int) bool {
			if op == "topk" {
				return members[i].V > members[j].V
			}
			return members[i].V < members[j].V
		})
		n := k
		if n > len(members) {
			n = len(members)
		}
		if n < 0 {
			n = 0
		}
		for i := 0; i < n; i++ {
			out = append(out, VectorSample{Metric: members[i].Metric, T: ts, V: members[i].V})
		}
	}
	return out
}

// aggregateCountValues counts how many series share each distinct value, emitting
// one series per value labelled with the given label name.
func aggregateCountValues(in Vector, grouping []string, without bool, ts int64, label string) Vector {
	set := map[string]bool{}
	for _, g := range grouping {
		set[g] = true
	}
	order := []string{}
	counts := map[string]*struct {
		labels model.Labels
		count  int
	}{}
	for _, s := range in {
		base := groupLabels(s.Metric, set, without)
		lbls := setLabel(base, label, strconv.FormatFloat(s.V, 'g', -1, 64))
		ks := lbls.String()
		c := counts[ks]
		if c == nil {
			c = &struct {
				labels model.Labels
				count  int
			}{labels: lbls}
			counts[ks] = c
			order = append(order, ks)
		}
		c.count++
	}
	out := make(Vector, 0, len(order))
	for _, ks := range order {
		out = append(out, VectorSample{Metric: counts[ks].labels, T: ts, V: float64(counts[ks].count)})
	}
	return out
}

// groupLabels projects a label set onto the grouping key. by() keeps only the
// listed labels; without() keeps everything except the listed labels and __name__.
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

// applyBinary implements scalar/vector arithmetic, comparison, and set operators.
func applyBinary(b *BinaryExpr, l, r evalResult) (evalResult, error) {
	op := b.Op
	if op == tLAnd || op == tLOr || op == tLUnless {
		if l.kind != ValueVector || r.kind != ValueVector {
			return evalResult{}, fmt.Errorf("set operators require two instant vectors")
		}
		return evalResult{kind: ValueVector, vector: vectorSetOp(op, l.vector, r.vector, b.Matching)}, nil
	}
	switch {
	case l.kind == ValueScalar && r.kind == ValueScalar:
		if isComparison(op) && !b.ReturnBool {
			return evalResult{}, fmt.Errorf("comparisons between two scalars must use a BOOL modifier")
		}
		v, keep := scalarBinop(op, l.scalar, r.scalar)
		if isComparison(op) {
			if keep {
				v = 1
			} else {
				v = 0
			}
		}
		return evalResult{kind: ValueScalar, scalar: v}, nil
	case l.kind == ValueScalar && r.kind == ValueVector:
		return evalResult{kind: ValueVector, vector: scalarVectorOp(op, l.scalar, r.vector, true, b.ReturnBool)}, nil
	case l.kind == ValueVector && r.kind == ValueScalar:
		return evalResult{kind: ValueVector, vector: scalarVectorOp(op, r.scalar, l.vector, false, b.ReturnBool)}, nil
	case l.kind == ValueVector && r.kind == ValueVector:
		return evalResult{kind: ValueVector, vector: vectorVectorOp(b, l.vector, r.vector)}, nil
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

// scalarVectorOp applies a scalar against each vector element. Arithmetic drops
// __name__; a plain comparison filters (keeping matching elements unchanged); a
// bool comparison yields 0/1 for every element.
func scalarVectorOp(op tokenType, scalar float64, vec Vector, scalarLeft, boolMod bool) Vector {
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
			if boolMod {
				bv := 0.0
				if keep {
					bv = 1
				}
				out = append(out, VectorSample{Metric: dropMetricName(s.Metric), T: s.T, V: bv})
			} else if keep {
				out = append(out, VectorSample{Metric: s.Metric, T: s.T, V: s.V})
			}
			continue
		}
		out = append(out, VectorSample{Metric: dropMetricName(s.Metric), T: s.T, V: v})
	}
	return out
}

// vectorVectorOp matches two vectors per the binary expression's on/ignoring and
// group_left/right modifiers and applies the operator.
func vectorVectorOp(b *BinaryExpr, lhs, rhs Vector) Vector {
	op := b.Op
	m := b.Matching
	cmp := isComparison(op)
	card := cardOneToOne
	if m != nil {
		card = m.Card
	}

	emit := func(metric model.Labels, lv, rv float64, lsamp VectorSample) (VectorSample, bool) {
		v, keep := scalarBinop(op, lv, rv)
		if cmp {
			if b.ReturnBool {
				bv := 0.0
				if keep {
					bv = 1
				}
				return VectorSample{Metric: metric, T: lsamp.T, V: bv}, true
			}
			if keep {
				return VectorSample{Metric: lsamp.Metric, T: lsamp.T, V: lsamp.V}, true
			}
			return VectorSample{}, false
		}
		return VectorSample{Metric: metric, T: lsamp.T, V: v}, true
	}

	switch card {
	case cardManyToOne: // group_left: many LHS to one RHS
		rIndex := map[string]VectorSample{}
		for _, s := range rhs {
			rIndex[matchSignature(s.Metric, m, false)] = s
		}
		var out Vector
		for _, ls := range lhs {
			rs, ok := rIndex[matchSignature(ls.Metric, m, false)]
			if !ok {
				continue
			}
			metric := dropMetricName(ls.Metric)
			for _, inc := range m.Include {
				if val := rs.Metric.Get(inc); val != "" {
					metric = setLabel(metric, inc, val)
				}
			}
			if vs, ok := emit(metric, ls.V, rs.V, ls); ok {
				out = append(out, vs)
			}
		}
		return out
	case cardOneToMany: // group_right: one LHS to many RHS
		lIndex := map[string]VectorSample{}
		for _, s := range lhs {
			lIndex[matchSignature(s.Metric, m, false)] = s
		}
		var out Vector
		for _, rs := range rhs {
			ls, ok := lIndex[matchSignature(rs.Metric, m, false)]
			if !ok {
				continue
			}
			metric := dropMetricName(rs.Metric)
			for _, inc := range m.Include {
				if val := ls.Metric.Get(inc); val != "" {
					metric = setLabel(metric, inc, val)
				}
			}
			// operate as lhs op rhs but iterate the many (rhs) side; report at rhs ts.
			if vs, ok := emit(metric, ls.V, rs.V, rs); ok {
				out = append(out, vs)
			}
		}
		return out
	default: // one-to-one
		rIndex := map[string]VectorSample{}
		for _, s := range rhs {
			rIndex[matchSignature(s.Metric, m, false)] = s
		}
		var out Vector
		for _, ls := range lhs {
			rs, ok := rIndex[matchSignature(ls.Metric, m, false)]
			if !ok {
				continue
			}
			if vs, ok := emit(resultMetricOneToOne(ls.Metric, m), ls.V, rs.V, ls); ok {
				out = append(out, vs)
			}
		}
		return out
	}
}

// vectorSetOp implements and / or / unless. Default matching is on all labels
// (including __name__).
func vectorSetOp(op tokenType, lhs, rhs Vector, m *VectorMatching) Vector {
	rSigs := map[string]bool{}
	for _, s := range rhs {
		rSigs[matchSignature(s.Metric, m, true)] = true
	}
	var out Vector
	switch op {
	case tLAnd:
		for _, s := range lhs {
			if rSigs[matchSignature(s.Metric, m, true)] {
				out = append(out, s)
			}
		}
	case tLUnless:
		for _, s := range lhs {
			if !rSigs[matchSignature(s.Metric, m, true)] {
				out = append(out, s)
			}
		}
	case tLOr:
		lSigs := map[string]bool{}
		for _, s := range lhs {
			lSigs[matchSignature(s.Metric, m, true)] = true
			out = append(out, s)
		}
		for _, s := range rhs {
			if !lSigs[matchSignature(s.Metric, m, true)] {
				out = append(out, s)
			}
		}
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
