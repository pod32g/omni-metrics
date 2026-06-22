package scheduler_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pod32g/omni-metrics/internal/alerts/models"
	"github.com/pod32g/omni-metrics/internal/alerts/scheduler"
)

type counters struct {
	mu sync.Mutex
	m  map[string]*int64
}

func newCounters() *counters { return &counters{m: map[string]*int64{}} }

func (c *counters) inc(id string) {
	c.mu.Lock()
	p := c.m[id]
	if p == nil {
		var n int64
		p = &n
		c.m[id] = p
	}
	c.mu.Unlock()
	atomic.AddInt64(p, 1)
}

func (c *counters) get(id string) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if p := c.m[id]; p != nil {
		return atomic.LoadInt64(p)
	}
	return 0
}

// rule encodes its tick interval in EvalIntervalS, which the test's interval
// function interprets as milliseconds for fast, deterministic tests.
func rule(id string, intervalMS int) models.Rule {
	return models.Rule{ID: id, EvalIntervalS: intervalMS, Enabled: true}
}

// msInterval reads EvalIntervalS as milliseconds (test-only fast clock).
func msInterval(r models.Rule) time.Duration {
	return time.Duration(r.EvalIntervalS) * time.Millisecond
}

func newSched(eval func(context.Context, string)) *scheduler.Scheduler {
	return scheduler.New(eval, msInterval)
}

func TestSchedulerTicksEnabledRules(t *testing.T) {
	c := newCounters()
	s := newSched(func(_ context.Context, id string) { c.inc(id) })
	s.Start(context.Background())
	defer s.Stop()

	s.Reconcile([]models.Rule{rule("a", 15), rule("b", 15)})
	time.Sleep(120 * time.Millisecond)
	if c.get("a") < 3 || c.get("b") < 3 {
		t.Fatalf("expected several ticks each, got a=%d b=%d", c.get("a"), c.get("b"))
	}
}

func TestSchedulerReconcileStopsRemoved(t *testing.T) {
	c := newCounters()
	s := newSched(func(_ context.Context, id string) { c.inc(id) })
	s.Start(context.Background())
	defer s.Stop()

	s.Reconcile([]models.Rule{rule("a", 15)})
	time.Sleep(80 * time.Millisecond)
	s.Reconcile(nil) // remove all
	time.Sleep(20 * time.Millisecond)
	stopped := c.get("a")
	time.Sleep(80 * time.Millisecond)
	if c.get("a") != stopped {
		t.Errorf("rule kept ticking after removal: %d -> %d", stopped, c.get("a"))
	}
}

func TestSchedulerDisabledRuleNotScheduled(t *testing.T) {
	c := newCounters()
	s := newSched(func(_ context.Context, id string) { c.inc(id) })
	s.Start(context.Background())
	defer s.Stop()
	r := rule("a", 15)
	r.Enabled = false
	s.Reconcile([]models.Rule{r})
	time.Sleep(80 * time.Millisecond)
	if c.get("a") != 0 {
		t.Errorf("disabled rule ticked %d times", c.get("a"))
	}
}

func TestSchedulerSlowRuleDoesNotBlockOthers(t *testing.T) {
	c := newCounters()
	s := newSched(func(ctx context.Context, id string) {
		if id == "slow" {
			select {
			case <-time.After(500 * time.Millisecond):
			case <-ctx.Done():
			}
		}
		c.inc(id)
	})
	s.Start(context.Background())
	defer s.Stop()

	s.Reconcile([]models.Rule{rule("slow", 15), rule("fast", 15)})
	time.Sleep(150 * time.Millisecond)
	if c.get("fast") < 3 {
		t.Errorf("fast rule starved by slow rule: %d ticks", c.get("fast"))
	}
}

func TestSchedulerRestartAfterStop(t *testing.T) {
	c := newCounters()
	s := newSched(func(_ context.Context, id string) { c.inc(id) })
	s.Start(context.Background())
	s.Reconcile([]models.Rule{rule("a", 15)})
	time.Sleep(40 * time.Millisecond)
	s.Stop()

	// A Reconcile after Stop (even without an explicit Start) must re-arm
	// evaluation against a fresh context, not stay silently dead on the
	// already-cancelled root.
	s.Reconcile([]models.Rule{rule("a", 15)})
	before := c.get("a")
	time.Sleep(80 * time.Millisecond)
	if c.get("a") <= before {
		t.Errorf("scheduler did not resume after Stop+Start: %d -> %d", before, c.get("a"))
	}
	s.Stop()
}

func TestSchedulerStopIsPromptAndClean(t *testing.T) {
	c := newCounters()
	s := newSched(func(_ context.Context, id string) { c.inc(id) })
	s.Start(context.Background())
	s.Reconcile([]models.Rule{rule("a", 10), rule("b", 10)})
	time.Sleep(40 * time.Millisecond)

	done := make(chan struct{})
	go func() { s.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return promptly")
	}
}
