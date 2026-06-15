// Package tsdb is omni-metrics' time-series storage: an in-memory head block
// holding recent samples, backed by an append-only write-ahead log for
// durability and crash recovery. The Storage, Appender, and Querier interfaces
// keep the engine backend-agnostic; the included conformance suite (see
// tsdbtest) pins the contract any implementation must satisfy.
package tsdb

import (
	"errors"
	"time"

	"github.com/pod32g/omni-metrics/internal/model"
)

// ErrTooManySeries is returned by an appender when creating a new series would
// exceed the head's configured cardinality cap.
var ErrTooManySeries = errors.New("tsdb: per-head series limit exceeded")

// Appender stages samples and applies them atomically on Commit. Within a batch,
// the same series may be appended to many times.
type Appender interface {
	Append(l model.Labels, t int64, v float64) (ref uint64, err error)
	Commit() error
	Rollback() error
}

// Series is a single result series: its labels and the samples within the
// queried range (ascending by time).
type Series interface {
	Labels() model.Labels
	Samples() []model.Sample
}

// SeriesSet is a forward iterator over query results.
type SeriesSet interface {
	Next() bool
	At() Series
	Err() error
}

// Querier reads from storage at a point in time.
type Querier interface {
	Select(mint, maxt int64, matchers ...model.Matcher) SeriesSet
	LabelValues(name string, matchers ...model.Matcher) []string
	LabelNames(matchers ...model.Matcher) []string
}

// Storage is the top-level engine: a source of appenders and queriers.
type Storage interface {
	Appender() Appender
	Querier() Querier
	Close() error
}

// Options configures a DB.
type Options struct {
	Dir       string        // WAL/data directory; empty = in-memory only (no durability)
	Retention time.Duration // head retention window; 0 = keep everything until truncated
	MaxSeries int           // head cardinality cap; 0 = unlimited
}

// DB is the default Storage implementation.
type DB struct {
	head *head
	wal  *WAL // nil when running in-memory
}

// Open creates a DB. When opts.Dir is set, the WAL there is replayed to rebuild
// the head before new writes are accepted. If replay encounters corruption, Open
// returns a non-nil DB (with everything recovered up to the corruption) together
// with a non-nil error describing what was lost — callers should log it and may
// still use the DB.
func Open(opts Options) (*DB, error) {
	retMs := int64(0)
	if opts.Retention > 0 {
		retMs = opts.Retention.Milliseconds()
	}
	db := &DB{head: newHead(retMs, opts.MaxSeries)}
	if opts.Dir == "" {
		return db, nil
	}
	replayErr := replayWAL(opts.Dir, db.head)
	w, err := openWAL(opts.Dir)
	if err != nil {
		return nil, err
	}
	db.wal = w
	return db, replayErr
}

// Truncate drops samples older than minT (milliseconds) from the head. The cmd
// layer calls this periodically with now-retention to enforce the window.
func (db *DB) Truncate(minT int64) { db.head.truncate(minT) }

// HeadSeries reports the number of series currently in the head (for /metrics).
func (db *DB) HeadSeries() int {
	db.head.mu.RLock()
	defer db.head.mu.RUnlock()
	return len(db.head.series)
}

func (db *DB) Appender() Appender { return &appender{db: db} }
func (db *DB) Querier() Querier   { return &querier{db: db} }

func (db *DB) Close() error {
	if db.wal != nil {
		return db.wal.close()
	}
	return nil
}

// appender stages new series and samples, writing them to the WAL and applying
// them to the head on Commit.
type appender struct {
	db        *DB
	newSeries []seriesRec
	pending   []refSample
}

type seriesRec struct {
	ref uint64
	l   model.Labels
}

func (a *appender) Append(l model.Labels, t int64, v float64) (uint64, error) {
	ref, created, err := a.db.head.acquire(l)
	if err != nil {
		return 0, err
	}
	if created {
		a.newSeries = append(a.newSeries, seriesRec{ref: ref, l: l})
	}
	a.pending = append(a.pending, refSample{ref: ref, t: t, v: v})
	return ref, nil
}

func (a *appender) Commit() error {
	if a.db.wal != nil {
		for _, s := range a.newSeries {
			if err := a.db.wal.logSeries(s.ref, s.l); err != nil {
				return err
			}
		}
		if len(a.pending) > 0 {
			if err := a.db.wal.logSamples(a.pending); err != nil {
				return err
			}
		}
		if err := a.db.wal.sync(); err != nil {
			return err
		}
	}
	for _, s := range a.pending {
		a.db.head.appendSample(s.ref, s.t, s.v)
	}
	a.newSeries = nil
	a.pending = nil
	return nil
}

// Rollback discards staged samples and reclaims any series this batch created
// that still hold no samples, releasing the cardinality-cap slots they took. This
// guards the cardinality-DoS class: an over-cap (hence rolled-back) append must
// not permanently burn the head's series budget. A created series that another
// committed batch has since populated is left intact (it is no longer empty).
func (a *appender) Rollback() error {
	for _, s := range a.newSeries {
		a.db.head.reclaimIfEmpty(s.ref)
	}
	a.newSeries = nil
	a.pending = nil
	return nil
}

type querier struct{ db *DB }

func (q *querier) Select(mint, maxt int64, matchers ...model.Matcher) SeriesSet {
	return &seriesSet{items: q.db.head.query(mint, maxt, matchers)}
}

func (q *querier) LabelValues(name string, matchers ...model.Matcher) []string {
	return q.db.head.labelValues(name, matchers)
}

func (q *querier) LabelNames(matchers ...model.Matcher) []string {
	return q.db.head.labelNames(matchers)
}

type seriesSet struct {
	items []seriesResult
	i     int
}

func (s *seriesSet) Next() bool { s.i++; return s.i <= len(s.items) }
func (s *seriesSet) At() Series {
	it := s.items[s.i-1]
	return &series{labels: it.labels, samples: it.samples}
}
func (s *seriesSet) Err() error { return nil }

type series struct {
	labels  model.Labels
	samples []model.Sample
}

func (s *series) Labels() model.Labels    { return s.labels }
func (s *series) Samples() []model.Sample { return s.samples }
