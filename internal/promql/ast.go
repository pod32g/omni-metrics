package promql

import "github.com/pod32g/omni-metrics/internal/model"

// Expr is a node in the parsed query tree.
type Expr interface{ exprNode() }

// NumberLiteral is a scalar constant.
type NumberLiteral struct{ Val float64 }

// StringLiteral is a string constant (used only inside selectors in this subset).
type StringLiteral struct{ Val string }

// VectorSelector selects an instant vector by metric name and label matchers.
type VectorSelector struct {
	Name     string
	Matchers []model.Matcher
}

// MatrixSelector wraps a VectorSelector with a range duration: vs[range].
type MatrixSelector struct {
	VS    *VectorSelector
	Range int64 // milliseconds
}

// Call is a function application, e.g. rate(x[5m]).
type Call struct {
	Func string
	Args []Expr
}

// AggregateExpr is sum/avg/min/max/count over a vector with optional grouping.
type AggregateExpr struct {
	Op       string
	Expr     Expr
	Grouping []string
	Without  bool
}

// BinaryExpr combines two operands with an arithmetic or comparison operator.
type BinaryExpr struct {
	Op       tokenType
	LHS, RHS Expr
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
func (*Call) exprNode()           {}
func (*AggregateExpr) exprNode()  {}
func (*BinaryExpr) exprNode()     {}
func (*UnaryExpr) exprNode()      {}
func (*ParenExpr) exprNode()      {}

// aggregators and functions recognized by the parser.
var aggregators = map[string]bool{
	"sum": true, "avg": true, "min": true, "max": true, "count": true,
}

var functions = map[string]bool{
	"rate": true, "irate": true, "increase": true,
	"sum_over_time": true, "avg_over_time": true, "min_over_time": true,
	"max_over_time": true, "count_over_time": true,
}
