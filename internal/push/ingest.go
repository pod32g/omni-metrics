package push

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/pod32g/omni-metrics/internal/tsdb"
)

// Appendable is the storage dependency: a source of appenders (satisfied by *tsdb.DB).
type Appendable interface {
	Appender() tsdb.Appender
}

// ErrTooManySeries re-exports the storage cardinality-cap sentinel so callers
// (e.g. the API) can classify a push rejection without importing tsdb.
var ErrTooManySeries = tsdb.ErrTooManySeries

// ErrorKind classifies an ingest failure for HTTP mapping.
type ErrorKind int

const (
	ErrBadData  ErrorKind = iota // client error -> 400
	ErrInternal                  // server/capacity error -> 503
)

// IngestError is a classified ingest failure. It may wrap an underlying error
// (e.g. tsdb.ErrTooManySeries) so callers can errors.Is/As on it.
type IngestError struct {
	Kind ErrorKind
	Msg  string
	Err  error
}

func (e *IngestError) Error() string { return e.Msg }
func (e *IngestError) Unwrap() error { return e.Err }

// Result reports what a successful push wrote.
type Result struct {
	SeriesTouched   int
	SamplesAppended int
}

// Source is a per-pusher health snapshot for /api/v1/push/sources.
type Source struct {
	Job          string    `json:"job"`
	Instance     string    `json:"instance"`
	LastPush     time.Time `json:"lastPush"`
	LastError    string    `json:"lastError"`
	PushesTotal  int64     `json:"pushesTotal"`
	SamplesTotal int64     `json:"samplesTotal"`
	LastSamples  int       `json:"lastSamples"`
}

// Ingester validates and appends push requests and tracks per-source health.
type Ingester struct {
	app         Appendable
	sampleLimit int // max samples per request; 0 = unlimited

	mu      sync.Mutex
	sources map[string]*Source
}

// NewIngester builds an Ingester over storage. sampleLimit caps samples per push.
func NewIngester(app Appendable, sampleLimit int) *Ingester {
	return &Ingester{app: app, sampleLimit: sampleLimit, sources: map[string]*Source{}}
}

// Ingest validates the request, appends it atomically, records health, and
// returns counts or a classified *IngestError. remoteHost is the default
// instance; nowMs stamps timestamp-less samples and the health LastPush.
func (i *Ingester) Ingest(req *Request, remoteHost string, nowMs int64) (Result, error) {
	if err := req.validate(); err != nil {
		// Deliberately not recorded in the per-source health registry: a malformed
		// request often has no stable identity (e.g. empty job), and recording
		// every validation failure would let an attacker grow the sources map with
		// junk job/instance keys. Aggregate validation failures remain visible via
		// the omni_push_requests_total{status="error"} counter.
		return Result{}, &IngestError{Kind: ErrBadData, Msg: err.Error()}
	}
	instance := req.Instance
	if instance == "" {
		instance = remoteHost
	}

	total := 0
	for j := range req.Series {
		if req.Series[j].Value != nil {
			total++
		} else {
			total += len(req.Series[j].Samples)
		}
	}
	if i.sampleLimit > 0 && total > i.sampleLimit {
		ie := &IngestError{Kind: ErrBadData, Msg: fmt.Sprintf("sample limit exceeded: %d > %d", total, i.sampleLimit)}
		i.recordFailure(req.Job, instance, nowMs, ie)
		return Result{}, ie
	}

	app := i.app.Appender()
	appended := 0
	for j := range req.Series {
		labels := buildLabels(req.Series[j], req.Job, instance)
		for _, p := range req.Series[j].samplePoints(nowMs) {
			if _, err := app.Append(labels, p.TimestampMs, float64(p.Value)); err != nil {
				_ = app.Rollback()
				ie := &IngestError{Kind: ErrInternal, Msg: fmt.Sprintf("append %s: %v", labels.String(), err), Err: err}
				i.recordFailure(req.Job, instance, nowMs, ie)
				return Result{}, ie
			}
			appended++
		}
	}
	if err := app.Commit(); err != nil {
		ie := &IngestError{Kind: ErrInternal, Msg: "commit: " + err.Error(), Err: err}
		i.recordFailure(req.Job, instance, nowMs, ie)
		return Result{}, ie
	}

	i.recordSuccess(req.Job, instance, nowMs, appended)
	return Result{SeriesTouched: len(req.Series), SamplesAppended: appended}, nil
}

// Sources returns a health snapshot, sorted by job then instance.
func (i *Ingester) Sources() []Source {
	i.mu.Lock()
	defer i.mu.Unlock()
	out := make([]Source, 0, len(i.sources))
	for _, s := range i.sources {
		out = append(out, *s)
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Job != out[b].Job {
			return out[a].Job < out[b].Job
		}
		return out[a].Instance < out[b].Instance
	})
	return out
}

func (i *Ingester) sourceLocked(job, instance string) *Source {
	key := job + "\x00" + instance
	s := i.sources[key]
	if s == nil {
		s = &Source{Job: job, Instance: instance}
		i.sources[key] = s
	}
	return s
}

func (i *Ingester) recordSuccess(job, instance string, nowMs int64, samples int) {
	i.mu.Lock()
	defer i.mu.Unlock()
	s := i.sourceLocked(job, instance)
	s.LastPush = time.UnixMilli(nowMs)
	s.LastError = ""
	s.PushesTotal++
	s.SamplesTotal += int64(samples)
	s.LastSamples = samples
}

func (i *Ingester) recordFailure(job, instance string, nowMs int64, err error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	s := i.sourceLocked(job, instance)
	s.LastPush = time.UnixMilli(nowMs)
	s.LastError = err.Error()
	s.PushesTotal++
}
