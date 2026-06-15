package promql

import (
	"fmt"
	"strconv"

	"github.com/pod32g/omni-metrics/internal/model"
)

// Parse turns a query string into an expression tree.
func Parse(input string) (Expr, error) {
	toks, err := lex(input)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	e, err := p.parseExpr(0)
	if err != nil {
		return nil, err
	}
	if p.peek().typ != tEOF {
		return nil, fmt.Errorf("unexpected trailing token %q", p.peek().val)
	}
	return e, nil
}

type parser struct {
	toks []token
	pos  int
}

func (p *parser) peek() token { return p.toks[p.pos] }
func (p *parser) next() token { t := p.toks[p.pos]; p.pos++; return t }

func (p *parser) expect(tt tokenType, what string) (token, error) {
	t := p.peek()
	if t.typ != tt {
		return t, fmt.Errorf("expected %s, got %q", what, t.val)
	}
	return p.next(), nil
}

// binaryPrec returns the precedence of a binary operator and whether it is one.
func binaryPrec(tt tokenType) (int, bool) {
	switch tt {
	case tEQLCmp, tNEQ, tGTR, tLSS, tGTE, tLTE:
		return 1, true
	case tAdd, tSub:
		return 2, true
	case tMul, tDiv, tMod:
		return 3, true
	case tPow:
		return 4, true
	default:
		return 0, false
	}
}

// parseExpr is a precedence-climbing parser. ^ (pow) is right-associative.
func (p *parser) parseExpr(minPrec int) (Expr, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for {
		op := p.peek().typ
		prec, ok := binaryPrec(op)
		if !ok || prec < minPrec {
			break
		}
		p.next()
		nextMin := prec + 1
		if op == tPow {
			nextMin = prec // right associative
		}
		right, err := p.parseExpr(nextMin)
		if err != nil {
			return nil, err
		}
		left = &BinaryExpr{Op: op, LHS: left, RHS: right}
	}
	return left, nil
}

func (p *parser) parseUnary() (Expr, error) {
	switch p.peek().typ {
	case tSub:
		p.next()
		// Unary minus binds looser than '^' (pow) but tighter than the binary
		// arithmetic/comparison operators, so parse the operand at pow precedence:
		// -2^2 parses as -(2^2), while -2*3 parses as (-2)*3.
		powPrec, _ := binaryPrec(tPow)
		e, err := p.parseExpr(powPrec)
		if err != nil {
			return nil, err
		}
		return &UnaryExpr{Op: tSub, Expr: e}, nil
	case tAdd:
		p.next()
		return p.parseUnary()
	default:
		return p.parsePrimary()
	}
}

func (p *parser) parsePrimary() (Expr, error) {
	t := p.peek()
	switch t.typ {
	case tNumber:
		p.next()
		f, err := strconv.ParseFloat(t.val, 64)
		if err != nil {
			return nil, fmt.Errorf("bad number %q: %w", t.val, err)
		}
		return &NumberLiteral{Val: f}, nil
	case tString:
		p.next()
		return &StringLiteral{Val: t.val}, nil
	case tLParen:
		p.next()
		e, err := p.parseExpr(0)
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(tRParen, "')'"); err != nil {
			return nil, err
		}
		return &ParenExpr{Expr: e}, nil
	case tLBrace:
		return p.parseSelectorFrom("")
	case tIdentifier:
		name := t.val
		switch {
		case aggregators[name]:
			return p.parseAggregate()
		case functions[name]:
			return p.parseCall()
		default:
			p.next()
			return p.parseSelectorFrom(name)
		}
	default:
		return nil, fmt.Errorf("unexpected token %q", t.val)
	}
}

// parseSelectorFrom parses an optional {matchers} and optional [range] following
// an already-consumed metric name (name may be "").
func (p *parser) parseSelectorFrom(name string) (Expr, error) {
	vs := &VectorSelector{Name: name}
	if name != "" {
		m, _ := model.NewMatcher(model.MatchEqual, model.MetricName, name)
		vs.Matchers = append(vs.Matchers, *m)
	}
	if p.peek().typ == tLBrace {
		ms, err := p.parseMatchers()
		if err != nil {
			return nil, err
		}
		vs.Matchers = append(vs.Matchers, ms...)
	}
	if len(vs.Matchers) == 0 {
		return nil, fmt.Errorf("vector selector must specify a metric name or at least one matcher")
	}
	if p.peek().typ == tLBracket {
		p.next()
		dt, err := p.expect(tDuration, "range duration")
		if err != nil {
			return nil, err
		}
		ms, err := parseDuration(dt.val)
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(tRBracket, "']'"); err != nil {
			return nil, err
		}
		return &MatrixSelector{VS: vs, Range: ms}, nil
	}
	return vs, nil
}

