// Package evaluator runs a single alert rule against its datasource, advances
// the per-series state machine, and persists the resulting instances and state
// transitions. It is deliberately independent of the scheduler so it can be
// driven both on a timer and synchronously from the API.
package evaluator

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/pod32g/omni-metrics/internal/alerts/datasource"
	"github.com/pod32g/omni-metrics/internal/alerts/models"
	"github.com/pod32g/omni-metrics/internal/alerts/state"
	"github.com/pod32g/omni-metrics/internal/alerts/storage"
)

// DefaultMaxInstances bounds the active alert instances a single rule may track,
// guarding against a high-cardinality result exploding the instance table.
const DefaultMaxInstances = 1000

// Evaluator evaluates rules and persists their state.
type Evaluator struct {
	store        storage.Store
	resolve      func(models.Datasource) datasource.Datasource
	maxInstances int
	now          func() time.Time
}

// New builds an Evaluator. resolve maps a datasource config to a live client
// (injected so the scheduler can cache clients and tests can fake them); now is
// the clock (injected for deterministic tests).
func New(store storage.Store, resolve func(models.Datasource) datasource.Datasource, maxInstances int, now func() time.Time) *Evaluator {
	if maxInstances <= 0 {
		maxInstances = DefaultMaxInstances
	}
	if now == nil {
		now = time.Now
	}
	return &Evaluator{store: store, resolve: resolve, maxInstances: maxInstances, now: now}
}

// Outcome summarizes a single evaluation.
type Outcome struct {
	Active      int          // instances now FIRING
	Pending     int          // instances now PENDING
	Transitions int          // state transitions persisted this evaluation
	Changes     []Transition // the transitions persisted this evaluation (len == Transitions)
	Err         error        // non-nil if the query failed (state was held, not resolved)
	FailReason  string       // classification when Err != nil or the instance cap tripped
}

// Transition is a single persisted state change surfaced to the caller so it can
// forward firing/resolved transitions to a notifier without re-reading the
// store. It carries the rule context and the instance's merged labels and
// annotations (which are gone from the store once a resolved instance is
// deleted).
type Transition struct {
	RuleID      string
	RuleName    string
	Severity    string
	Fingerprint string
	Prev        models.State
	New         models.State
	Value       float64
	Labels      map[string]string
	Annotations map[string]string
	Time        time.Time
}

// EvaluateRule queries the rule's datasource, advances state for each result
// series, resolves series that disappeared, and persists transitions.
func (e *Evaluator) EvaluateRule(ctx context.Context, rule models.Rule, dsCfg models.Datasource) Outcome {
	now := e.now()
	forD := time.Duration(rule.ForS) * time.Second

	res, err := e.resolve(dsCfg).Query(ctx, rule.PromQL, now)
	if err != nil {
		// A query/HTTP/auth/timeout failure must not resolve alerts: hold prior
		// state and surface the failure to the caller (metrics + logs).
		return Outcome{Err: err, FailReason: classify(err)}
	}

	existing, err := e.store.ListInstancesByRule(ctx, rule.ID)
	if err != nil {
		return Outcome{Err: err, FailReason: "storage"}
	}
	byFP := make(map[string]models.Instance, len(existing))
	for _, in := range existing {
		byFP[in.Fingerprint] = in
	}

	present := indexSamples(res.Samples)
	tracked := len(existing)
	var out Outcome

	for _, fp := range sortedKeys(present) {
		sample := present[fp]
		cur, ok := byFP[fp]
		curState := models.StateOK
		activeAt := now
		started := now
		id := uuid.NewString()
		if ok {
			curState = cur.State
			activeAt = cur.ActiveAt
			started = cur.StartedAt
			id = cur.ID
		} else if tracked >= e.maxInstances {
			// Refuse new instances beyond the cap; existing ones still tick.
			out.FailReason = "instance_cap"
			continue
		}

		next, changed := state.Next(curState, true, activeAt, now, forD)
		in := models.Instance{
			ID:           id,
			RuleID:       rule.ID,
			Fingerprint:  fp,
			State:        next,
			StateName:    next.String(),
			CurrentValue: sample.Value,
			ActiveAt:     activeAt,
			StartedAt:    started,
			UpdatedAt:    now,
			Labels:       mergeLabels(sample.Labels, rule.Labels),
			Annotations:  rule.Annotations,
		}
		if err := e.store.UpsertInstance(ctx, in); err != nil {
			return Outcome{Err: err, FailReason: "storage"}
		}
		if !ok {
			tracked++
		}
		if changed {
			e.record(ctx, rule.ID, fp, curState, next, now, sample.Value, transitionReason(curState, next))
			out.Transitions++
			out.Changes = append(out.Changes, Transition{
				RuleID:      rule.ID,
				RuleName:    rule.Name,
				Severity:    string(rule.Severity),
				Fingerprint: fp,
				Prev:        curState,
				New:         next,
				Value:       sample.Value,
				Labels:      in.Labels,
				Annotations: in.Annotations,
				Time:        now,
			})
		}
		switch next {
		case models.StateFiring:
			out.Active++
		case models.StatePending:
			out.Pending++
		}
	}

	// Resolve instances whose series disappeared this evaluation.
	for _, in := range existing {
		if _, stillPresent := present[in.Fingerprint]; stillPresent {
			continue
		}
		next, changed := state.Next(in.State, false, in.ActiveAt, now, forD)
		if changed {
			e.record(ctx, rule.ID, in.Fingerprint, in.State, next, now, 0, "condition no longer true")
			out.Transitions++
			out.Changes = append(out.Changes, Transition{
				RuleID:      rule.ID,
				RuleName:    rule.Name,
				Severity:    string(rule.Severity),
				Fingerprint: in.Fingerprint,
				Prev:        in.State,
				New:         next,
				Value:       0,
				Labels:      in.Labels,
				Annotations: in.Annotations,
				Time:        now,
			})
		}
		// A resolved instance leaves the active set; its transition lives in history.
		if err := e.store.DeleteInstance(ctx, in.ID); err != nil {
			return Outcome{Err: err, FailReason: "storage"}
		}
	}

	return out
}

