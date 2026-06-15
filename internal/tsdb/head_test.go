package tsdb

import (
	"testing"

	"github.com/pod32g/omni-metrics/internal/model"
)

func TestHeadGetOrCreateStableRef(t *testing.T) {
	h := newHead(0, 0)
	l := model.FromStrings(model.MetricName, "m", "a", "1")
	ref1, created1 := h.getOrCreate(l)
	ref2, created2 := h.getOrCreate(l)
	if ref1 != ref2 {
		t.Errorf("ref not stable: %d vs %d", ref1, ref2)
	}
	if !created1 || created2 {
		t.Errorf("created flags wrong: %v %v", created1, created2)
	}
}

func TestHeadMatcherSemantics(t *testing.T) {
	h := newHead(0, 0)
	add := func(name, job string, ts int64) {
		l := model.FromStrings(model.MetricName, name, "job", job)
		ref, _ := h.getOrCreate(l)
		h.appendSample(ref, ts, 1)
	}
	add("http_requests", "api", 10)
	add("http_requests", "web", 10)
	add("http_errors", "api", 10)

	cases := []struct {
		name     string
		matchers []model.Matcher
		want     int
	}{
		{"equal name", matchers(t, model.MatchEqual, model.MetricName, "http_requests"), 2},
		{"regex name", matchers(t, model.MatchRegexp, model.MetricName, "http_.*"), 3},
		{"neg job", matchers(t, model.MatchNotEqual, "job", "api"), 1},
		{"absent label equals empty", matchers(t, model.MatchEqual, "missing", ""), 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := h.query(0, 100, tc.matchers)
			if len(res) != tc.want {
				t.Errorf("got %d series, want %d", len(res), tc.want)
			}
		})
	}
}

func TestHeadTruncateRetention(t *testing.T) {
	h := newHead(0, 0)
	l := model.FromStrings(model.MetricName, "m")
	ref, _ := h.getOrCreate(l)
	for ts := int64(0); ts <= 1000; ts += 100 {
		h.appendSample(ref, ts, float64(ts))
	}
	h.truncate(500) // drop samples with T < 500
	res := h.query(0, 2000, matchers(t, model.MatchEqual, model.MetricName, "m"))
	if len(res) != 1 {
		t.Fatalf("want 1 series, got %d", len(res))
	}
	for _, s := range res[0].samples {
		if s.T < 500 {
			t.Errorf("sample %d should have been truncated", s.T)
		}
	}
	if len(res[0].samples) != 6 { // 500,600,...,1000
		t.Errorf("want 6 samples after truncation, got %d", len(res[0].samples))
	}
}

func TestHeadLabelIntrospection(t *testing.T) {
	h := newHead(0, 0)
	for _, job := range []string{"api", "web", "api"} {
		ref, _ := h.getOrCreate(model.FromStrings(model.MetricName, "m", "job", job))
		h.appendSample(ref, 1, 1)
	}
	names := h.labelNames(nil)
	if !contains(names, "__name__") || !contains(names, "job") {
		t.Errorf("label names = %v", names)
	}
	vals := h.labelValues("job", nil)
	if len(vals) != 2 || !contains(vals, "api") || !contains(vals, "web") {
		t.Errorf("job values = %v, want [api web]", vals)
	}
}

func TestAppendDedupesDuplicateTimestamp(t *testing.T) {
	h := newHead(0, 0)
	ref, _ := h.getOrCreate(model.FromStrings(model.MetricName, "m"))
	h.appendSample(ref, 100, 1)
	h.appendSample(ref, 100, 2) // duplicate timestamp: must be dropped, keeping the first
	h.appendSample(ref, 50, 9)  // out-of-order duplicate region: distinct ts, inserted
	h.appendSample(ref, 50, 8)  // duplicate of the inserted ts: dropped
	res := h.query(0, 200, matchers(t, model.MatchEqual, model.MetricName, "m"))
	if len(res) != 1 {
		t.Fatalf("want 1 series, got %d", len(res))
	}
	got := res[0].samples
	if len(got) != 2 || got[0] != (model.Sample{T: 50, V: 9}) || got[1] != (model.Sample{T: 100, V: 1}) {
		t.Fatalf("duplicate timestamps not deduped (keep first): %v", got)
	}
}

func TestTruncateReclaimsEmptiedSeries(t *testing.T) {
	h := newHead(0, 2) // cardinality cap of 2
	for _, n := range []string{"a", "b"} {
		ref, _ := h.getOrCreate(model.FromStrings(model.MetricName, n))
		h.appendSample(ref, 1, 1)
	}
	h.truncate(100) // all samples (t=1) age out
	if len(h.series) != 0 {
		t.Fatalf("emptied series not reclaimed: %d remain", len(h.series))
	}
	if len(h.postings) != 0 {
		t.Errorf("postings not pruned after GC: %v", h.postings)
	}
	// The freed cardinality budget must allow a new series.
	if _, _, ok := h.getOrCreateLimited(model.FromStrings(model.MetricName, "c"), 2); !ok {
		t.Errorf("cardinality cap not freed after reclaiming empty series")
	}
}

func TestTruncateKeepsSeriesWithLiveSamples(t *testing.T) {
	h := newHead(0, 0)
	ref, _ := h.getOrCreate(model.FromStrings(model.MetricName, "m"))
	h.appendSample(ref, 100, 1)
	h.appendSample(ref, 600, 2)
	h.truncate(500) // drops t=100, keeps t=600
	if len(h.series) != 1 {
		t.Fatalf("series with live samples wrongly reclaimed")
	}
}

func matchers(t *testing.T, typ model.MatchType, name, val string) []model.Matcher {
	t.Helper()
	m, err := model.NewMatcher(typ, name, val)
	if err != nil {
		t.Fatal(err)
	}
	return []model.Matcher{*m}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
