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
	ev := &evaluator{q: e.q.Querier(), lookback: e.lookback, ctx: ctx}
	r, err := ev.evalExpr(expr, ts)
	if err != nil {
		return Result{}, err
	}
	switch r.kind {
	case ValueScalar:
		return Result{Type: ValueScalar, Scalar: Scalar{T: ts, V: r.scalar}}, nil
	case ValueVector:
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
	ev := &evaluator{q: e.q.Querier(), lookback: e.lookback, ctx: ctx}

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

type evaluator struct {
	q        tsdb.Querier
	lookback int64
	ctx      context.Context
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
		}
	default:
		return evalResult{}, fmt.Errorf("unary '-' requires scalar or vector")
	}
	return r, nil
}

// selectInstant returns the latest sample within the lookback window for each
// series matching the selector.
func (ev *evaluator) selectInstant(vs *VectorSelector, ts int64) Vector {
	ss := ev.q.Select(ts-ev.lookback, ts, vs.Matchers...)
	var out Vector
	for ss.Next() {
		s := ss.At()
		samples := s.Samples()
		if len(samples) == 0 {
			continue
		}
		last := samples[len(samples)-1]
		out = append(out, VectorSample{Metric: s.Labels(), T: ts, V: last.V})
	}
	return out
}

// selectRange returns all samples in [ts-Range, ts] for each matching series.
func (ev *evaluator) selectRange(ms *MatrixSelector, ts int64) Matrix {
	ss := ev.q.Select(ts-ms.Range, ts, ms.VS.Matchers...)
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

func (ev *evaluator) evalCall(c *Call, ts int64) (evalResult, error) {
	arg, err := ev.evalExpr(c.Args[0], ts)
	if err != nil {
		return evalResult{}, err
	}
	if arg.kind != ValueMatrix {
		return evalResult{}, fmt.Errorf("%s expects a range vector argument", c.Func)
	}
	return evalResult{kind: ValueVector, vector: applyRangeFunc(c.Func, arg.matrix, ts)}, nil
}

func (ev *evaluator) evalAggregate(a *AggregateExpr, ts int64) (evalResult, error) {
	inner, err := ev.evalExpr(a.Expr, ts)
	if err != nil {
		return evalResult{}, err
	}
	if inner.kind != ValueVector {
		return evalResult{}, fmt.Errorf("aggregation %s requires an instant vector", a.Op)
	}
	out, err := aggregate(a.Op, inner.vector, a.Grouping, a.Without, ts)
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
	return applyBinary(b.Op, l, r)
}