func (e *Evaluator) record(ctx context.Context, ruleID, fp string, prev, next models.State, ts time.Time, value float64, reason string) {
	_, _ = e.store.AppendHistory(ctx, models.HistoryEntry{
		RuleID:      ruleID,
		Fingerprint: fp,
		Prev:        prev,
		New:         next,
		PrevName:    prev.String(),
		NewName:     next.String(),
		Timestamp:   ts,
		Value:       value,
		Reason:      reason,
	})
}

// indexSamples maps each result sample to its label fingerprint. When two
// samples share a fingerprint (identical labels) the last one wins.
func indexSamples(samples []models.Sample) map[string]models.Sample {
	out := make(map[string]models.Sample, len(samples))
	for _, s := range samples {
		out[models.Fingerprint(s.Labels)] = s
	}
	return out
}

func sortedKeys(m map[string]models.Sample) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// mergeLabels combines series labels with the rule's labels; rule labels win on
// conflict (Prometheus alerting semantics).
func mergeLabels(series, ruleLabels map[string]string) map[string]string {
	if len(series) == 0 && len(ruleLabels) == 0 {
		return nil
	}
	out := make(map[string]string, len(series)+len(ruleLabels))
	for k, v := range series {
		out[k] = v
	}
	for k, v := range ruleLabels {
		out[k] = v
	}
	return out
}

func transitionReason(prev, next models.State) string {
	switch {
	case next == models.StateFiring && prev == models.StatePending:
		return "for duration elapsed"
	case next == models.StateFiring:
		return "condition is true"
	case next == models.StatePending:
		return "condition is true; waiting for 'for' duration"
	case next == models.StateResolved:
		return "condition no longer true"
	default:
		return ""
	}
}

// classify maps a query error to a failure reason for metrics and logs.
func classify(err error) string {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "timeout"
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "Timeout") || strings.Contains(msg, "deadline exceeded") || strings.Contains(msg, "timeout"):
		return "timeout"
	case strings.Contains(msg, "HTTP 401") || strings.Contains(msg, "HTTP 403"):
		return "auth"
	case strings.Contains(msg, "HTTP "):
		return "http"
	case strings.Contains(msg, "query error"):
		return "query"
	case strings.Contains(msg, "decoding") || strings.Contains(msg, "parsing") || strings.Contains(msg, "unsupported result"):
		return "invalid"
	default:
		return "query"
	}
}
