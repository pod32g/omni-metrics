package promql

import "github.com/pod32g/omni-metrics/internal/model"

// Expr is a node in the parsed query tree.
type Expr interface{ exprNode() }

// NumberLiteral is a scalar constant.
type NumberLiteral struct{ Val float64 }

// StringLiteral is a string constant.
type StringLiteral struct{ Val string }

// atKind classifies an @ modifier on a selector/subquery.
type atKind uint8

const (
	atNone atKind = iota
	atTime
	atStart
	atEnd
)

// AtModifier pins evaluation of a selector to a fixed timestamp (@ <ts>), or to
// the query's start()/end().
type AtModifier struct {
	Kind atKind
	TS   int64 // milliseconds, when Kind == atTime
}

// VectorSelector selects an instant vector by metric name and label matchers,
// with optional offset and @ modifiers.
type VectorSelector struct {
	Name     string
	Matchers []model.Matcher
	Offset   int64 // milliseconds
	At       *AtModifier
}

// MatrixSelector wraps a VectorSelector with a range duration: vs[range].
type MatrixSelector struct {
	VS     *VectorSelector
	Range  int64 // milliseconds
	Offset int64
	At     *AtModifier
}

// SubqueryExpr evaluates an inner expression over a range at a resolution,
// producing a range vector: expr[range:resolution].
type SubqueryExpr struct {
	Expr   Expr
	Range  int64 // milliseconds
	Step   int64 // milliseconds; 0 = engine default
	Offset int64
	At     *AtModifier
}

// Call is a function application, e.g. rate(x[5m]) or label_replace(...).
type Call struct {
	Func string
	Args []Expr
}

// AggregateExpr is an aggregation over a vector with optional grouping and, for
// topk/bottomk/quantile/count_values, a parameter.
type AggregateExpr struct {
	Op       string
	Expr     Expr
	Param    Expr // nil unless the aggregator takes a parameter
	Grouping []string
	Without  bool
}

// matchCardinality describes how a vector-vector binary op matches its sides.
type matchCardinality uint8

const (
	cardOneToOne  matchCardinality = iota
	cardManyToOne                  // group_left
	cardOneToMany                  // group_right
)

// VectorMatching describes on/ignoring + group_left/right for a binary op.
type VectorMatching struct {
	On             bool     // true: on(...); false: ignoring(...)
	MatchingLabels []string // labels to match on / ignore
	Card           matchCardinality
	Include        []string // group_left/right(...) labels copied from the "one" side
}

// BinaryExpr combines two operands with an arithmetic/comparison/set operator.
type BinaryExpr struct {
	Op         tokenType
	LHS, RHS   Expr
	Matching   *VectorMatching
	ReturnBool bool // the 'bool' modifier on a comparison
}

// UnaryExpr is a unary minus.
type UnaryExpr struct {
	Op   tokenType
	Expr Expr
}

// ParenExpr is a parenthesized sub-expression.
type ParenExpr struct{ Expr Expr }

func (*NumberLiteral) exprNode()  {}
func (*StringLiteral) exprNode()  {}
func (*VectorSelector) exprNode() {}
func (*MatrixSelector) exprNode() {}
func (*SubqueryExpr) exprNode()   {}
func (*Call) exprNode()           {}
func (*AggregateExpr) exprNode()  {}
func (*BinaryExpr) exprNode()     {}
func (*UnaryExpr) exprNode()      {}
func (*ParenExpr) exprNode()      {}

// aggregators recognized by the parser.
var aggregators = map[string]bool{
	"sum": true, "avg": true, "min": true, "max": true, "count": true,
	"topk": true, "bottomk": true, "quantile": true, "stddev": true,
	"stdvar": true, "group": true, "count_values": true,
}

// paramAggregators take a leading parameter argument: op(param, vector).
var paramAggregators = map[string]bool{
	"topk": true, "bottomk": true, "quantile": true, "count_values": true,
}

// functions recognized by the parser (arity is validated during evaluation).
var functions = map[string]bool{
	// range-vector input
	"rate": true, "irate": true, "increase": true, "delta": true, "idelta": true,
	"deriv": true, "predict_linear": true, "changes": true, "resets": true,
	"sum_over_time": true, "avg_over_time": true, "min_over_time": true,
	"max_over_time": true, "count_over_time": true, "stddev_over_time": true,
	"stdvar_over_time": true, "last_over_time": true, "present_over_time": true,
	"quantile_over_time": true,
	// instant-vector / scalar input
	"abs": true, "ceil": true, "floor": true, "round": true, "exp": true,
	"ln": true, "log2": true, "log10": true, "sqrt": true, "sgn": true,
	"clamp": true, "clamp_max": true, "clamp_min": true,
	"scalar": true, "vector": true, "sort": true, "sort_desc": true,
	"absent": true, "absent_over_time": true,
	"label_replace": true, "label_join": true,
	"histogram_quantile": true,
	"time":               true, "timestamp": true,
	"day_of_month": true, "day_of_week": true, "day_of_year": true,
	"days_in_month": true, "hour": true, "minute": true, "month": true, "year": true,
}
