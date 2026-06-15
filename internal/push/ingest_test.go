package push

import (
	"errors"
	"sync"
	"testing"

	"github.com/pod32g/omni-metrics/internal/model"
	"github.com/pod32g/omni-metrics/internal/tsdb"
)

func newDB(t *testing.T, maxSeries int) *tsdb.DB {
	t.Helper()
	db, err := tsdb.Open(tsdb.Options{MaxSeries: maxSeries})
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func ptr(f float64) *Value { v := Value(f); return &v }

// ptr2 is a samples-field helper: SamplePoint.Value is a Value, not *Value.
func ptr2(f float64) Value { return Value(f) }

func TestIngestAppendsSeries(t *testing.T) {
	db := newDB(t, 0)
	ing := NewIngester(db, 0)
	req := &Request{Job: "app", Series: []SeriesInput{
		{Name: "reqs_total", Value: ptr(5)},
		{Name: "latency", Samples: []SamplePoint{{TimestampMs: 1000, Value: ptr2(0.1)}, {TimestampMs: 2000, Value: ptr2(0.2)}}},
	}}
	res, err := ing.Ingest(req, "10.0.0.9", 9999)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if res.SeriesTouched != 2 || res.SamplesAppended != 3 {
		t.Fatalf("res = %+v, want 2 series / 3 samples", res)
	}
	// instance defaulted to remote host
	q := db.Querier()
	ss := q.Select(0, 10000, model.Matcher{Type: model.MatchEqual, Name: "instance", Value: "10.0.0.9"})
	got := 0
	for ss.Next() {
		got++
	}
	if got != 2 {
		t.Errorf("series with instance=10.0.0.9 = %d, want 2", got)
	}
}

func TestIngestRollsBackOnCardinalityCap(t *testing.T) {
	db := newDB(t, 1) // head holds only 1 series
	ing := NewIngester(db, 0)
	req := &Request{Job: "app", Series: []SeriesInput{
		{Name: "a", Value: ptr(1)},
		{Name: "b", Value: ptr(2)}, // creating this exceeds the cap
	}}
	_, err := ing.Ingest(req, "h", 100)
	if err == nil {
		t.Fatal("expected cardinality error")
	}
	var ie *IngestError
	if !errors.As(err, &ie) || ie.Kind != ErrInternal {
		t.Fatalf("want ErrInternal IngestError, got %v", err)
	}
	if !errors.Is(err, tsdb.ErrTooManySeries) {
		t.Errorf("error should unwrap to ErrTooManySeries: %v", err)
	}
	// Atomic: nothing from this push should be queryable.
	q := db.Querier()
	ss := q.Select(0, 1000, model.Matcher{Type: model.MatchEqual, Name: "job", Value: "app"})
	if ss.Next() {
		t.Error("rolled-back push left data behind")
	}
	// And the rolled-back push must not have burned the cardinality budget: the
	// (empty) series it created up to the cap must be reclaimed (guards the
	// push cardinality-DoS class).
	if db.HeadSeries() != 0 {
		t.Errorf("rolled-back over-cap push leaked %d series into the cap", db.HeadSeries())
	}
}

func TestIngestSampleLimit(t *testing.T) {
	db := newDB(t, 0)
	ing := NewIngester(db, 1) // at most 1 sample per push
	req := &Request{Job: "app", Series: []SeriesInput{{Name: "a", Value: ptr(1)}, {Name: "b", Value: ptr(2)}}}
	_, err := ing.Ingest(req, "h", 100)
	var ie *IngestError
	if !errors.As(err, &ie) || ie.Kind != ErrBadData {
		t.Fatalf("want ErrBadData, got %v", err)
	}
}

func TestIngestValidationError(t *testing.T) {
	db := newDB(t, 0)
	ing := NewIngester(db, 0)
	_, err := ing.Ingest(&Request{Job: "", Series: nil}, "h", 1)
	var ie *IngestError
	if !errors.As(err, &ie) || ie.Kind != ErrBadData {
		t.Fatalf("want ErrBadData, got %v", err)
	}
}

func TestSourcesHealth(t *testing.T) {
	db := newDB(t, 0)
	ing := NewIngester(db, 0)
	_, _ = ing.Ingest(&Request{Job: "app", Instance: "i1", Series: []SeriesInput{{Name: "a", Value: ptr(1)}}}, "h", 100)
	_, _ = ing.Ingest(&Request{Job: "app", Instance: "i1", Series: []SeriesInput{{Name: "a", Value: ptr(2)}}}, "h", 200)
	srcs := ing.Sources()
	if len(srcs) != 1 {
		t.Fatalf("sources = %d, want 1", len(srcs))
	}
	s := srcs[0]
	if s.Job != "app" || s.Instance != "i1" {
		t.Errorf("identity = %s/%s", s.Job, s.Instance)
	}
	if s.PushesTotal != 2 || s.SamplesTotal != 2 || s.LastError != "" {
		t.Errorf("health = %+v", s)
	}
}

func TestIngestConcurrent(t *testing.T) {
	db := newDB(t, 0)
	ing := NewIngester(db, 0)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			req := &Request{Job: "app", Instance: "i", Series: []SeriesInput{{Name: "a", Samples: []SamplePoint{{TimestampMs: int64(n + 1), Value: ptr2(float64(n))}}}}}
			_, _ = ing.Ingest(req, "h", int64(n+1))
		}(i)
	}
	wg.Wait()
	if len(ing.Sources()) != 1 {
		t.Errorf("expected 1 source")
	}
}
