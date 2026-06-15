// Package promql implements a TDD-able subset of PromQL: instant and range
// queries over selectors, aggregations (sum/avg/min/max/count by/without),
// range functions (rate/irate/increase, *_over_time), and scalar/vector
// arithmetic and comparison. Deferred to a later milestone: histogram_quantile,
// topk/bottomk, offset/@ modifiers, subqueries, and on/ignoring/group_left.
package promql

import "github.com/pod32g/omni-metrics/internal/model"

// ValueType tags the kind of a query result.
type ValueType uint8

const (
	ValueScalar ValueType = iota
	ValueVector
	ValueMatrix
	ValueString
)

func (v ValueType) String() string {
	switch v {
	case ValueScalar:
		return "scalar"
	case ValueVector:
		return "vector"
	case ValueMatrix:
		return "matrix"
	case ValueString:
		return "string"
	default:
		return "unknown"
	}
}

// Point is a (timestamp, value) pair in a matrix series.
type Point struct {
	T int64
	V float64
}

// VectorSample is one element of an instant vector: a label set with a single
// value at the evaluation timestamp.
type VectorSample struct {
	Metric model.Labels
	T      int64
	V      float64
}

// Vector is an instant vector — a set of samples at one instant.
type Vector []VectorSample

// SeriesData is one series of a matrix: labels plus points over time.
type SeriesData struct {
	Metric model.Labels
	Points []Point
}

// Matrix is a range vector — a set of series each with multiple points.
type Matrix []SeriesData

// Scalar is a single numeric value at a timestamp.
type Scalar struct {
	T int64
	V float64
}

// Result is the typed output of a query. Exactly one of the value fields is
// meaningful, per Type.
type Result struct {
	Type   ValueType
	Scalar Scalar
	Vector Vector
	Matrix Matrix
	String string
}
