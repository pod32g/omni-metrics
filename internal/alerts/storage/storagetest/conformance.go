// Package storagetest holds the reusable conformance suite for any
// storage.Store implementation. Keeping it out of the storage package itself
// (as a separate, test-only-imported package) avoids pulling "testing" into the
// production storage build — mirroring the tsdb/tsdbtest convention.
package storagetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pod32g/omni-metrics/internal/alerts/models"
	"github.com/pod32g/omni-metrics/internal/alerts/storage"
)

// RunConformance exercises the Store contract against a freshly-opened, empty
// store produced by newStore. It is shared so any backend can be verified.
func RunConformance(t *testing.T, newStore func() storage.Store) {
	t.Helper()
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0).UTC()

	t.Run("datasource CRUD", func(t *testing.T) {
		s := newStore()
		defer s.Close()
		d := models.Datasource{ID: "ds1", Name: "local", Type: "prometheus", BaseURL: "http://x", AuthType: models.AuthBearer, Credentials: "tok", Headers: map[string]string{"H": "v"}, TimeoutMS: 1000, Enabled: true, Source: models.SourceConfig, CreatedAt: now, UpdatedAt: now}
		if err := s.PutDatasource(ctx, d); err != nil {
			t.Fatalf("PutDatasource: %v", err)
		}
		got, err := s.GetDatasource(ctx, "ds1")
		if err != nil {
			t.Fatalf("GetDatasource: %v", err)
		}
		if got.Name != "local" || got.Credentials != "tok" || got.Headers["H"] != "v" || got.Source != models.SourceConfig {
			t.Errorf("roundtrip mismatch: %+v", got)
		}
		byName, err := s.GetDatasourceByName(ctx, "local")
		if err != nil || byName.ID != "ds1" {
			t.Fatalf("GetDatasourceByName: %+v %v", byName, err)
		}
		// Update in place.
		d.BaseURL = "http://y"
		if err := s.PutDatasource(ctx, d); err != nil {
			t.Fatalf("update: %v", err)
		}
		got, _ = s.GetDatasource(ctx, "ds1")
		if got.BaseURL != "http://y" {
			t.Errorf("update not applied: %q", got.BaseURL)
		}
		list, _ := s.ListDatasources(ctx)
		if len(list) != 1 {
			t.Fatalf("list = %d, want 1", len(list))
		}
		if err := s.DeleteDatasource(ctx, "ds1"); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if _, err := s.GetDatasource(ctx, "ds1"); !errors.Is(err, storage.ErrNotFound) {
			t.Errorf("after delete err = %v, want storage.ErrNotFound", err)
		}
	})

	t.Run("datasource unique name", func(t *testing.T) {
		s := newStore()
		defer s.Close()
		_ = s.PutDatasource(ctx, models.Datasource{ID: "a", Name: "dup", Source: models.SourceAPI})
		err := s.PutDatasource(ctx, models.Datasource{ID: "b", Name: "dup", Source: models.SourceAPI})
		if err == nil {
			t.Fatal("expected unique-name violation")
		}
	})

	t.Run("rule CRUD", func(t *testing.T) {
		s := newStore()
		defer s.Close()
		r := models.Rule{ID: "r1", Name: "High errors", DatasourceID: "ds1", PromQL: "up == 0", EvalIntervalS: 15, ForS: 60, Severity: "critical", Labels: map[string]string{"team": "x"}, Annotations: map[string]string{"summary": "boom"}, Enabled: true, CreatedAt: now, UpdatedAt: now}
		if err := s.PutRule(ctx, r); err != nil {
			t.Fatalf("PutRule: %v", err)
		}
		got, err := s.GetRule(ctx, "r1")
		if err != nil {
			t.Fatalf("GetRule: %v", err)
		}
		if got.PromQL != "up == 0" || got.ForS != 60 || got.Labels["team"] != "x" || !got.Enabled {
			t.Errorf("rule roundtrip: %+v", got)
		}
		r.Enabled = false
		_ = s.PutRule(ctx, r)
		got, _ = s.GetRule(ctx, "r1")
		if got.Enabled {
			t.Error("disable not persisted")
		}
		if list, _ := s.ListRules(ctx); len(list) != 1 {
			t.Fatalf("ListRules = %d", len(list))
		}
		if err := s.DeleteRule(ctx, "r1"); err != nil {
			t.Fatalf("DeleteRule: %v", err)
		}
		if _, err := s.GetRule(ctx, "r1"); !errors.Is(err, storage.ErrNotFound) {
			t.Errorf("after delete: %v", err)
		}
	})

	t.Run("instance upsert and list", func(t *testing.T) {
		s := newStore()
		defer s.Close()
		in := models.Instance{ID: "i1", RuleID: "r1", Fingerprint: "fp1", State: models.StateFiring, StateName: "firing", CurrentValue: 7, ActiveAt: now, StartedAt: now, UpdatedAt: now, Labels: map[string]string{"instance": "a"}}
		if err := s.UpsertInstance(ctx, in); err != nil {
			t.Fatalf("UpsertInstance: %v", err)
		}
		// Upsert again with new value — same id, no duplicate.
		in.CurrentValue = 9
		if err := s.UpsertInstance(ctx, in); err != nil {
			t.Fatalf("UpsertInstance update: %v", err)
		}
		active, _ := s.ListActiveInstances(ctx)
		if len(active) != 1 || active[0].CurrentValue != 9 || active[0].State != models.StateFiring {
			t.Fatalf("active = %+v", active)
		}
		byRule, _ := s.ListInstancesByRule(ctx, "r1")
		if len(byRule) != 1 {
			t.Fatalf("byRule = %d", len(byRule))
		}
		if err := s.DeleteInstance(ctx, "i1"); err != nil {
			t.Fatalf("DeleteInstance: %v", err)
		}
		if active, _ = s.ListActiveInstances(ctx); len(active) != 0 {
			t.Fatalf("after delete active = %d", len(active))
		}
	})

	t.Run("history append cursor and filter", func(t *testing.T) {
		s := newStore()
		defer s.Close()
		id1, err := s.AppendHistory(ctx, models.HistoryEntry{RuleID: "r1", Fingerprint: "fp1", Prev: models.StateOK, New: models.StateFiring, PrevName: "ok", NewName: "firing", Timestamp: now, Value: 5, Reason: "condition true"})
		if err != nil {
			t.Fatalf("AppendHistory: %v", err)
		}
		id2, _ := s.AppendHistory(ctx, models.HistoryEntry{RuleID: "r2", Fingerprint: "fp2", Prev: models.StateFiring, New: models.StateResolved, PrevName: "firing", NewName: "resolved", Timestamp: now, Value: 0, Reason: "no longer true"})
		if id2 <= id1 {
			t.Fatalf("cursor not monotonic: %d then %d", id1, id2)
		}
		all, _ := s.History(ctx, storage.HistoryFilter{})
		if len(all) != 2 {
			t.Fatalf("history all = %d", len(all))
		}
		r1only, _ := s.History(ctx, storage.HistoryFilter{RuleID: "r1"})
		if len(r1only) != 1 || r1only[0].RuleID != "r1" {
			t.Fatalf("filtered = %+v", r1only)
		}
		since, _ := s.History(ctx, storage.HistoryFilter{Since: id1})
		if len(since) != 1 || since[0].ID != id2 {
			t.Fatalf("since = %+v", since)
		}
		ev, _ := s.Events(ctx, 0, 10)
		if len(ev) != 2 || ev[0].ID != id1 || ev[1].ID != id2 {
			t.Fatalf("events order = %+v", ev)
		}
		evSince, _ := s.Events(ctx, id1, 10)
		if len(evSince) != 1 || evSince[0].ID != id2 {
			t.Fatalf("events since = %+v", evSince)
		}
	})
}
