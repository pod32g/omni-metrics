package model

import (
	"fmt"
	"regexp"
)

// MatchType is the kind of comparison a Matcher performs.
type MatchType uint8

const (
	MatchEqual     MatchType = iota // =
	MatchNotEqual                   // !=
	MatchRegexp                     // =~
	MatchNotRegexp                  // !~
)

// String returns the PromQL operator for the match type.
func (t MatchType) String() string {
	switch t {
	case MatchEqual:
		return "="
	case MatchNotEqual:
		return "!="
	case MatchRegexp:
		return "=~"
	case MatchNotRegexp:
		return "!~"
	default:
		return "?"
	}
}

// Matcher selects series by comparing a label's value. Regexp matchers are
// fully anchored, matching the whole value (Prometheus semantics).
type Matcher struct {
	Type  MatchType
	Name  string
	Value string
	re    *regexp.Regexp
}

// NewMatcher compiles a matcher. For regexp types the pattern is anchored with
// ^(?:...)$ so it must match the entire label value. An invalid regexp returns
// an error.
func NewMatcher(t MatchType, name, value string) (*Matcher, error) {
	m := &Matcher{Type: t, Name: name, Value: value}
	if t == MatchRegexp || t == MatchNotRegexp {
		re, err := regexp.Compile("^(?:" + value + ")$")
		if err != nil {
			return nil, fmt.Errorf("compiling matcher %s%s%q: %w", name, t, value, err)
		}
		m.re = re
	}
	return m, nil
}

// Matches reports whether the candidate label value satisfies the matcher.
func (m *Matcher) Matches(s string) bool {
	switch m.Type {
	case MatchEqual:
		return s == m.Value
	case MatchNotEqual:
		return s != m.Value
	case MatchRegexp:
		return m.re.MatchString(s)
	case MatchNotRegexp:
		return !m.re.MatchString(s)
	default:
		return false
	}
}

// String renders the matcher as label<op>"value".
func (m *Matcher) String() string {
	return fmt.Sprintf("%s%s%q", m.Name, m.Type, m.Value)
}
