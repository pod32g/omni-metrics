package promql

import (
	"context"
	"fmt"
	"sort"

	"github.com/pod32g/omni-metrics/internal/model"
	"github.com/pod32g/omni-metrics/internal/tsdb"
)

// Queryable is the storage dependency of the engine.
type Queryable interface {
	Querier() tsdb.Querier
}

// Engine evaluates PromQL queries against a Queryable.
type Engine struct {
	q        Queryable
	lookback int64 // staleness window for instant selectors, in ms
}

// maxResolution caps the number of points a range query may produce, bounding
// CPU/memory per request (Prometheus uses the same 11,000 limit).
const maxResolution = 11000

// NewEngine builds an engine with the default 5-minute lookback.
func NewEngine(q Queryable) *Engine {
	return &Engine{q: q, lookback: 5 * 60 * 1000}
}

// InstantQuery evaluates the query at a single timestamp.
func (e *Engine) InstantQuery(ctx context.Context, qs string, ts int64) (Result, error) {
	expr, err := Parse(qs)
	if err != nil {
		return Result{}, err
	}
	ev := &evaluator{q: e.q.Querier(), lookback: e.lookback, ctx: ctx, start: ts, end: ts}
	r, err := ev.evalExpr(expr, ts)
	if err != nil {
		return Result{}, err
	}
	switch r.kind {
	case ValueScalar:
		return Result{Type: ValueScalar, Scalar: Scalar{T: ts, V: r.scalar}}, nil
	case ValueVector:
		// Normalize result timestamps to the evaluation time (a bare selector
		// carries its sample's time so timestamp() works; the reported instant is
		// the eval time, as in Prometheus).
		for i := range r.vector {
			r.vector[i].T = ts
		}
		return Result{Type: ValueVector, Vector: r.vector}, nil
	case ValueMatrix:
		return Result{Type: ValueMatrix, Matrix: r.matrix}, nil
	default:
		return Result{Type: ValueString, String: r.str}, nil
	}
}

// RangeQuery evaluates the query at each step in [start, end] and assembles a
// matrix. The query must evaluate to a scalar or instant vector at each step.
func (e *Engine) RangeQuery(ctx context.Context, qs string, start, end, step int64) (Result, error) {
	if step <= 0 {
		return Result{}, fmt.Errorf("step must be positive")
	}
	if end < start {
		return Result{}, fmt.Errorf("end timestamp must not be before start time")
	}
	span := end - start
	if span < 0 { // int64 overflow from extreme bounds
		return Result{}, fmt.Errorf("time range too large")
	}
	steps := span / step
	if steps+1 > maxResolution {
		return Result{}, fmt.Errorf("exceeded maximum resolution of %d points; reduce the range or increase the step", maxResolution)
	}
	expr, err := Parse(qs)
	if err != nil {
		return Result{}, err
	}
	ev := &evaluator{q: e.q.Querier(), lookback: e.lookback, ctx: ctx, start: start, end: end}

	order := []string{}
	byKey := map[string]*SeriesData{}
	add := func(metric model.Labels, t int64, v float64) {
		key := metric.String()
		sd := byKey[key]
		if sd == nil {
			sd = &SeriesData{Metric: metric}
			byKey[key] = sd
			order = append(order, key)
		}
		sd.Points = append(sd.Points, Point{T: t, V: v})
	}

	// Iterate by an integer counter (start + i*step) so the loop cannot run away
	// on int64 overflow of ts += step near the bounds.
	for i := int64(0); i <= steps; i++ {
		ts := start + i*step
		r, err := ev.evalExpr(expr, ts)
		if err != nil {
			return Result{}, err
		}
		switch r.kind {
		case ValueScalar:
			add(model.Labels{}, ts, r.scalar)
		case ValueVector:
			for _, s := range r.vector {
				add(s.Metric, ts, s.V)
			}
		default:
			return Result{}, fmt.Errorf("range query expression must return scalar or instant vector")
		}
	}

	sort.Strings(order)
	m := make(Matrix, 0, len(order))
	for _, k := range order {
		m = append(m, *byKey[k])
	}
	return Result{Type: ValueMatrix, Matrix: m}, nil
}

// evalResult is the internal value produced by evaluating an expression.
type evalResult struct {
	kind   ValueType
	scalar float64
	vector Vector
	matrix Matrix
	str    string
}

// defaultSubqueryStep is the resolution used for a subquery that omits one.
const defaultSubqueryStep = 60 * 1000 // 1m

type evaluator struct {
	q          tsdb.Querier
	lookback   int64
	ctx        context.Context
	start, end int64 // query bounds, for @ start()/end()
}

func (ev *evaluator) evalExpr(e Expr, ts int64) (evalResult, error) {
	if ev.ctx != nil {
		if err := ev.ctx.Err(); err != nil {
			return evalResult{}, err
		}
	}
	switch n := e.(type) {
	case *NumberLiteral:
		return evalResult{kind: ValueScalar, scalar: n.Val}, nil
	case *StringLiteral:
		return evalResult{kind: ValueString, str: n.Val}, nil
	case *ParenExpr:
		return ev.evalExpr(n.Expr, ts)
	case *UnaryExpr:
		return ev.evalUnary(n, ts)
	case *VectorSelector:
		return evalResult{kind: ValueVector, vector: ev.selectInstant(n, ts)}, nil
	case *MatrixSelector:
		return evalResult{kind: ValueMatrix, matrix: ev.selectRange(n, ts)}, nil
	case *SubqueryExpr:
		m, err := ev.evalSubquery(n, ts)
		if err != nil {
			return evalResult{}, err
		}
		return evalResult{kind: ValueMatrix, matrix: m}, nil
	case *Call:
		return ev.evalCall(n, ts)
	case *AggregateExpr:
		return ev.evalAggregate(n, ts)
	case *BinaryExpr:
		return ev.evalBinary(n, ts)
	default:
		return evalResult{}, fmt.Errorf("cannot evaluate %T", e)
	}
}

