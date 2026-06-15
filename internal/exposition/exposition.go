// Package exposition parses the Prometheus text exposition format produced by
// instrumented targets at their /metrics endpoint. It is a pure function over an
// io.Reader and has no side effects, which keeps it trivial to fixture-test.
package exposition

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"

	"github.com/pod32g/omni-metrics/internal/model"
)

// Series is one parsed sample line: its full label set (including __name__), the
// value, and an optional explicit timestamp in milliseconds.
type Series struct {
	Labels    model.Labels
	Value     float64
	Timestamp *int64
}

// Metadata is the "# TYPE"/"# HELP" information for a metric family.
type Metadata struct {
	Type model.MetricType
	Help string
}

// Result holds everything parsed from one exposition body.
type Result struct {
	Series   []Series
	Metadata map[string]Metadata
}

// Parse reads the exposition text and returns the parsed series and metadata.
//
// Parsing is lenient: a malformed sample line is skipped rather than aborting the
// whole body, and the returned error (non-nil if any line failed) reports the
// first failure with its line number and a total count. The Result is always
// non-nil and contains every line that parsed successfully, so a caller can use
// the good series and log the error.
func Parse(r io.Reader) (*Result, error) {
	res := &Result{Metadata: map[string]Metadata{}}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	var firstErr error
	badLines := 0
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if line[0] == '#' {
			parseMeta(line, res.Metadata)
			continue
		}
		s, err := parseSample(line)
		if err != nil {
			badLines++
			if firstErr == nil {
				firstErr = fmt.Errorf("line %d: %w", lineNo, err)
			}
			continue
		}
		res.Series = append(res.Series, s)
	}
	if err := sc.Err(); err != nil {
		return res, fmt.Errorf("reading exposition: %w", err)
	}
	if firstErr != nil {
		return res, fmt.Errorf("%w (%d malformed line(s) skipped)", firstErr, badLines)
	}
	return res, nil
}

// parseMeta handles "# HELP name text" and "# TYPE name kind"; any other comment
// is ignored.
func parseMeta(line string, md map[string]Metadata) {
	fields := strings.Fields(line)
	if len(fields) < 3 {
		return
	}
	kind, name := fields[1], fields[2]
	switch kind {
	case "HELP":
		m := md[name]
		// HELP text is everything after the name; unescape \\ and \n per spec.
		idx := strings.Index(line, name)
		help := strings.TrimSpace(line[idx+len(name):])
		m.Help = unescapeHelp(help)
		md[name] = m
	case "TYPE":
		if len(fields) >= 4 {
			m := md[name]
			m.Type = model.ParseMetricType(fields[3])
			md[name] = m
		}
	}
}

