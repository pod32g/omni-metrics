// Package alerts assembles the alerting engine: it owns the store, builds the
// evaluator and per-rule scheduler, exposes the REST handler and the metrics
// collector, and seeds datasources from configuration. It is the single seam
// the rest of omni wires against — cmd/omni constructs a Service, mounts its
// Handler and Collector, and runs its scheduler.
package alerts

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/pod32g/omni-metrics/internal/alerts/api"
	"github.com/pod32g/omni-metrics/internal/alerts/datasource"
	"github.com/pod32g/omni-metrics/internal/alerts/evaluator"
	"github.com/pod32g/omni-metrics/internal/alerts/metrics"
	"github.com/pod32g/omni-metrics/internal/alerts/models"
	"github.com/pod32g/omni-metrics/internal/alerts/scheduler"
	"github.com/pod32g/omni-metrics/internal/alerts/storage"
)

// Options configures a Service.
type Options struct {
	// StorePath is the SQLite path (":memory:" for ephemeral).
	StorePath string
	// Datasources are the config- and builtin-sourced datasources to seed. Their
	// Source field must be set; the ID is used as-is and must be stable across
	// boots (rules reference it).
	Datasources []models.Datasource
	// DefaultDatasource is the name of the datasource applied to rules that omit
	// one.
	DefaultDatasource string
	// MaxInstances bounds active instances per rule (0 = default).
	MaxInstances int
	// Now is the clock (nil = time.Now).
	Now func() time.Time
	// Interval derives a rule's tick interval (nil = seconds).
	Interval scheduler.IntervalFunc
	// Logger receives structured engine logs (nil = the standard logger).
	Logger *log.Logger
}

// Service is the running alerting engine.
type Service struct {
	store       storage.Store
	eval        *evaluator.Evaluator
	sched       *scheduler.Scheduler
	metrics     *metrics.Metrics
	handler     http.Handler
	now         func() time.Time
	logger      *log.Logger
	defaultDSID string
}

// NewService opens the store, seeds datasources, and builds the engine.
func NewService(opts Options) (*Service, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = log.Default()
	}
	store, err := storage.OpenSQLite(opts.StorePath)
	if err != nil {
		return nil, err
	}
	s := &Service{
		store:   store,
		metrics: metrics.New(),
		now:     opts.Now,
		logger:  opts.Logger,
	}
	s.eval = evaluator.New(store, func(ds models.Datasource) datasource.Datasource {
		return datasource.New(ds)
	}, opts.MaxInstances, opts.Now)

	if err := s.seedDatasources(context.Background(), opts.Datasources, opts.DefaultDatasource); err != nil {
		store.Close()
		return nil, err
	}

	s.sched = scheduler.New(func(ctx context.Context, ruleID string) {
		_, _ = s.evaluate(ctx, ruleID) // evaluate logs/meters its own failures
	}, opts.Interval)

	s.handler = api.New(api.Deps{
		Store:               store,
		Evaluate:            s.evaluate,
		EvaluateAll:         s.evaluateAll,
		TestDatasource:      s.testDatasource,
		OnRulesChanged:      s.reconcile,
		Now:                 opts.Now,
		DefaultDatasourceID: s.defaultDSID,
	})
	return s, nil
}

// Handler returns the alerting REST handler to mount under /api/v1/alerts and
// /api/v1/datasources.
func (s *Service) Handler() http.Handler { return s.handler }

// Collector returns a function that writes the engine's metrics in the
// Prometheus text format — registered as a /metrics sub-collector.
func (s *Service) Collector() func(io.Writer) {
	return func(w io.Writer) {
		s.refreshGauges(context.Background())
		s.metrics.WriteExposition(w)
	}
}

// Start loads rules, schedules the enabled ones, and begins evaluating.
func (s *Service) Start(ctx context.Context) {
	s.sched.Start(ctx)
	s.reconcile()
	s.refreshGauges(ctx)
}

// Stop halts the scheduler and closes the store.
func (s *Service) Stop() {
	if s.sched != nil {
		s.sched.Stop()
	}
	if s.store != nil {
		s.store.Close()
	}
}

