package notify

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSender records calls and returns a scripted error per call (nil once the
// script is exhausted).
type fakeSender struct {
	mu    sync.Mutex
	calls []Notification
	errs  []error
}

func (f *fakeSender) Send(_ context.Context, n Notification) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := len(f.calls)
	f.calls = append(f.calls, n)
	if i < len(f.errs) {
		return f.errs[i]
	}
	return nil
}

func (f *fakeSender) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeSender) call(i int) Notification {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[i]
}

// newTestDispatcher builds a dispatcher with instant backoff for fast tests.
func newTestDispatcher(cfg Config, s sender) *Dispatcher {
	d := NewDispatcher(cfg, s)
	d.backoff = func(int) time.Duration { return 0 }
	return d
}

func exposition(d *Dispatcher) string {
	var b strings.Builder
	d.WriteExposition(&b)
	return b.String()
}

func TestDispatcherDeliversFiringAndResolved(t *testing.T) {
	f := &fakeSender{}
	d := newTestDispatcher(Config{}, f)
	d.Start(context.Background())
	d.Enqueue(Notification{RuleID: "r", Fingerprint: "x", Status: "firing", Severity: "critical"})
	d.Enqueue(Notification{RuleID: "r", Fingerprint: "x", Status: "resolved", Severity: "critical"})
	d.Stop()

	if f.count() != 2 {
		t.Fatalf("sent %d, want 2", f.count())
	}
	if got := f.call(0).Severity; got != "critical" {
		t.Errorf("severity = %q, want canonical critical", got)
	}
	if !strings.Contains(exposition(d), "omni_alerts_notify_sent_total 2") {
		t.Errorf("want sent_total 2 in:\n%s", exposition(d))
	}
}

func TestDispatcherRetriesThenSucceeds(t *testing.T) {
	f := &fakeSender{errs: []error{errors.New("temporary")}}
	d := newTestDispatcher(Config{MaxRetries: 3}, f)
	d.Start(context.Background())
	d.Enqueue(Notification{Status: "firing", Severity: "warning"})
	d.Stop()

	if f.count() != 2 {
		t.Fatalf("sent %d times, want 2 (1 fail + 1 success)", f.count())
	}
	out := exposition(d)
	if !strings.Contains(out, "omni_alerts_notify_sent_total 1") {
		t.Errorf("want sent_total 1 in:\n%s", out)
	}
	if !strings.Contains(out, "omni_alerts_notify_retries_total 1") {
		t.Errorf("want retries_total 1 in:\n%s", out)
	}
}

func TestDispatcherGivesUpAfterMaxRetries(t *testing.T) {
	f := &fakeSender{errs: []error{
		errors.New("e1"), errors.New("e2"), errors.New("e3"), errors.New("e4"),
	}}
	d := newTestDispatcher(Config{MaxRetries: 2}, f)
	d.Start(context.Background())
	d.Enqueue(Notification{Status: "firing", Severity: "error"})
	d.Stop()

	if f.count() != 3 {
		t.Fatalf("sent %d times, want 3 (1 + 2 retries)", f.count())
	}
	out := exposition(d)
	if !strings.Contains(out, "omni_alerts_notify_retries_total 2") {
		t.Errorf("want retries_total 2 in:\n%s", out)
	}
	if !strings.Contains(out, `omni_alerts_notify_failed_total{reason="giveup"} 1`) {
		t.Errorf("want failed giveup 1 in:\n%s", out)
	}
}

func TestDispatcherPermanentNotRetried(t *testing.T) {
	f := &fakeSender{errs: []error{fmt.Errorf("%w: 401", ErrPermanent)}}
	d := newTestDispatcher(Config{MaxRetries: 3}, f)
	d.Start(context.Background())
	d.Enqueue(Notification{Status: "firing", Severity: "critical"})
	d.Stop()

	if f.count() != 1 {
		t.Fatalf("sent %d times, want 1 (no retry on permanent)", f.count())
	}
	out := exposition(d)
	if !strings.Contains(out, `omni_alerts_notify_failed_total{reason="permanent"} 1`) {
		t.Errorf("want failed permanent 1 in:\n%s", out)
	}
	if !strings.Contains(out, "omni_alerts_notify_retries_total 0") {
		t.Errorf("want retries_total 0 in:\n%s", out)
	}
}

func TestDispatcherFiltersBelowMinSeverity(t *testing.T) {
	f := &fakeSender{}
	d := newTestDispatcher(Config{MinSeverity: "warning"}, f)
	d.Start(context.Background())
	d.Enqueue(Notification{Status: "firing", Severity: "info"})     // below -> filtered
	d.Enqueue(Notification{Status: "firing", Severity: "critical"}) // above -> sent
	d.Stop()

	if f.count() != 1 {
		t.Fatalf("sent %d, want 1 (info filtered out)", f.count())
	}
	out := exposition(d)
	if !strings.Contains(out, "omni_alerts_notify_filtered_total 1") {
		t.Errorf("want filtered_total 1 in:\n%s", out)
	}
	if !strings.Contains(out, "omni_alerts_notify_sent_total 1") {
		t.Errorf("want sent_total 1 in:\n%s", out)
	}
}

func TestDispatcherDropsWhenQueueFull(t *testing.T) {
	f := &fakeSender{}
	// Do not Start: with no worker draining, the buffer fills and overflow drops.
	d := newTestDispatcher(Config{QueueSize: 2}, f)
	for i := 0; i < 5; i++ {
		d.Enqueue(Notification{Status: "firing", Severity: "warning"})
	}
	if !strings.Contains(exposition(d), `omni_alerts_notify_dropped_total{reason="queue_full"} 3`) {
		t.Errorf("want dropped 3 in:\n%s", exposition(d))
	}
}

func TestDispatcherEnqueueAfterStopIsNoop(t *testing.T) {
	f := &fakeSender{}
	d := newTestDispatcher(Config{}, f)
	d.Start(context.Background())
	d.Stop()
	d.Enqueue(Notification{Status: "firing", Severity: "critical"}) // must not panic or send
	if f.count() != 0 {
		t.Fatalf("sent %d after stop, want 0", f.count())
	}
}
