package metrics_test

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pod32g/omni-metrics/internal/alerts/metrics"
)

func render(m *metrics.Metrics) string {
	var b bytes.Buffer
	m.WriteExposition(&b)
	return b.String()
}

func TestMetricsExposition(t *testing.T) {
	m := metrics.New()
	m.SetRules(3)
	m.SetActive(2)
	m.SetPending(1)
	m.IncEval("success")
	m.IncEval("success")
	m.IncEval("error")
	m.IncFailure("timeout")
	m.IncFailure("auth")
	m.IncTransition()
	m.ObserveDuration(250 * time.Millisecond)

	out := render(m)
	want := []string{
		"# TYPE omni_alert_rules_total gauge",
		"omni_alert_rules_total 3",
		"omni_alerts_active 2",
		"omni_alerts_pending 1",
		`omni_alert_evaluations_total{result="success"} 2`,
		`omni_alert_evaluations_total{result="error"} 1`,
		`omni_alert_evaluation_failures_total{reason="timeout"} 1`,
		`omni_alert_evaluation_failures_total{reason="auth"} 1`,
		"omni_alert_state_transitions_total 1",
		"omni_alert_evaluation_duration_seconds_count 1",
		"omni_alert_scheduler_duration_seconds 0.25",
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("exposition missing %q\n--- got ---\n%s", w, out)
		}
	}
	// _sum should reflect the observed 0.25s.
	if !strings.Contains(out, "omni_alert_evaluation_duration_seconds_sum 0.25") {
		t.Errorf("missing duration sum 0.25\n%s", out)
	}
	// HELP/TYPE names must match the emitted series exactly (strict-parser safe).
	for _, w := range []string{
		"# HELP omni_alert_evaluation_duration_seconds_sum",
		"# TYPE omni_alert_evaluation_duration_seconds_sum counter",
		"# HELP omni_alert_evaluation_duration_seconds_count",
		"# TYPE omni_alert_evaluation_duration_seconds_count counter",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("exposition missing %q\n%s", w, out)
		}
	}
	// The bare (series-less) duration name must NOT get a stray HELP/TYPE.
	if strings.Contains(out, "TYPE omni_alert_evaluation_duration_seconds counter") {
		t.Errorf("stray TYPE for series-less duration name\n%s", out)
	}
}

func TestMetricsConcurrent(t *testing.T) {
	m := metrics.New()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.IncEval("success")
			m.IncFailure("query")
			m.IncTransition()
			m.ObserveDuration(time.Millisecond)
			m.SetActive(1)
		}()
	}
	wg.Wait()
	if !strings.Contains(render(m), `omni_alert_evaluations_total{result="success"} 50`) {
		t.Error("concurrent increments lost")
	}
}
