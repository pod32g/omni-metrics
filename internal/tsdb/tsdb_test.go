package tsdb_test

import (
	"path/filepath"
	"sync"
	"testing"

	"github.com/pod32g/omni-metrics/internal/model"
	"github.com/pod32g/omni-metrics/internal/tsdb"
	"github.com/pod32g/omni-metrics/internal/tsdb/tsdbtest"
)

func mustMatcher(t *testing.T, typ model.MatchType, name, val string) model.Matcher {
	t.Helper()
	m, err := model.NewMatcher(typ, name, val)
	if err != nil {
		t.Fatal(err)
	}
	return *m
}

// collect drains a SeriesSet into a map of labels-string -> samples.
func collect(t *testing.T, ss tsdb.SeriesSet) map[string][]model.Sample {
	t.Helper()
	out := map[string][]model.Sample{}
	for ss.Next() {
		s := ss.At()
		out[s.Labels().String()] = s.Samples()
	}
	if err := ss.Err(); err != nil {
		t.Fatalf("series set error: %v", err)
	}
	return out
}

func TestConformance(t *testing.T) {
	tsdbtest.RunConformance(t, func() tsdb.Storage {
		db, err := tsdb.Open(tsdb.Options{})
		if err != nil {
			t.Fatal(err)
		}
		return db
	})
}

func TestAppendAndSelectRange(t *testing.T) {
	db, err := tsdb.Open(tsdb.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	app := db.Appender()
	lbls := model.FromStrings(model.MetricName, "m", "job", "a")
	for _, s := range []model.Sample{{T: 100, V: 1}, {T: 200, V: 2}, {T: 300, V: 3}} {
		if _, err := app.Append(lbls, s.T, s.V); err != nil {
			t.Fatal(err)
		}
	}
	if err := app.Commit(); err != nil {
		t.Fatal(err)
	}

	q := db.Querier()
	got := collect(t, q.Select(150, 250, mustMatcher(t, model.MatchEqual, model.MetricName, "m")))
	samples := got[`m{job="a"}`]
	if len(samples) != 1 || samples[0].T != 200 {
		t.Fatalf("range select = %v, want single sample @200", samples)
	}
}

func TestWALCrashRecovery(t *testing.T) {
	dir := t.TempDir()
	lbls := model.FromStrings(model.MetricName, "persisted", "job", "x")

	// Write and commit, then close cleanly.
	db, err := tsdb.Open(tsdb.Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	app := db.Appender()
	if _, err := app.Append(lbls, 1000, 11); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Append(lbls, 2000, 22); err != nil {
		t.Fatal(err)
	}
	if err := app.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen: data must be recovered from the WAL.
	db2, err := tsdb.Open(tsdb.Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	got := collect(t, db2.Querier().Select(0, 9999, mustMatcher(t, model.MatchEqual, model.MetricName, "persisted")))
	samples := got[`persisted{job="x"}`]
	if len(samples) != 2 || samples[0].V != 11 || samples[1].V != 22 {
		t.Fatalf("recovered samples = %v, want [11,22]", samples)
	}
}

func TestWALReplayIdempotent(t *testing.T) {
	dir := t.TempDir()
	lbls := model.FromStrings(model.MetricName, "m")
	db, _ := tsdb.Open(tsdb.Options{Dir: dir})
	app := db.Appender()
	app.Append(lbls, 1, 1)
	app.Commit()
	db.Close()

	// Open twice in a row; the second open replays the same WAL again.
	db2, _ := tsdb.Open(tsdb.Options{Dir: dir})
	db2.Close()
	db3, _ := tsdb.Open(tsdb.Options{Dir: dir})
	defer db3.Close()

	got := collect(t, db3.Querier().Select(0, 100, mustMatcher(t, model.MatchEqual, model.MetricName, "m")))
	if s := got[`m{}`]; len(s) != 1 {
		t.Fatalf("idempotent replay should yield exactly 1 sample, got %v", s)
	}
}

func TestCardinalityCap(t *testing.T) {
	db, _ := tsdb.Open(tsdb.Options{MaxSeries: 2})
	defer db.Close()
	app := db.Appender()
	_, e1 := app.Append(model.FromStrings(model.MetricName, "a"), 1, 1)
	_, e2 := app.Append(model.FromStrings(model.MetricName, "b"), 1, 1)
	_, e3 := app.Append(model.FromStrings(model.MetricName, "c"), 1, 1)
	if e1 != nil || e2 != nil {
		t.Fatalf("first two series should be accepted: %v %v", e1, e2)
	}
	if e3 == nil {
		t.Fatalf("third distinct series should be rejected by MaxSeries cap")
	}
}

func TestConcurrentAppends(t *testing.T) {
	db, _ := tsdb.Open(tsdb.Options{})
	defer db.Close()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			app := db.Appender()
			lbls := model.FromStrings(model.MetricName, "m", "g", string(rune('A'+g)))
			for i := 0; i < 50; i++ {
				if _, err := app.Append(lbls, int64(i), float64(i)); err != nil {
					t.Errorf("append: %v", err)
				}
			}
			if err := app.Commit(); err != nil {
				t.Errorf("commit: %v", err)
			}
		}(g)
	}
	wg.Wait()
	got := collect(t, db.Querier().Select(0, 1000, mustMatcher(t, model.MatchEqual, model.MetricName, "m")))
	if len(got) != 8 {
		t.Fatalf("expected 8 distinct series, got %d", len(got))
	}
}

func TestSegmentFilesCreated(t *testing.T) {
	dir := t.TempDir()
	db, _ := tsdb.Open(tsdb.Options{Dir: dir})
	app := db.Appender()
	app.Append(model.FromStrings(model.MetricName, "m"), 1, 1)
	app.Commit()
	db.Close()
	segs, _ := filepath.Glob(filepath.Join(dir, "*.wal"))
	if len(segs) == 0 {
		t.Fatalf("expected at least one .wal segment in %s", dir)
	}
}
