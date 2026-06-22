// Package scheduler runs one goroutine per enabled alert rule, each ticking at
// the rule's own interval. Independent goroutines mean a slow datasource only
// stalls its own rule, never the others. Reconcile diffs the desired rule set
// against the running goroutines; Stop cancels everything and waits.
package scheduler

import (
	"context"
	"sync"
	"time"

	"github.com/pod32g/omni-metrics/internal/alerts/models"
)

// IntervalFunc derives a rule's tick interval. Production uses seconds; tests
// inject a faster clock.
type IntervalFunc func(models.Rule) time.Duration

// DefaultInterval reads EvalIntervalS as seconds.
func DefaultInterval(r models.Rule) time.Duration {
	return time.Duration(r.EvalIntervalS) * time.Second
}

// Scheduler manages per-rule evaluation goroutines.
type Scheduler struct {
	evalFn   func(ctx context.Context, ruleID string)
	interval IntervalFunc

	mu      sync.Mutex
	ctx     context.Context
	cancel  context.CancelFunc
	running map[string]*task
	wg      sync.WaitGroup
}

type task struct {
	cancel   context.CancelFunc
	interval time.Duration
}

// New builds a scheduler that invokes evalFn for each due rule. interval may be
// nil (defaults to seconds).
func New(evalFn func(ctx context.Context, ruleID string), interval IntervalFunc) *Scheduler {
	if interval == nil {
		interval = DefaultInterval
	}
	return &Scheduler{evalFn: evalFn, interval: interval, running: map[string]*task{}}
}

// Start establishes the root context all rule goroutines derive from. Call it
// once before Reconcile.
func (s *Scheduler) Start(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ctx, s.cancel = context.WithCancel(ctx)
}

// Reconcile makes the running goroutines match the desired enabled rule set:
// it stops goroutines for removed/disabled/interval-changed rules and starts
// goroutines for newly enabled ones.
func (s *Scheduler) Reconcile(rules []models.Rule) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx == nil { // Reconcile before Start: root from Background.
		s.ctx, s.cancel = context.WithCancel(context.Background())
	}

	desired := make(map[string]models.Rule, len(rules))
	for _, r := range rules {
		if r.Enabled && s.interval(r) > 0 {
			desired[r.ID] = r
		}
	}

	// Stop goroutines that are gone or whose interval changed.
	for id, tk := range s.running {
		r, ok := desired[id]
		if !ok || s.interval(r) != tk.interval {
			tk.cancel()
			delete(s.running, id)
		}
	}

	// Start goroutines for newly desired rules.
	for id, r := range desired {
		if _, ok := s.running[id]; ok {
			continue
		}
		interval := s.interval(r)
		tctx, tcancel := context.WithCancel(s.ctx)
		s.running[id] = &task{cancel: tcancel, interval: interval}
		s.wg.Add(1)
		go s.loop(tctx, id, interval)
	}
}

func (s *Scheduler) loop(ctx context.Context, id string, interval time.Duration) {
	defer s.wg.Done()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.evalFn(ctx, id)
		}
	}
}

// Stop cancels all rule goroutines and waits for them to exit.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	if s.cancel != nil {
		s.cancel()
	}
	s.running = map[string]*task{}
	s.mu.Unlock()
	s.wg.Wait()
}
