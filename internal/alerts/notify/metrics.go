package notify

import (
	"fmt"
	"io"
	"sort"
	"sync"
)

// metrics holds the notifier's counters and queue gauge, rendered in the
// Prometheus text exposition format and folded into the alert engine's
// /metrics. All methods are safe for concurrent use.
type metrics struct {
	mu         sync.Mutex
	sent       int64
	failed     map[string]int64 // by reason: permanent|giveup|canceled
	dropped    map[string]int64 // by reason: queue_full
	filtered   int64
	retries    int64
	queueDepth int
}

func newMetrics() *metrics {
	return &metrics{failed: map[string]int64{}, dropped: map[string]int64{}}
}

func (m *metrics) incSent() { m.mu.Lock(); m.sent++; m.mu.Unlock() }

func (m *metrics) incFailed(reason string) {
	m.mu.Lock()
	m.failed[reason]++
	m.mu.Unlock()
}

func (m *metrics) incDropped(reason string) {
	m.mu.Lock()
	m.dropped[reason]++
	m.mu.Unlock()
}

func (m *metrics) incFiltered() { m.mu.Lock(); m.filtered++; m.mu.Unlock() }

func (m *metrics) incRetry() { m.mu.Lock(); m.retries++; m.mu.Unlock() }

func (m *metrics) setQueueDepth(n int) { m.mu.Lock(); m.queueDepth = n; m.mu.Unlock() }

// WriteExposition renders the notifier metrics. Each counter family emits its
// HELP/TYPE; labelled families iterate sorted reasons.
func (m *metrics) WriteExposition(w io.Writer) {
	m.mu.Lock()
	defer m.mu.Unlock()

	fmt.Fprintf(w, "# HELP omni_alerts_notify_sent_total Notifications delivered to omni-notify.\n")
	fmt.Fprintf(w, "# TYPE omni_alerts_notify_sent_total counter\n")
	fmt.Fprintf(w, "omni_alerts_notify_sent_total %d\n", m.sent)

	fmt.Fprintf(w, "# HELP omni_alerts_notify_failed_total Notifications that failed to deliver, by reason.\n")
	fmt.Fprintf(w, "# TYPE omni_alerts_notify_failed_total counter\n")
	for _, k := range sortedInt64Keys(m.failed) {
		fmt.Fprintf(w, "omni_alerts_notify_failed_total{reason=%q} %d\n", k, m.failed[k])
	}

	fmt.Fprintf(w, "# HELP omni_alerts_notify_dropped_total Notifications dropped before sending, by reason.\n")
	fmt.Fprintf(w, "# TYPE omni_alerts_notify_dropped_total counter\n")
	for _, k := range sortedInt64Keys(m.dropped) {
		fmt.Fprintf(w, "omni_alerts_notify_dropped_total{reason=%q} %d\n", k, m.dropped[k])
	}

	fmt.Fprintf(w, "# HELP omni_alerts_notify_filtered_total Notifications skipped for being below min_severity.\n")
	fmt.Fprintf(w, "# TYPE omni_alerts_notify_filtered_total counter\n")
	fmt.Fprintf(w, "omni_alerts_notify_filtered_total %d\n", m.filtered)

	fmt.Fprintf(w, "# HELP omni_alerts_notify_retries_total Retry attempts made while delivering notifications.\n")
	fmt.Fprintf(w, "# TYPE omni_alerts_notify_retries_total counter\n")
	fmt.Fprintf(w, "omni_alerts_notify_retries_total %d\n", m.retries)

	fmt.Fprintf(w, "# HELP omni_alerts_notify_queue_depth Notifications currently buffered for sending.\n")
	fmt.Fprintf(w, "# TYPE omni_alerts_notify_queue_depth gauge\n")
	fmt.Fprintf(w, "omni_alerts_notify_queue_depth %d\n", m.queueDepth)
}

func sortedInt64Keys(m map[string]int64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
