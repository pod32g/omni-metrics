package storage_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/pod32g/omni-metrics/internal/alerts/models"
	"github.com/pod32g/omni-metrics/internal/alerts/storage"
)

// TestSQLiteConcurrentAccess mimics the per-rule scheduler hammering the store
// from many goroutines at once. It must not deadlock or error.
func TestSQLiteConcurrentAccess(t *testing.T) {
	for _, dsn := range []string{":memory:", t.TempDir() + "/stress.db"} {
		t.Run(dsn, func(t *testing.T) {
			s, err := storage.OpenSQLite(dsn)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer s.Close()
			ctx := context.Background()

			done := make(chan struct{})
			go func() {
				var wg sync.WaitGroup
				for g := 0; g < 12; g++ {
					wg.Add(1)
					go func(g int) {
						defer wg.Done()
						for i := 0; i < 30; i++ {
							fp := fmt.Sprintf("g%d-i%d", g, i%5)
							_ = s.UpsertInstance(ctx, models.Instance{
								ID: fmt.Sprintf("g%d-%d", g, i%5), RuleID: fmt.Sprintf("r%d", g),
								Fingerprint: fp, State: models.StateFiring, StateName: "firing",
								ActiveAt: time.Unix(1, 0), StartedAt: time.Unix(1, 0), UpdatedAt: time.Unix(1, 0),
							})
							_, _ = s.ListActiveInstances(ctx)
							_, _ = s.AppendHistory(ctx, models.HistoryEntry{RuleID: fmt.Sprintf("r%d", g), Fingerprint: fp, PrevName: "ok", NewName: "firing", Timestamp: time.Unix(1, 0)})
							_, _ = s.History(ctx, storage.HistoryFilter{Limit: 10})
						}
					}(g)
				}
				wg.Wait()
				close(done)
			}()

			select {
			case <-done:
			case <-time.After(20 * time.Second):
				t.Fatal("concurrent access deadlocked / timed out")
			}
		})
	}
}
