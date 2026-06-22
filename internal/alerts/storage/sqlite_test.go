package storage_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/pod32g/omni-metrics/internal/alerts/models"
	"github.com/pod32g/omni-metrics/internal/alerts/storage"
	"github.com/pod32g/omni-metrics/internal/alerts/storage/storagetest"
)

func TestSQLiteConformance(t *testing.T) {
	storagetest.RunConformance(t, func() storage.Store {
		s, err := storage.OpenSQLite(":memory:")
		if err != nil {
			t.Fatalf("OpenSQLite: %v", err)
		}
		return s
	})
}

func TestSQLitePersistsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "alerts.db")
	now := time.Unix(1_700_000_000, 0).UTC()

	s, err := storage.OpenSQLite(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := s.PutRule(ctx, models.Rule{ID: "r1", Name: "n", PromQL: "up==0", EvalIntervalS: 15, Enabled: true, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("PutRule: %v", err)
	}
	if err := s.UpsertInstance(ctx, models.Instance{ID: "i1", RuleID: "r1", Fingerprint: "fp", State: models.StateFiring, StateName: "firing", ActiveAt: now, StartedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertInstance: %v", err)
	}
	if _, err := s.AppendHistory(ctx, models.HistoryEntry{RuleID: "r1", Fingerprint: "fp", PrevName: "ok", NewName: "firing", Timestamp: now}); err != nil {
		t.Fatalf("AppendHistory: %v", err)
	}
	s.Close()

	s2, err := storage.OpenSQLite(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	if _, err := s2.GetRule(ctx, "r1"); err != nil {
		t.Errorf("rule lost after reopen: %v", err)
	}
	active, _ := s2.ListActiveInstances(ctx)
	if len(active) != 1 || active[0].State != models.StateFiring {
		t.Errorf("active instance lost after reopen: %+v", active)
	}
	hist, _ := s2.History(ctx, storage.HistoryFilter{})
	if len(hist) != 1 {
		t.Errorf("history lost after reopen: %d", len(hist))
	}
}
