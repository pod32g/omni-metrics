// Package metrics is the alert engine's own instrumentation, rendered in the
// Prometheus text exposition format. It is registered as a sub-collector of the
// server's /metrics handler so the engine stays decoupled from the core
// SelfMetrics.
package metrics

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Metrics holds the alert engine counters and gauges. All methods are safe for
// concurrent use.
type Metrics struct {
	mu sync.Mutex

	rules   int
	active  int
	pending int

	evals       map[string]int64 // by result (success|error)
	failures    map[string]int64 // by reason
	transitions int64

	durSum      float64 // seconds
	durCount    int64
	lastDurSecs float64
}

// New builds an empty collector.
func New() *Metrics {
	return &Metrics{evals: map[string]int64{}, failures: map[string]int64{}}
}

// SetRules records the current number of configured rules.
func (m *Metrics) SetRules(n int) { m.mu.Lock(); m.rules = n; m.mu.Unlock() }

// SetActive records the current number of firing instances.
func (m *Metrics) SetActive(n int) { m.mu.Lock(); m.active = n; m.mu.Unlock() }

// SetPending records the current number of pending instances.
func (m *Metrics) SetPending(n int) { m.mu.Lock(); m.pending = n; m.mu.Unlock() }

// IncEval counts an evaluation by result ("success" or "error").
func (m *Metrics) IncEval(result string) {
	m.mu.Lock()
	m.evals[result]++
	m.mu.Unlock()
}

// IncFailure counts an evaluation failure by reason.
func (m *Metrics) IncFailure(reason string) {
	m.mu.Lock()
	m.failures[reason]++
	m.mu.Unlock()
}

// IncTransition counts a persisted state transition.
func (m *Metrics) IncTransition() { m.mu.Lock(); m.transitions++; m.mu.Unlock() }

// ObserveDuration records an evaluation's wall-clock duration.
func (m *Metrics) ObserveDuration(d time.Duration) {
	secs := d.Seconds()
	m.mu.Lock()
	m.durSum += secs
	m.durCount++
	m.lastDurSecs = secs
	m.mu.Unlock()
}

// WriteExposition renders all metrics in the Prometheus text format.
func (m *Metrics) WriteExposition(w io.Writer) {
	m.mu.Lock()
	defer m.mu.Unlock()

	gauge(w, "omni_alert_rules_total", "Number of configured alert rules.", float64(m.rules))
	gauge(w, "omni_alerts_active", "Number of firing alert instances.", float64(m.active))
	gauge(w, "omni_alerts_pending", "Number of pending alert instances.", float64(m.pending))

	fmt.Fprintf(w, "# HELP omni_alert_evaluations_total Total rule evaluations by result.\n")
	fmt.Fprintf(w, "# TYPE omni_alert_evaluations_total counter\n")
	for _, k := range sortedKeys(m.evals) {
		fmt.Fprintf(w, "omni_alert_evaluations_total{result=%q} %d\n", k, m.evals[k])
	}

	fmt.Fprintf(w, "# HELP omni_alert_evaluation_failures_total Total evaluation failures by reason.\n")
	fmt.Fprintf(w, "# TYPE omni_alert_evaluation_failures_total counter\n")
	for _, k := range sortedKeys(m.failures) {
		fmt.Fprintf(w, "omni_alert_evaluation_failures_total{reason=%q} %d\n", k, m.failures[k])
	}

	fmt.Fprintf(w, "# HELP omni_alert_state_transitions_total Total alert state transitions.\n")
	fmt.Fprintf(w, "# TYPE omni_alert_state_transitions_total counter\n")
	fmt.Fprintf(w, "omni_alert_state_transitions_total %d\n", m.transitions)

	// Emit _sum and _count as independent counter families, each with its own
	// HELP/TYPE referencing the exact series name (strict parsers reject HELP/TYPE
	// whose name does not match an emitted series).
	fmt.Fprintf(w, "# HELP omni_alert_evaluation_duration_seconds_sum Cumulative evaluation duration in seconds.\n")
	fmt.Fprintf(w, "# TYPE omni_alert_evaluation_duration_seconds_sum counter\n")
	fmt.Fprintf(w, "omni_alert_evaluation_duration_seconds_sum %s\n", f(m.durSum))
	fmt.Fprintf(w, "# HELP omni_alert_evaluation_duration_seconds_count Number of evaluations measured.\n")
	fmt.Fprintf(w, "# TYPE omni_alert_evaluation_duration_seconds_count counter\n")
	fmt.Fprintf(w, "omni_alert_evaluation_duration_seconds_count %d\n", m.durCount)

	gauge(w, "omni_alert_scheduler_duration_seconds", "Duration of the most recent rule evaluation.", m.lastDurSecs)
}

func gauge(w io.Writer, name, help string, v float64) {
	fmt.Fprintf(w, "# HELP %s %s\n", name, help)
	fmt.Fprintf(w, "# TYPE %s gauge\n", name)
	fmt.Fprintf(w, "%s %s\n", name, f(v))
}

func f(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }

func sortedKeys(m map[string]int64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
