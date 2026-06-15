// Package tsdbtest provides a reusable conformance suite that pins the contract
// of any tsdb.Storage implementation. Keeping it in its own package (rather than
// a _test.go file) lets multiple storage backends share it.
package tsdbtest

import (
	"testing"

	"github.com/pod32g/omni-metrics/internal/model"
	"github.com/pod32g/omni-metrics/internal/tsdb"
)

// RunConformance exercises the Storage contract against a freshly-opened store
// produced by newStorage.
func RunConformance(t *testing.T, newStorage func() tsdb.Storage) {
	t.Helper()

	t.Run("append and select", func(t *testing.T) {
		s := newStorage()
		defer s.Close()
		appendSeries(t, s, model.FromStrings(model.MetricName, "m", "job", "a"),
			model.Sample{T: 1, V: 10}, model.Sample{T: 2, V: 20})
		appendSeries(t, s, model.FromStrings(model.MetricName, "m", "job", "b"),
			model.Sample{T: 1, V: 30})

		got := drain(t, s.Querier().Select(0, 100, eq(t, model.MetricName, "m")))
		if len(got) != 2 {
			t.Fatalf("got %d series, want 2", len(got))
		}
		if vs := got[`m{job="a"}`]; len(vs) != 2 || vs[1].V != 20 {
			t.Errorf("job=a samples = %v", vs)
		}
	})

	t.Run("time range filters samples", func(t *testing.T) {
		s := newStorage()
		defer s.Close()
		appendSeries(t, s, model.FromStrings(model.MetricName, "m"),
			model.Sample{T: 10, V: 1}, model.Sample{T: 20, V: 2}, model.Sample{T: 30, V: 3})
		got := drain(t, s.Querier().Select(15, 25, eq(t, model.MetricName, "m")))
		if vs := got[`m{}`]; len(vs) != 1 || vs[0].T != 20 {
			t.Errorf("range select = %v, want single @20", vs)
		}
	})

	t.Run("regex and negative matchers", func(t *testing.T) {
		s := newStorage()
		defer s.Close()
		appendSeries(t, s, model.FromStrings(model.MetricName, "http_a", "job", "x"), model.Sample{T: 1, V: 1})
		appendSeries(t, s, model.FromStrings(model.MetricName, "http_b", "job", "y"), model.Sample{T: 1, V: 1})
		appendSeries(t, s, model.FromStrings(model.MetricName, "other", "job", "x"), model.Sample{T: 1, V: 1})

		re := drain(t, s.Querier().Select(0, 10, matcher(t, model.MatchRegexp, model.MetricName, "http_.*")))
		if len(re) != 2 {
			t.Errorf("regex select got %d, want 2", len(re))
		}
		neg := drain(t, s.Querier().Select(0, 10, matcher(t, model.MatchNotEqual, "job", "x")))
		if len(neg) != 1 {
			t.Errorf("negative select got %d, want 1", len(neg))
		}
	})

	t.Run("label introspection", func(t *testing.T) {
		s := newStorage()
		defer s.Close()
		appendSeries(t, s, model.FromStrings(model.MetricName, "m", "job", "a"), model.Sample{T: 1, V: 1})
		appendSeries(t, s, model.FromStrings(model.MetricName, "m", "job", "b"), model.Sample{T: 1, V: 1})
		q := s.Querier()
		if vs := q.LabelValues("job"); len(vs) != 2 {
			t.Errorf("LabelValues(job) = %v, want 2", vs)
		}
		names := q.LabelNames()
		if !hasString(names, model.MetricName) || !hasString(names, "job") {
			t.Errorf("LabelNames = %v", names)
		}
	})
}

func appendSeries(t *testing.T, s tsdb.Storage, l model.Labels, samples ...model.Sample) {
	t.Helper()
	app := s.Appender()
	for _, sm := range samples {
		if _, err := app.Append(l, sm.T, sm.V); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	if err := app.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func drain(t *testing.T, ss tsdb.SeriesSet) map[string][]model.Sample {
	t.Helper()
	out := map[string][]model.Sample{}
	for ss.Next() {
		out[ss.At().Labels().String()] = ss.At().Samples()
	}
	if err := ss.Err(); err != nil {
		t.Fatalf("series set err: %v", err)
	}
	return out
}

func eq(t *testing.T, name, val string) model.Matcher { return matcher(t, model.MatchEqual, name, val) }

func matcher(t *testing.T, typ model.MatchType, name, val string) model.Matcher {
	t.Helper()
	m, err := model.NewMatcher(typ, name, val)
	if err != nil {
		t.Fatal(err)
	}
	return *m
}

func hasString(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
