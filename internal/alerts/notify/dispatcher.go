package notify

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"
)

// sender delivers one notification. *Client satisfies it; tests inject fakes.
type sender interface {
	Send(ctx context.Context, n Notification) error
}

// graceDrainDefault bounds how long Stop waits to flush the buffer before it
// cancels in-flight sends and gives up (best-effort delivery).
const graceDrainDefault = 2 * time.Second

// Dispatcher forwards Notifications to omni-notify off the evaluation path. A
// single background worker drains a bounded buffer, applies the min-severity
// filter, and sends with bounded retry. Enqueue never blocks: a full buffer
// drops the notification (and meters it). A nil *Dispatcher is a no-op.
type Dispatcher struct {
	cfg     Config
	sender  sender
	metrics *metrics
	ch      chan Notification
	backoff func(attempt int) time.Duration

	graceDrain time.Duration

	mu      sync.Mutex
	stopped bool
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// NewDispatcher builds a Dispatcher around s. Defaults are applied to cfg.
func NewDispatcher(cfg Config, s sender) *Dispatcher {
	cfg = cfg.withDefaults()
	return &Dispatcher{
		cfg:        cfg,
		sender:     s,
		metrics:    newMetrics(),
		ch:         make(chan Notification, cfg.QueueSize),
		backoff:    expBackoff,
		graceDrain: graceDrainDefault,
	}
}

// expBackoff is the default retry backoff: 200ms doubling, capped at 5s.
func expBackoff(attempt int) time.Duration {
	d := 200 * time.Millisecond << attempt
	if d > 5*time.Second || d <= 0 {
		return 5 * time.Second
	}
	return d
}

// Enqueue offers n to the send buffer without blocking. If the buffer is full
// the notification is dropped and metered. Calls after Stop are ignored.
func (d *Dispatcher) Enqueue(n Notification) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped {
		return
	}
	select {
	case d.ch <- n:
		d.metrics.setQueueDepth(len(d.ch))
	default:
		d.metrics.incDropped("queue_full")
	}
}

// Start launches the background worker. It is idempotent.
func (d *Dispatcher) Start(ctx context.Context) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cancel != nil {
		return
	}
	d.ctx, d.cancel = context.WithCancel(ctx)
	d.wg.Add(1)
	go d.worker()
}

// Stop stops accepting, drains the buffer best-effort within the grace window,
// then cancels in-flight sends and waits for the worker to exit. It is
// idempotent and safe on a never-started dispatcher.
func (d *Dispatcher) Stop() {
	if d == nil {
		return
	}
	d.mu.Lock()
	if d.stopped {
		d.mu.Unlock()
		return
	}
	d.stopped = true
	close(d.ch)
	cancel, started := d.cancel, d.cancel != nil
	d.mu.Unlock()

	if !started {
		return // worker never ran; buffered items are discarded
	}
	done := make(chan struct{})
	go func() { d.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(d.graceDrain):
		cancel() // force in-flight Send/backoff to abort
		<-done
	}
}

// WriteExposition renders the notifier metrics in the Prometheus text format.
func (d *Dispatcher) WriteExposition(w io.Writer) {
	if d == nil {
		return
	}
	d.metrics.WriteExposition(w)
}

func (d *Dispatcher) worker() {
	defer d.wg.Done()
	for n := range d.ch {
		d.process(n)
	}
}

func (d *Dispatcher) process(n Notification) {
	d.metrics.setQueueDepth(len(d.ch))
	n.Severity = MapSeverity(n.Severity)
	if !meetsMin(d.cfg.MinSeverity, n.Severity) {
		d.metrics.incFiltered()
		return
	}
	d.sendWithRetry(n)
}

// sendWithRetry attempts delivery up to MaxRetries+1 times. Permanent failures
// are not retried; retryable failures back off (respecting context cancel).
func (d *Dispatcher) sendWithRetry(n Notification) {
	for attempt := 0; ; attempt++ {
		err := d.sender.Send(d.ctx, n)
		if err == nil {
			d.metrics.incSent()
			return
		}
		if errors.Is(err, ErrPermanent) {
			d.metrics.incFailed("permanent")
			return
		}
		if d.ctx.Err() != nil {
			d.metrics.incFailed("canceled")
			return
		}
		if attempt >= d.cfg.MaxRetries {
			d.metrics.incFailed("giveup")
			return
		}
		d.metrics.incRetry()
		select {
		case <-d.ctx.Done():
			d.metrics.incFailed("canceled")
			return
		case <-time.After(d.backoff(attempt)):
		}
	}
}
