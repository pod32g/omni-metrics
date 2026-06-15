package exposition

import (
	"math"
	"strings"
	"testing"

	"github.com/pod32g/omni-metrics/internal/model"
)

func parse(t *testing.T, input string) *Result {
	t.Helper()
	res, err := Parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return res
}

func TestParseSimple(t *testing.T) {
	res := parse(t, "go_goroutines 42\n")
	if len(res.Series) != 1 {
		t.Fatalf("got %d series, want 1", len(res.Series))
	}
	s := res.Series[0]
	if s.Labels.Get(model.MetricName) != "go_goroutines" {
		t.Errorf("name = %q", s.Labels.Get(model.MetricName))
	}
	if s.Value != 42 {
		t.Errorf("value = %v", s.Value)
	}
	if s.Timestamp != nil {
		t.Errorf("timestamp = %v, want nil", *s.Timestamp)
	}
}

func TestParseLabelsAndTimestamp(t *testing.T) {
	res := parse(t, `http_requests_total{method="get",code="200"} 1027 1395066363000`+"\n")
	s := res.Series[0]
	if s.Labels.Get("method") != "get" || s.Labels.Get("code") != "200" {
		t.Errorf("labels = %v", s.Labels)
	}
	if s.Labels.Get(model.MetricName) != "http_requests_total" {
		t.Errorf("name = %q", s.Labels.Get(model.MetricName))
	}
	if s.Value != 1027 {
		t.Errorf("value = %v", s.Value)
	}
	if s.Timestamp == nil || *s.Timestamp != 1395066363000 {
		t.Errorf("timestamp = %v", s.Timestamp)
	}
}

func TestParseTypeAndHelp(t *testing.T) {
	input := `# HELP http_requests_total The total number of HTTP requests.
# TYPE http_requests_total counter
http_requests_total{code="200"} 5
`
	res := parse(t, input)
	md, ok := res.Metadata["http_requests_total"]
	if !ok {
		t.Fatalf("no metadata for http_requests_total")
	}
	if md.Type != model.Counter {
		t.Errorf("type = %v, want counter", md.Type)
	}
	if !strings.Contains(md.Help, "total number of HTTP requests") {
		t.Errorf("help = %q", md.Help)
	}
}

func TestParseSpecialFloats(t *testing.T) {
	res := parse(t, "a +Inf\nb -Inf\nc NaN\nd 1.5e3\ne -0.5\n")
	byName := map[string]float64{}
	nan := false
	for _, s := range res.Series {
		n := s.Labels.Get(model.MetricName)
		if n == "c" && math.IsNaN(s.Value) {
			nan = true
		}
		byName[n] = s.Value
	}
	if !math.IsInf(byName["a"], 1) {
		t.Errorf("a = %v, want +Inf", byName["a"])
	}
	if !math.IsInf(byName["b"], -1) {
		t.Errorf("b = %v, want -Inf", byName["b"])
	}
	if !nan {
		t.Errorf("c should be NaN, got %v", byName["c"])
	}
	if byName["d"] != 1500 {
		t.Errorf("d = %v, want 1500", byName["d"])
	}
	if byName["e"] != -0.5 {
		t.Errorf("e = %v, want -0.5", byName["e"])
	}
}

func TestParseEscapedLabelValue(t *testing.T) {
	res := parse(t, `m{l="a\"b\\c\nd"} 1`+"\n")
	got := res.Series[0].Labels.Get("l")
	want := "a\"b\\c\nd"
	if got != want {
		t.Errorf("label value = %q, want %q", got, want)
	}
}

func TestParseEmptyBracesAndComments(t *testing.T) {
	input := `# a bare comment, ignore me
m{} 7

m2 8
`
	res := parse(t, input)
	if len(res.Series) != 2 {
		t.Fatalf("got %d series, want 2: %v", len(res.Series), res.Series)
	}
	if res.Series[0].Labels.Get(model.MetricName) != "m" || res.Series[0].Value != 7 {
		t.Errorf("series[0] = %v", res.Series[0])
	}
}

func TestParseSpaceLeniency(t *testing.T) {
	res := parse(t, `m{ a = "b" ,c= "d" } 9`+"\n")
	s := res.Series[0]
	if s.Labels.Get("a") != "b" || s.Labels.Get("c") != "d" || s.Value != 9 {
		t.Errorf("lenient parse failed: %v value=%v", s.Labels, s.Value)
	}
}

func TestParseHistogramBucket(t *testing.T) {
	res := parse(t, `request_duration_seconds_bucket{le="0.1"} 5`+"\n")
	s := res.Series[0]
	if s.Labels.Get(model.MetricName) != "request_duration_seconds_bucket" || s.Labels.Get("le") != "0.1" {
		t.Errorf("bucket labels = %v", s.Labels)
	}
}

func TestParseIgnoresExplicitNameLabel(t *testing.T) {
	// A target must not be able to forge a reserved series by smuggling a
	// __name__ label inside the braces; the positional metric name always wins.
	res := parse(t, `evil{__name__="up",job="x"} 0`+"\n")
	s := res.Series[0]
	if s.Labels.Get(model.MetricName) != "evil" {
		t.Errorf("metric name = %q, want evil (smuggled __name__ must be ignored)", s.Labels.Get(model.MetricName))
	}
	if s.Labels.Get("job") != "x" {
		t.Errorf("other labels should be preserved: %v", s.Labels)
	}
}

func TestParseLenientSkipsBadLines(t *testing.T) {
	res, err := Parse(strings.NewReader("good 1\nbad line here nope\ngood2 2\n"))
	if err == nil {
		t.Errorf("expected an error reporting the bad line")
	}
	if res == nil || len(res.Series) != 2 {
		t.Fatalf("expected 2 good series despite bad line, got %v", res)
	}
	names := []string{res.Series[0].Labels.Get(model.MetricName), res.Series[1].Labels.Get(model.MetricName)}
	if names[0] != "good" || names[1] != "good2" {
		t.Errorf("good series = %v", names)
	}
}