func (ev *evaluator) resolveTime(at *AtModifier, ts int64) int64 {
	if at == nil {
		return ts
	}
	switch at.Kind {
	case atTime:
		return at.TS
	case atStart:
		return ev.start
	case atEnd:
		return ev.end
	default:
		return ts
	}
}

func (ev *evaluator) evalUnary(n *UnaryExpr, ts int64) (evalResult, error) {
	r, err := ev.evalExpr(n.Expr, ts)
	if err != nil {
		return evalResult{}, err
	}
	switch r.kind {
	case ValueScalar:
		r.scalar = -r.scalar
	case ValueVector:
		for i := range r.vector {
			r.vector[i].V = -r.vector[i].V
			r.vector[i].Metric = dropMetricName(r.vector[i].Metric)
		}
	default:
		return evalResult{}, fmt.Errorf("unary '-' requires scalar or vector")
	}
	return r, nil
}

// selectInstant returns the latest sample within the lookback window for each
// matching series, honoring offset and @ modifiers. The result is reported at the
// evaluation timestamp.
func (ev *evaluator) selectInstant(vs *VectorSelector, ts int64) Vector {
	selTs := ev.resolveTime(vs.At, ts) - vs.Offset
	ss := ev.q.Select(selTs-ev.lookback, selTs, vs.Matchers...)
	var out Vector
	for ss.Next() {
		s := ss.At()
		samples := s.Samples()
		if len(samples) == 0 {
			continue
		}
		last := samples[len(samples)-1]
		// Carry the sample's own timestamp so timestamp() reports it; the final
		// instant result is normalized to the eval time in InstantQuery.
		out = append(out, VectorSample{Metric: s.Labels(), T: last.T, V: last.V})
	}
	return out
}

// selectRange returns the samples in the range window for each matching series,
// honoring offset and @ modifiers.
func (ev *evaluator) selectRange(ms *MatrixSelector, ts int64) Matrix {
	selTs := ev.resolveTime(ms.At, ts) - ms.Offset
	ss := ev.q.Select(selTs-ms.Range, selTs, ms.VS.Matchers...)
	var out Matrix
	for ss.Next() {
		s := ss.At()
		samples := s.Samples()
		pts := make([]Point, len(samples))
		for i, sm := range samples {
			pts[i] = Point{T: sm.T, V: sm.V}
		}
		out = append(out, SeriesData{Metric: s.Labels(), Points: pts})
	}
	return out
}

// evalSubquery evaluates the inner expression at each resolution step across the
// subquery range, assembling a range vector.
func (ev *evaluator) evalSubquery(sq *SubqueryExpr, ts int64) (Matrix, error) {
	step := sq.Step
	if step <= 0 {
		step = defaultSubqueryStep
	}
	selTs := ev.resolveTime(sq.At, ts) - sq.Offset
	start := selTs - sq.Range
	n := sq.Range / step
	if n+1 > maxResolution {
		return nil, fmt.Errorf("subquery exceeds maximum resolution of %d points", maxResolution)
	}
	order := []string{}
	byKey := map[string]*SeriesData{}
	add := func(metric model.Labels, t int64, v float64) {
		key := metric.String()
		sd := byKey[key]
		if sd == nil {
			sd = &SeriesData{Metric: metric}
			byKey[key] = sd
			order = append(order, key)
		}
		sd.Points = append(sd.Points, Point{T: t, V: v})
	}
	for i := int64(0); i <= n; i++ {
		t := start + i*step
		r, err := ev.evalExpr(sq.Expr, t)
		if err != nil {
			return nil, err
		}
		switch r.kind {
		case ValueVector:
			for _, s := range r.vector {
				add(s.Metric, t, s.V)
			}
		case ValueScalar:
			add(model.Labels{}, t, r.scalar)
		default:
			return nil, fmt.Errorf("subquery inner expression must return a scalar or instant vector")
		}
	}
	m := make(Matrix, 0, len(order))
	for _, k := range order {
		m = append(m, *byKey[k])
	}
	return m, nil
}

func (ev *evaluator) evalAggregate(a *AggregateExpr, ts int64) (evalResult, error) {
	inner, err := ev.evalExpr(a.Expr, ts)
	if err != nil {
		return evalResult{}, err
	}
	if inner.kind != ValueVector {
		return evalResult{}, fmt.Errorf("aggregation %s requires an instant vector", a.Op)
	}
	var param float64
	var paramStr string
	if a.Param != nil {
		pr, err := ev.evalExpr(a.Param, ts)
		if err != nil {
			return evalResult{}, err
		}
		switch pr.kind {
		case ValueScalar:
			param = pr.scalar
		case ValueString:
			paramStr = pr.str
		default:
			return evalResult{}, fmt.Errorf("%s parameter must be a scalar or string", a.Op)
		}
	}
	out, err := aggregate(a.Op, inner.vector, a.Grouping, a.Without, ts, param, paramStr)
	if err != nil {
		return evalResult{}, err
	}
	return evalResult{kind: ValueVector, vector: out}, nil
}

func (ev *evaluator) evalBinary(b *BinaryExpr, ts int64) (evalResult, error) {
	l, err := ev.evalExpr(b.LHS, ts)
	if err != nil {
		return evalResult{}, err
	}
	r, err := ev.evalExpr(b.RHS, ts)
	if err != nil {
		return evalResult{}, err
	}
	return applyBinary(b, l, r)
}
