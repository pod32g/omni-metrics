package evaluator_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pod32g/omni-metrics/internal/alerts/datasource"
	"github.com/pod32g/omni-metrics/internal/alerts/evaluator"
	"github.com/pod32g/omni-metrics/internal/alerts/models"
	"github.com/pod32g/omni-metrics/internal/alerts/storage"
)

// fakeDS returns a scripted result/error and ignores the query.
type fakeDS struct {
	res models.Result
	err error
}

func (f *fakeDS) Query(context.Context, string, time.Time) (models.Result, error) {
	return f.res, f.err
}

func newStore(t *testing.T) storage.Store {
	t.Helper()
	s, err := storage.OpenSQLite(":memory:")
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func evalWith(t *testing.T, st storage.Store, ds datasource.Datasource, max int, now time.Time) *evaluator.Evaluator {
	t.Helper()
	return evaluator.New(st, func(models.Datasource) datasource.Datasource { return ds }, max, func() time.Time { return now })
}

func vec(samples ...models.Sample) models.Result {
	if len(samples) == 0 {
		return models.Result{Kind: models.KindEmpty}
	}
	return models.Result{Kind: models.KindVector, Samples: samples}
}

func TestEvaluateMultiSeriesFiringImmediately(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	now := time.Unix(1000, 0).UTC()
	ds := &fakeDS{res: vec(
		models.Sample{Labels: map[string]string{"instance": "a"}, Value: 7},
		models.Sample{Labels: map[string]string{"instance": "b"}, Value: 9},
	)}
	rule := models.Rule{ID: "r1", PromQL: "up==0", ForS: 0, Severity: "critical", Labels: map[string]string{"team": "x"}}

	out := evalWith(t, st, ds, 1000, now).EvaluateRule(ctx, rule, models.Datasource{})
	if out.Err != nil {
		t.Fatalf("Err: %v", out.Err)
	}
	if out.Active != 2 || out.Transitions != 2 {
		t.Fatalf("active=%d transitions=%d, want 2/2", out.Active, out.Transitions)
	}
	active, _ := st.ListInstancesByRule(ctx, "r1")
	if len(active) != 2 {
		t.Fatalf("active instances = %d", len(active))
	}
	for _, in := range active {
		if in.State != models.StateFiring {
			t.Errorf("instance %s state=%v", in.Fingerprint, in.State)
		}
		if in.Labels["team"] != "x" {
			t.Errorf("rule label not merged: %v", in.Labels)
		}
	}
	hist, _ := st.History(ctx, storage.HistoryFilter{})
	if len(hist) != 2 {
		t.Errorf("history = %d, want 2", len(hist))
	}
}

func TestEvaluateNoChangeNoHistory(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	now := time.Unix(1000, 0).UTC()
	ds := &fakeDS{res: vec(models.Sample{Labels: map[string]string{"instance": "a"}, Value: 7})}
	rule := models.Rule{ID: "r1", PromQL: "x", ForS: 0}
	e := evalWith(t, st, ds, 1000, now)

	e.EvaluateRule(ctx, rule, models.Datasource{})
	ds.res = vec(models.Sample{Labels: map[string]string{"instance": "a"}, Value: 12}) // value changes, state doesn't
	out := e.EvaluateRule(ctx, rule, models.Datasource{})
	if out.Transitions != 0 {
		t.Errorf("transitions = %d, want 0", out.Transitions)
	}
	hist, _ := st.History(ctx, storage.HistoryFilter{})
	if len(hist) != 1 {
		t.Errorf("history = %d, want 1 (no new transition)", len(hist))
	}
	active, _ := st.ListInstancesByRule(ctx, "r1")
	if len(active) != 1 || active[0].CurrentValue != 12 {
		t.Errorf("value not refreshed: %+v", active)
	}
}

func TestEvaluateResolveWhenSeriesDisappears(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	now := time.Unix(1000, 0).UTC()
	ds := &fakeDS{res: vec(
		models.Sample{Labels: map[string]string{"instance": "a"}, Value: 1},
		models.Sample{Labels: map[string]string{"instance": "b"}, Value: 1},
	)}
	rule := models.Rule{ID: "r1", PromQL: "x", ForS: 0}
	e := evalWith(t, st, ds, 1000, now)
	e.EvaluateRule(ctx, rule, models.Datasource{})

	ds.res = vec(models.Sample{Labels: map[string]string{"instance": "a"}, Value: 1}) // b disappears
	out := e.EvaluateRule(ctx, rule, models.Datasource{})
	if out.Active != 1 || out.Transitions != 1 {
		t.Fatalf("active=%d transitions=%d, want 1/1", out.Active, out.Transitions)
	}
	active, _ := st.ListInstancesByRule(ctx, "r1")
	if len(active) != 1 || active[0].Labels["instance"] != "a" {
		t.Fatalf("active = %+v", active)
	}
	hist, _ := st.History(ctx, storage.HistoryFilter{})
	// 2 firing + 1 resolve = 3.
	if len(hist) != 3 {
		t.Fatalf("history = %d, want 3", len(hist))
	}
	last := hist[len(hist)-1]
	if last.New != models.StateResolved {
		t.Errorf("last transition = %v, want resolved", last.New)
	}
}

func TestEvaluateForDuration(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	t0 := time.Unix(1000, 0).UTC()
	ds := &fakeDS{res: vec(models.Sample{Labels: map[string]string{"i": "a"}, Value: 1})}
	rule := models.Rule{ID: "r1", PromQL: "x", ForS: 60}

	out := evalWith(t, st, ds, 1000, t0).EvaluateRule(ctx, rule, models.Datasource{})
	if out.Pending != 1 || out.Active != 0 {
		t.Fatalf("first eval pending=%d active=%d, want 1/0", out.Pending, out.Active)
	}
	// 30s later: still pending.
	out = evalWith(t, st, ds, 1000, t0.Add(30*time.Second)).EvaluateRule(ctx, rule, models.Datasource{})
	if out.Pending != 1 || out.Active != 0 {
		t.Fatalf("30s eval pending=%d active=%d, want 1/0", out.Pending, out.Active)
	}
	// 60s later: fires.
	out = evalWith(t, st, ds, 1000, t0.Add(60*time.Second)).EvaluateRule(ctx, rule, models.Datasource{})
	if out.Active != 1 || out.Transitions != 1 {
		t.Fatalf("60s eval active=%d transitions=%d, want 1/1", out.Active, out.Transitions)
	}
}

func TestEvaluateDatasourceErrorHoldsState(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	now := time.Unix(1000, 0).UTC()
	okDS := &fakeDS{res: vec(models.Sample{Labels: map[string]string{"i": "a"}, Value: 1})}
	rule := models.Rule{ID: "r1", PromQL: "x", ForS: 0}
	evalWith(t, st, okDS, 1000, now).EvaluateRule(ctx, rule, models.Datasource{}) // FIRING

	errDS := &fakeDS{err: errors.New("datasource HTTP 401: unauthorized")}
	out := evalWith(t, st, errDS, 1000, now.Add(time.Minute)).EvaluateRule(ctx, rule, models.Datasource{})
	if out.Err == nil {
		t.Fatal("expected error outcome")
	}
	if out.FailReason != "auth" {
		t.Errorf("FailReason = %q, want auth", out.FailReason)
	}
	// State must be held — instance still firing, no resolve transition.
	active, _ := st.ListInstancesByRule(ctx, "r1")
	if len(active) != 1 || active[0].State != models.StateFiring {
		t.Errorf("state not held on failure: %+v", active)
	}
	hist, _ := st.History(ctx, storage.HistoryFilter{})
	if len(hist) != 1 {
		t.Errorf("history = %d, want 1 (no transition on failure)", len(hist))
	}
}

func TestEvaluateInstanceCap(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	now := time.Unix(1000, 0).UTC()
	ds := &fakeDS{res: vec(
		models.Sample{Labels: map[string]string{"i": "a"}, Value: 1},
		models.Sample{Labels: map[string]string{"i": "b"}, Value: 1},
		models.Sample{Labels: map[string]string{"i": "c"}, Value: 1},
	)}
	rule := models.Rule{ID: "r1", PromQL: "x", ForS: 0}
	out := evalWith(t, st, ds, 2, now).EvaluateRule(ctx, rule, models.Datasource{})
	if out.FailReason != "instance_cap" {
		t.Errorf("FailReason = %q, want instance_cap", out.FailReason)
	}
	active, _ := st.ListInstancesByRule(ctx, "r1")
	if len(active) != 2 {
		t.Errorf("active = %d, want capped at 2", len(active))
	}
}

func TestEvaluateScalarResult(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	now := time.Unix(1000, 0).UTC()
	ds := &fakeDS{res: models.Result{Kind: models.KindScalar, Samples: []models.Sample{{Value: 42}}}}
	rule := models.Rule{ID: "r1", PromQL: "vector(1)>0", ForS: 0}
	out := evalWith(t, st, ds, 1000, now).EvaluateRule(ctx, rule, models.Datasource{})
	if out.Active != 1 {
		t.Fatalf("scalar active = %d, want 1", out.Active)
	}
	active, _ := st.ListInstancesByRule(ctx, "r1")
	if len(active) != 1 || active[0].CurrentValue != 42 {
		t.Errorf("scalar instance = %+v", active)
	}
}
