package notify

import (
	"strings"
	"testing"
)

func TestNotifyMetricsExposition(t *testing.T) {
	m := newMetrics()
	m.incSent()
	m.incSent()
	m.incFailed("giveup")
	m.incFailed("permanent")
	m.incDropped("queue_full")
	m.incFiltered()
	m.incRetry()
	m.incRetry()
	m.setQueueDepth(3)

	var b strings.Builder
	m.WriteExposition(&b)
	out := b.String()

	want := []string{
		"# TYPE omni_alerts_notify_sent_total counter",
		"omni_alerts_notify_sent_total 2",
		"# TYPE omni_alerts_notify_failed_total counter",
		`omni_alerts_notify_failed_total{reason="giveup"} 1`,
		`omni_alerts_notify_failed_total{reason="permanent"} 1`,
		`omni_alerts_notify_dropped_total{reason="queue_full"} 1`,
		"omni_alerts_notify_filtered_total 1",
		"omni_alerts_notify_retries_total 2",
		"# TYPE omni_alerts_notify_queue_depth gauge",
		"omni_alerts_notify_queue_depth 3",
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("exposition missing %q\n--- got ---\n%s", w, out)
		}
	}
}