// seedDatasources upserts the config- and builtin-sourced datasources and
// resolves the default datasource name to its id.
func (s *Service) seedDatasources(ctx context.Context, dss []models.Datasource, defaultName string) error {
	now := s.now()
	for _, d := range dss {
		if d.CreatedAt.IsZero() {
			d.CreatedAt = now
		}
		d.UpdatedAt = now
		if err := s.store.PutDatasource(ctx, d); err != nil {
			return fmt.Errorf("seeding datasource %q: %w", d.Name, err)
		}
		if d.Name == defaultName {
			s.defaultDSID = d.ID
		}
	}
	// Fall back to the first seeded datasource if the named default was absent.
	if s.defaultDSID == "" && len(dss) > 0 {
		s.defaultDSID = dss[0].ID
	}
	return nil
}

// evaluate runs one rule and updates metrics/logs. It returns the summary for
// the synchronous API path.
func (s *Service) evaluate(ctx context.Context, ruleID string) (api.EvalResult, error) {
	start := s.now()
	rule, err := s.store.GetRule(ctx, ruleID)
	if err != nil {
		return api.EvalResult{}, err
	}
	dsCfg, err := s.store.GetDatasource(ctx, rule.DatasourceID)
	if err != nil {
		s.metrics.IncEval("error")
		s.metrics.IncFailure("datasource")
		s.logf("alert_eval rule=%q error=%q reason=datasource", rule.Name, err.Error())
		if errors.Is(err, storage.ErrNotFound) {
			return api.EvalResult{}, fmt.Errorf("rule %q references unknown datasource %q", rule.Name, rule.DatasourceID)
		}
		return api.EvalResult{}, err
	}
	if !dsCfg.Enabled {
		s.metrics.IncEval("error")
		s.metrics.IncFailure("datasource_disabled")
		return api.EvalResult{}, fmt.Errorf("datasource %q is disabled", dsCfg.Name)
	}

	out := s.eval.EvaluateRule(ctx, rule, dsCfg)
	s.metrics.ObserveDuration(s.now().Sub(start))

	if out.Err != nil {
		s.metrics.IncEval("error")
		s.metrics.IncFailure(out.FailReason)
		s.logf("alert_eval rule=%q error=%q reason=%s", rule.Name, out.Err.Error(), out.FailReason)
		return api.EvalResult{}, out.Err
	}
	s.metrics.IncEval("success")
	if out.FailReason != "" { // e.g. instance_cap tripped on an otherwise-OK eval
		s.metrics.IncFailure(out.FailReason)
		s.logf("alert_eval rule=%q reason=%s", rule.Name, out.FailReason)
	}
	for i := 0; i < out.Transitions; i++ {
		s.metrics.IncTransition()
	}
	if out.Transitions > 0 {
		s.logf("alert_state rule=%q transitions=%d active=%d pending=%d", rule.Name, out.Transitions, out.Active, out.Pending)
	}
	s.refreshGauges(ctx)
	return api.EvalResult{Active: out.Active, Pending: out.Pending, Transitions: out.Transitions}, nil
}

// evaluateAll evaluates every enabled rule synchronously.
func (s *Service) evaluateAll(ctx context.Context) (int, error) {
	rules, err := s.store.ListRules(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		_, _ = s.evaluate(ctx, r.ID)
		n++
	}
	return n, nil
}

// testDatasource runs a trivial query to verify connectivity.
func (s *Service) testDatasource(ctx context.Context, ds models.Datasource) error {
	_, err := datasource.New(ds).Query(ctx, "vector(1)", s.now())
	return err
}

// reconcile reloads rules and updates the scheduler.
func (s *Service) reconcile() {
	rules, err := s.store.ListRules(context.Background())
	if err != nil {
		s.logf("alert_scheduler reconcile_error=%q", err.Error())
		return
	}
	s.sched.Reconcile(rules)
	s.refreshGauges(context.Background())
}

// refreshGauges recomputes the rule/active/pending gauges from the store.
func (s *Service) refreshGauges(ctx context.Context) {
	if rules, err := s.store.ListRules(ctx); err == nil {
		s.metrics.SetRules(len(rules))
	}
	active, err := s.store.ListActiveInstances(ctx)
	if err != nil {
		return
	}
	firing, pending := 0, 0
	for _, in := range active {
		switch in.State {
		case models.StateFiring:
			firing++
		case models.StatePending:
			pending++
		}
	}
	s.metrics.SetActive(firing)
	s.metrics.SetPending(pending)
}

func (s *Service) logf(format string, args ...interface{}) {
	if s.logger != nil {
		s.logger.Printf(format, args...)
	}
}
