package model

import "testing"

func TestFromStringsSortsAndDedupes(t *testing.T) {
	l := FromStrings("__name__", "http_requests_total", "method", "get", "job", "api")
	// Expect sorted by name: __name__, job, method
	want := []Label{
		{Name: "__name__", Value: "http_requests_total"},
		{Name: "job", Value: "api"},
		{Name: "method", Value: "get"},
	}
	if len(l) != len(want) {
		t.Fatalf("len = %d, want %d (%v)", len(l), len(want), l)
	}
	for i := range want {
		if l[i] != want[i] {
			t.Errorf("label[%d] = %+v, want %+v", i, l[i], want[i])
		}
	}
}

func TestFromStringsOddPanicsAvoided(t *testing.T) {
	// Odd number of strings: trailing name without value is dropped.
	l := FromStrings("a", "1", "b")
	if l.Get("b") != "" {
		t.Errorf("expected dangling label dropped, got %q", l.Get("b"))
	}
	if l.Get("a") != "1" {
		t.Errorf("a = %q, want 1", l.Get("a"))
	}
}

func TestLabelsGetHasMap(t *testing.T) {
	l := FromMap(map[string]string{"__name__": "up", "instance": "localhost:9090"})
	if got := l.Get("__name__"); got != "up" {
		t.Errorf("Get(__name__) = %q", got)
	}
	if got := l.Get("missing"); got != "" {
		t.Errorf("Get(missing) = %q, want empty", got)
	}
	if !l.Has("instance") || l.Has("nope") {
		t.Errorf("Has wrong: instance=%v nope=%v", l.Has("instance"), l.Has("nope"))
	}
	m := l.Map()
	if m["instance"] != "localhost:9090" || len(m) != 2 {
		t.Errorf("Map = %v", m)
	}
}

func TestLabelsHashStableAndOrderIndependent(t *testing.T) {
	a := FromStrings("__name__", "x", "a", "1", "b", "2")
	b := FromStrings("b", "2", "__name__", "x", "a", "1")
	if a.Hash() != b.Hash() {
		t.Errorf("hash not order-independent: %d vs %d", a.Hash(), b.Hash())
	}
	c := FromStrings("__name__", "x", "a", "1", "b", "3")
	if a.Hash() == c.Hash() {
		t.Errorf("hash collision for distinct label sets")
	}
}

func TestLabelsString(t *testing.T) {
	tests := []struct {
		name string
		in   Labels
		want string
	}{
		{"named", FromStrings("__name__", "http_requests_total", "job", "api", "method", "get"), `http_requests_total{job="api",method="get"}`},
		{"anonymous", FromStrings("a", "1", "b", "2"), `{a="1",b="2"}`},
		{"escaping", FromStrings("__name__", "m", "l", "a\"b\\c\nd"), `m{l="a\"b\\c\nd"}`},
		{"empty", Labels{}, `{}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLabelsEqual(t *testing.T) {
	a := FromStrings("a", "1", "b", "2")
	b := FromStrings("b", "2", "a", "1")
	c := FromStrings("a", "1")
	if !a.Equal(b) {
		t.Errorf("a should equal b")
	}
	if a.Equal(c) {
		t.Errorf("a should not equal c")
	}
}

func TestMatcherMatches(t *testing.T) {
	tests := []struct {
		typ   MatchType
		value string
		input string
		want  bool
	}{
		{MatchEqual, "get", "get", true},
		{MatchEqual, "get", "post", false},
		{MatchNotEqual, "get", "post", true},
		{MatchNotEqual, "get", "get", false},
		{MatchRegexp, "g.*", "get", true},
		{MatchRegexp, "g.*", "post", false},
		{MatchRegexp, "get|post", "post", true},
		{MatchRegexp, "et", "get", false}, // anchored: must match full string
		{MatchNotRegexp, "g.*", "post", true},
		{MatchNotRegexp, "g.*", "get", false},
	}
	for _, tc := range tests {
		m, err := NewMatcher(tc.typ, "method", tc.value)
		if err != nil {
			t.Fatalf("NewMatcher(%v,%q): %v", tc.typ, tc.value, err)
		}
		if got := m.Matches(tc.input); got != tc.want {
			t.Errorf("matcher(%v %q).Matches(%q) = %v, want %v", tc.typ, tc.value, tc.input, got, tc.want)
		}
	}
}

func TestMatcherBadRegexp(t *testing.T) {
	if _, err := NewMatcher(MatchRegexp, "method", "([)"); err == nil {
		t.Errorf("expected error for invalid regexp")
	}
}

func TestMetricTypeParseString(t *testing.T) {
	tests := []struct {
		in  string
		typ MetricType
	}{
		{"counter", Counter},
		{"gauge", Gauge},
		{"histogram", Histogram},
		{"summary", Summary},
		{"untyped", Untyped},
		{"bogus", Untyped},
	}
	for _, tc := range tests {
		if got := ParseMetricType(tc.in); got != tc.typ {
			t.Errorf("ParseMetricType(%q) = %v, want %v", tc.in, got, tc.typ)
		}
		if tc.in != "bogus" && tc.typ != Untyped && tc.typ.String() != tc.in {
			t.Errorf("MetricType(%v).String() = %q, want %q", tc.typ, tc.typ.String(), tc.in)
		}
	}
}
