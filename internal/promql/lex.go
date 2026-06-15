package promql

import (
	"fmt"
	"strings"
)

type tokenType int

const (
	tEOF tokenType = iota
	tIdentifier
	tNumber
	tString
	tDuration

	tLBrace
	tRBrace
	tLBracket
	tRBracket
	tLParen
	tRParen
	tComma
	tColon // ':' inside [range:res] subqueries
	tAt    // '@' modifier

	// set operators (assigned by the parser from and/or/unless identifiers)
	tLAnd
	tLOr
	tLUnless

	// matcher operators
	tEQL      // =
	tNEQ      // !=
	tEQLRegex // =~
	tNEQRegex // !~

	// arithmetic
	tAdd // +
	tSub // -
	tMul // *
	tDiv // /
	tMod // %
	tPow // ^

	// comparison
	tEQLCmp // ==
	tGTR    // >
	tLSS    // <
	tGTE    // >=
	tLTE    // <=
)

type token struct {
	typ tokenType
	val string
	pos int
}

// lex tokenizes a PromQL query string. bracketDepth tracks '[' nesting so a ':'
// inside a range/subquery selector is its own token while ':' elsewhere is a
// valid metric-name character (recording rules like instance:foo:rate5m).
func lex(input string) ([]token, error) {
	var toks []token
	i := 0
	n := len(input)
	bracketDepth := 0
	for i < n {
		c := input[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
			continue
		case c == '{':
			toks = append(toks, token{tLBrace, "{", i})
			i++
		case c == '}':
			toks = append(toks, token{tRBrace, "}", i})
			i++
		case c == '[':
			toks = append(toks, token{tLBracket, "[", i})
			bracketDepth++
			i++
		case c == ']':
			toks = append(toks, token{tRBracket, "]", i})
			if bracketDepth > 0 {
				bracketDepth--
			}
			i++
		case c == ':' && bracketDepth > 0:
			toks = append(toks, token{tColon, ":", i})
			i++
		case c == '@':
			toks = append(toks, token{tAt, "@", i})
			i++
		case c == '(':
			toks = append(toks, token{tLParen, "(", i})
			i++
		case c == ')':
			toks = append(toks, token{tRParen, ")", i})
			i++
		case c == ',':
			toks = append(toks, token{tComma, ",", i})
			i++
		case c == '+':
			toks = append(toks, token{tAdd, "+", i})
			i++
		case c == '-':
			toks = append(toks, token{tSub, "-", i})
			i++
		case c == '*':
			toks = append(toks, token{tMul, "*", i})
			i++
		case c == '/':
			toks = append(toks, token{tDiv, "/", i})
			i++
		case c == '%':
			toks = append(toks, token{tMod, "%", i})
			i++
		case c == '^':
			toks = append(toks, token{tPow, "^", i})
			i++
		case c == '=':
			if i+1 < n && input[i+1] == '~' {
				toks = append(toks, token{tEQLRegex, "=~", i})
				i += 2
			} else if i+1 < n && input[i+1] == '=' {
				toks = append(toks, token{tEQLCmp, "==", i})
				i += 2
			} else {
				toks = append(toks, token{tEQL, "=", i})
				i++
			}
		case c == '!':
			if i+1 < n && input[i+1] == '=' {
				toks = append(toks, token{tNEQ, "!=", i})
				i += 2
			} else if i+1 < n && input[i+1] == '~' {
				toks = append(toks, token{tNEQRegex, "!~", i})
				i += 2
			} else {
				return nil, fmt.Errorf("unexpected '!' at %d", i)
			}
		case c == '>':
			if i+1 < n && input[i+1] == '=' {
				toks = append(toks, token{tGTE, ">=", i})
				i += 2
			} else {
				toks = append(toks, token{tGTR, ">", i})
				i++
			}
		case c == '<':
			if i+1 < n && input[i+1] == '=' {
				toks = append(toks, token{tLTE, "<=", i})
				i += 2
			} else {
				toks = append(toks, token{tLSS, "<", i})
				i++
			}
		case c == '"' || c == '\'':
			s, ni, err := lexString(input, i)
			if err != nil {
				return nil, err
			}
			toks = append(toks, token{tString, s, i})
			i = ni
		case isDigit(c) || (c == '.' && i+1 < n && isDigit(input[i+1])):
			tok, ni := lexNumberOrDuration(input, i)
			toks = append(toks, tok)
			i = ni
		case isIdentStart(c):
			start := i
			i++
			for i < n && isIdentCont(input[i]) {
				i++
			}
			toks = append(toks, token{tIdentifier, input[start:i], start})
		default:
			return nil, fmt.Errorf("unexpected character %q at %d", string(c), i)
		}
	}
	toks = append(toks, token{tEOF, "", n})
	return toks, nil
}

func lexString(input string, i int) (string, int, error) {
	quote := input[i]
	i++
	var b strings.Builder
	for i < len(input) {
		c := input[i]
		if c == '\\' && i+1 < len(input) {
			switch input[i+1] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case '\\':
				b.WriteByte('\\')
			case '"':
				b.WriteByte('"')
			case '\'':
				b.WriteByte('\'')
			default:
				b.WriteByte(input[i+1])
			}
			i += 2
			continue
		}
		if c == quote {
			return b.String(), i + 1, nil
		}
		b.WriteByte(c)
		i++
	}
	return "", i, fmt.Errorf("unterminated string literal")
}

// lexNumberOrDuration scans a number, or a duration when integer digits are
// immediately followed by a unit letter (e.g. 5m, 1h30m).
func lexNumberOrDuration(input string, i int) (token, int) {
	start := i
	n := len(input)
	hasDot := false
	hasExp := false
	for i < n {
		c := input[i]
		if isDigit(c) {
			i++
		} else if c == '.' && !hasDot && !hasExp {
			hasDot = true
			i++
		} else if (c == 'e' || c == 'E') && !hasExp {
			hasExp = true
			i++
			if i < n && (input[i] == '+' || input[i] == '-') {
				i++
			}
		} else {
			break
		}
	}
	// Duration: pure integer run followed by a unit letter.
	if !hasDot && !hasExp && i < n && isDurationUnit(input[i]) {
		for i < n && (isDigit(input[i]) || isDurationUnit(input[i])) {
			i++
		}
		return token{tDuration, input[start:i], start}, i
	}
	return token{tNumber, input[start:i], start}, i
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
func isDurationUnit(c byte) bool {
	return c == 's' || c == 'm' || c == 'h' || c == 'd' || c == 'w' || c == 'y'
}
func isIdentStart(c byte) bool {
	return c == '_' || c == ':' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
func isIdentCont(c byte) bool {
	return isIdentStart(c) || isDigit(c)
}