func (p *parser) parseMatchers() ([]model.Matcher, error) {
	if _, err := p.expect(tLBrace, "'{'"); err != nil {
		return nil, err
	}
	var out []model.Matcher
	for p.peek().typ != tRBrace {
		nameT, err := p.expect(tIdentifier, "label name")
		if err != nil {
			return nil, err
		}
		opT := p.next()
		var mt model.MatchType
		switch opT.typ {
		case tEQL:
			mt = model.MatchEqual
		case tNEQ:
			mt = model.MatchNotEqual
		case tEQLRegex:
			mt = model.MatchRegexp
		case tNEQRegex:
			mt = model.MatchNotRegexp
		default:
			return nil, fmt.Errorf("expected matcher operator, got %q", opT.val)
		}
		valT, err := p.expect(tString, "matcher value")
		if err != nil {
			return nil, err
		}
		m, err := model.NewMatcher(mt, nameT.val, valT.val)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
		if p.peek().typ == tComma {
			p.next()
		} else if p.peek().typ != tRBrace {
			return nil, fmt.Errorf("expected ',' or '}' in matchers, got %q", p.peek().val)
		}
	}
	p.next() // consume }
	return out, nil
}

func (p *parser) parseCall() (Expr, error) {
	name := p.next().val
	if _, err := p.expect(tLParen, "'('"); err != nil {
		return nil, err
	}
	var args []Expr
	if p.peek().typ != tRParen {
		for {
			a, err := p.parseExpr(0)
			if err != nil {
				return nil, err
			}
			args = append(args, a)
			if p.peek().typ == tComma {
				p.next()
				continue
			}
			break
		}
	}
	if _, err := p.expect(tRParen, "')'"); err != nil {
		return nil, err
	}
	if len(args) != 1 {
		return nil, fmt.Errorf("%s expects exactly 1 argument, got %d", name, len(args))
	}
	return &Call{Func: name, Args: args}, nil
}

func (p *parser) parseAggregate() (Expr, error) {
	op := p.next().val
	agg := &AggregateExpr{Op: op}

	// Grouping clause may precede or follow the argument list.
	if p.isGroupingKeyword() {
		without, grouping, err := p.parseGrouping()
		if err != nil {
			return nil, err
		}
		agg.Without, agg.Grouping = without, grouping
	}

	if _, err := p.expect(tLParen, "'('"); err != nil {
		return nil, err
	}
	inner, err := p.parseExpr(0)
	if err != nil {
		return nil, err
	}
	agg.Expr = inner
	if _, err := p.expect(tRParen, "')'"); err != nil {
		return nil, err
	}

	if agg.Grouping == nil && p.isGroupingKeyword() {
		without, grouping, err := p.parseGrouping()
		if err != nil {
			return nil, err
		}
		agg.Without, agg.Grouping = without, grouping
	}
	return agg, nil
}

func (p *parser) isGroupingKeyword() bool {
	t := p.peek()
	return t.typ == tIdentifier && (t.val == "by" || t.val == "without")
}

func (p *parser) parseGrouping() (without bool, labels []string, err error) {
	kw := p.next().val
	without = kw == "without"
	if _, err := p.expect(tLParen, "'(' after "+kw); err != nil {
		return false, nil, err
	}
	labels = []string{}
	for p.peek().typ != tRParen {
		lt, err := p.expect(tIdentifier, "grouping label")
		if err != nil {
			return false, nil, err
		}
		labels = append(labels, lt.val)
		if p.peek().typ == tComma {
			p.next()
		} else if p.peek().typ != tRParen {
			return false, nil, fmt.Errorf("expected ',' or ')' in grouping, got %q", p.peek().val)
		}
	}
	p.next() // consume )
	return without, labels, nil
}

// ParseMatchers parses a bare selector string (e.g. `up{job="omni"}`) and
// returns its label matchers. Used by the /series and /labels match[] params.
func ParseMatchers(s string) ([]model.Matcher, error) {
	e, err := Parse(s)
	if err != nil {
		return nil, err
	}
	switch v := e.(type) {
	case *VectorSelector:
		return v.Matchers, nil
	case *MatrixSelector:
		return v.VS.Matchers, nil
	default:
		return nil, fmt.Errorf("%q is not a series selector", s)
	}
}

// parseDuration parses compound durations like "5m", "1h30m", "2d" into ms.
func parseDuration(s string) (int64, error) {
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	var total int64
	i := 0
	for i < len(s) {
		start := i
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if start == i || i >= len(s) {
			return 0, fmt.Errorf("malformed duration %q", s)
		}
		num, _ := strconv.ParseInt(s[start:i], 10, 64)
		var unitMs int64
		switch s[i] {
		case 's':
			unitMs = 1000
		case 'm':
			unitMs = 60 * 1000
		case 'h':
			unitMs = 60 * 60 * 1000
		case 'd':
			unitMs = 24 * 60 * 60 * 1000
		case 'w':
			unitMs = 7 * 24 * 60 * 60 * 1000
		case 'y':
			unitMs = 365 * 24 * 60 * 60 * 1000
		default:
			return 0, fmt.Errorf("unknown duration unit %q", string(s[i]))
		}
		total += num * unitMs
		i++
	}
	return total, nil
}