func unescapeHelp(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case 'n':
				b.WriteByte('\n')
				i++
				continue
			case '\\':
				b.WriteByte('\\')
				i++
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// parseSample parses a single "name{labels} value [timestamp]" line.
func parseSample(line string) (Series, error) {
	p := &parser{s: line}
	name, err := p.metricName()
	if err != nil {
		return Series{}, err
	}
	pairs := []string{model.MetricName, name}

	p.skipSpaces()
	if p.peek() == '{' {
		labelPairs, err := p.labels()
		if err != nil {
			return Series{}, err
		}
		// Drop any explicit __name__ label so a target cannot override the
		// positional metric name and forge a reserved series (e.g. up).
		for i := 0; i+1 < len(labelPairs); i += 2 {
			if labelPairs[i] == model.MetricName {
				continue
			}
			pairs = append(pairs, labelPairs[i], labelPairs[i+1])
		}
		p.skipSpaces()
	}

	valTok := p.token()
	if valTok == "" {
		return Series{}, fmt.Errorf("missing value")
	}
	val, err := parseFloat(valTok)
	if err != nil {
		return Series{}, fmt.Errorf("invalid value %q: %w", valTok, err)
	}

	var ts *int64
	if tsTok := p.token(); tsTok != "" {
		n, err := strconv.ParseInt(tsTok, 10, 64)
		if err != nil {
			return Series{}, fmt.Errorf("invalid timestamp %q: %w", tsTok, err)
		}
		ts = &n
	}
	if rest := strings.TrimSpace(p.rest()); rest != "" {
		return Series{}, fmt.Errorf("trailing data %q", rest)
	}

	return Series{Labels: model.FromStrings(pairs...), Value: val, Timestamp: ts}, nil
}

func parseFloat(tok string) (float64, error) {
	switch tok {
	case "+Inf", "Inf", "+inf", "inf":
		return math.Inf(1), nil
	case "-Inf", "-inf":
		return math.Inf(-1), nil
	case "NaN", "nan":
		return math.NaN(), nil
	}
	return strconv.ParseFloat(tok, 64)
}

// parser is a small cursor over a single line.
type parser struct {
	s string
	i int
}

func (p *parser) peek() byte {
	if p.i >= len(p.s) {
		return 0
	}
	return p.s[p.i]
}

func (p *parser) skipSpaces() {
	for p.i < len(p.s) && (p.s[p.i] == ' ' || p.s[p.i] == '\t') {
		p.i++
	}
}

// token returns the next whitespace-delimited token, or "" at end of line.
func (p *parser) token() string {
	p.skipSpaces()
	start := p.i
	for p.i < len(p.s) && p.s[p.i] != ' ' && p.s[p.i] != '\t' {
		p.i++
	}
	return p.s[start:p.i]
}

func (p *parser) rest() string { return p.s[p.i:] }

func isNameStart(b byte) bool {
	return b == '_' || b == ':' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func isNameCont(b byte) bool {
	return isNameStart(b) || (b >= '0' && b <= '9')
}

func isLabelNameStart(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func isLabelNameCont(b byte) bool {
	return isLabelNameStart(b) || (b >= '0' && b <= '9')
}

func (p *parser) metricName() (string, error) {
	p.skipSpaces()
	if p.i >= len(p.s) || !isNameStart(p.s[p.i]) {
		return "", fmt.Errorf("expected metric name at %q", p.rest())
	}
	start := p.i
	p.i++
	for p.i < len(p.s) && isNameCont(p.s[p.i]) {
		p.i++
	}
	return p.s[start:p.i], nil
}

// labels parses {name="value", ...} and returns flat name,value pairs.
func (p *parser) labels() ([]string, error) {
	if p.peek() != '{' {
		return nil, fmt.Errorf("expected '{'")
	}
	p.i++ // consume {
	var out []string
	for {
		p.skipSpaces()
		if p.peek() == '}' {
			p.i++
			return out, nil
		}
		if p.i >= len(p.s) {
			return nil, fmt.Errorf("unterminated label set")
		}
		// label name
		if !isLabelNameStart(p.s[p.i]) {
			return nil, fmt.Errorf("invalid label name at %q", p.rest())
		}
		start := p.i
		p.i++
		for p.i < len(p.s) && isLabelNameCont(p.s[p.i]) {
			p.i++
		}
		name := p.s[start:p.i]

		p.skipSpaces()
		if p.peek() != '=' {
			return nil, fmt.Errorf("expected '=' after label %q", name)
		}
		p.i++ // consume =
		p.skipSpaces()
		val, err := p.quotedValue()
		if err != nil {
			return nil, err
		}
		out = append(out, name, val)

		p.skipSpaces()
		switch p.peek() {
		case ',':
			p.i++
		case '}':
			p.i++
			return out, nil
		default:
			return nil, fmt.Errorf("expected ',' or '}' at %q", p.rest())
		}
	}
}

// quotedValue parses a double-quoted, escaped label value.
func (p *parser) quotedValue() (string, error) {
	if p.peek() != '"' {
		return "", fmt.Errorf("expected '\"' at %q", p.rest())
	}
	p.i++ // opening quote
	var b strings.Builder
	for p.i < len(p.s) {
		c := p.s[p.i]
		switch c {
		case '"':
			p.i++
			return b.String(), nil
		case '\\':
			if p.i+1 >= len(p.s) {
				return "", fmt.Errorf("dangling escape in label value")
			}
			switch p.s[p.i+1] {
			case 'n':
				b.WriteByte('\n')
			case '"':
				b.WriteByte('"')
			case '\\':
				b.WriteByte('\\')
			default:
				b.WriteByte(p.s[p.i+1])
			}
			p.i += 2
		default:
			b.WriteByte(c)
			p.i++
		}
	}
	return "", fmt.Errorf("unterminated label value")
}
