# Push Write Model Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a JSON push-ingestion endpoint (`POST /api/v1/push`) so a process with no HTTP server can emit metrics by POSTing samples that append into the existing TSDB as real time series.

**Architecture:** A new `internal/push` package decodes/validates a JSON request and appends through the existing `tsdb.Appender` (push = inverted scrape; no storage changes), keeping a per-source health registry. A thin `internal/api` handler adds auth, body limits, error mapping, self-metrics, and a `/api/v1/push/sources` health endpoint. The web console gains a "Pushers" view mirroring the Targets page. No new third-party dependencies.

**Tech Stack:** Go 1.25 stdlib (`net/http`, `encoding/json`, `crypto/subtle`), existing `internal/{model,tsdb}` packages, vanilla embedded HTML/CSS/JS.

---

## Spec

Implements [docs/superpowers/specs/2026-06-15-push-write-model-design.md](../specs/2026-06-15-push-write-model-design.md).

## File Structure

- **Create** `internal/push/value.go` — `Value` type with number-or-string JSON decoding (NaN/±Inf).
- **Create** `internal/push/value_test.go`
- **Create** `internal/push/push.go` — `Request`, `SeriesInput`, `SamplePoint`, decode, validation, label building, name validators.
- **Create** `internal/push/push_test.go`
- **Create** `internal/push/ingest.go` — `Ingester`, `Source`, `Result`, `IngestError`, `Ingest`, `Sources`, health registry.
- **Create** `internal/push/ingest_test.go`
- **Modify** `internal/config/config.go` — add `PushConfig`, `Config.Push`, accessors.
- **Modify** `internal/config/config_test.go`
- **Modify** `internal/api/selfmetrics.go` — push counters + exposition lines.
- **Modify** `internal/api/selfmetrics_test.go` (create if absent) — assert new metrics.
- **Create** `internal/api/push.go` — `Pusher`/`PushSourcesProvider` interfaces, `PushConfig`, `handlePush`, `handlePushSources`, auth.
- **Modify** `internal/api/api.go` — `Options` fields + route registration.
- **Create** `internal/api/push_test.go`
- **Modify** `cmd/omni/main.go` — construct `push.Ingester`, wire into `api.Options`.
- **Modify** `web/assets/index.html` — Pushers nav link.
- **Modify** `web/assets/app.js` — router case + `renderPushers`.
- **Modify** `web/web_test.go` — assert the new view ships.
- **Modify** `examples/omni.yml` and `README.md` — document the push block + endpoint.

---

## Task 1: `push.Value` — number-or-string JSON decoding

**Files:**
- Create: `internal/push/value.go`
- Test: `internal/push/value_test.go`

- [ ] **Step 1: Write the failing test**

```go
package push

import (
	"encoding/json"
	"math"
	"testing"
)

func TestValueUnmarshal(t *testing.T) {
	cases := []struct {
		in   string
		want float64
		nan  bool
		err  bool
	}{
		{in: `42`, want: 42},
		{in: `-1.5`, want: -1.5},
		{in: `0`, want: 0},
		{in: `"NaN"`, nan: true},
		{in: `"+Inf"`, want: math.Inf(1)},
		{in: `"Inf"`, want: math.Inf(1)},
		{in: `"-Inf"`, want: math.Inf(-1)},
		{in: `"banana"`, err: true},
		{in: `true`, err: true},
		{in: `{}`, err: true},
	}
	for _, tc := range cases {
		var v Value
		err := json.Unmarshal([]byte(tc.in), &v)
		if tc.err {
			if err == nil {
				t.Errorf("%s: expected error, got %v", tc.in, float64(v))
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: unexpected error %v", tc.in, err)
			continue
		}
		if tc.nan {
			if !math.IsNaN(float64(v)) {
				t.Errorf("%s: want NaN, got %v", tc.in, float64(v))
			}
			continue
		}
		if float64(v) != tc.want {
			t.Errorf("%s: got %v, want %v", tc.in, float64(v), tc.want)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/push/ -run TestValueUnmarshal`
Expected: FAIL — package/type `Value` undefined.

- [ ] **Step 3: Write minimal implementation**

```go
// Package push implements JSON push ingestion: a process with no HTTP server can
// POST samples that append into storage as time series (the inverse of scrape).
package push

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
)

// Value is a float64 that decodes from either a JSON number or one of the
// strings "NaN", "+Inf"/"Inf", or "-Inf" — JSON has no native non-finite floats.
type Value float64

func (v *Value) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return fmt.Errorf("value: %w", err)
		}
		switch s {
		case "NaN":
			*v = Value(math.NaN())
		case "+Inf", "Inf":
			*v = Value(math.Inf(1))
		case "-Inf":
			*v = Value(math.Inf(-1))
		default:
			return fmt.Errorf("value: invalid number string %q", s)
		}
		return nil
	}
	var f float64
	if err := json.Unmarshal(b, &f); err != nil {
		return fmt.Errorf("value: expected number or string, got %s", b)
	}
	*v = Value(f)
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/push/ -run TestValueUnmarshal`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/push/
git add internal/push/value.go internal/push/value_test.go
git -c user.name=pod32g -c user.email=3311662+pod32g@users.noreply.github.com commit -m "feat(push): Value decodes JSON number or NaN/Inf strings"
```

---

## Task 2: Request schema, validation, and label building

**Files:**
- Create: `internal/push/push.go`
- Test: `internal/push/push_test.go`

Validation rules (from the spec): `job` non-empty; ≥1 series; each series has a valid metric name and exactly one of `value`/`samples` (non-empty if `samples`); label names match `^[a-zA-Z_][a-zA-Z0-9_]*$`; the reserved labels `__name__`/`job`/`instance` in `labels` are dropped (server overrides them); any *other* `__`-prefixed label name is rejected.

- [ ] **Step 1: Write the failing test**

```go
package push

import (
	"testing"

	"github.com/pod32g/omni-metrics/internal/model"
)

func TestRequestValidate(t *testing.T) {
	v := func(f float64) *Value { x := Value(f); return &x }
	cases := []struct {
		name string
		req  Request
		ok   bool
	}{
		{name: "ok value", req: Request{Job: "j", Series: []SeriesInput{{Name: "m", Value: v(1)}}}, ok: true},
		{name: "ok samples", req: Request{Job: "j", Series: []SeriesInput{{Name: "m", Samples: []SamplePoint{{Value: v(1)}}}}}, ok: true},
		{name: "empty job", req: Request{Series: []SeriesInput{{Name: "m", Value: v(1)}}}},
		{name: "no series", req: Request{Job: "j"}},
		{name: "empty name", req: Request{Job: "j", Series: []SeriesInput{{Value: v(1)}}}},
		{name: "bad metric name", req: Request{Job: "j", Series: []SeriesInput{{Name: "1bad", Value: v(1)}}}},
		{name: "neither value nor samples", req: Request{Job: "j", Series: []SeriesInput{{Name: "m"}}}},
		{name: "both value and samples", req: Request{Job: "j", Series: []SeriesInput{{Name: "m", Value: v(1), Samples: []SamplePoint{{Value: v(1)}}}}}},
		{name: "empty samples", req: Request{Job: "j", Series: []SeriesInput{{Name: "m", Samples: []SamplePoint{}}}}},
		{name: "bad label name", req: Request{Job: "j", Series: []SeriesInput{{Name: "m", Labels: map[string]string{"a-b": "1"}, Value: v(1)}}}},
		{name: "reserved __ label", req: Request{Job: "j", Series: []SeriesInput{{Name: "m", Labels: map[string]string{"__x": "1"}, Value: v(1)}}}},
		{name: "client job label allowed (overridden)", req: Request{Job: "j", Series: []SeriesInput{{Name: "m", Labels: map[string]string{"job": "evil"}, Value: v(1)}}}, ok: true},
	}
	for _, tc := range cases {
		err := tc.req.validate()
		if tc.ok && err != nil {
			t.Errorf("%s: unexpected error %v", tc.name, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("%s: expected validation error", tc.name)
		}
	}
}

func TestBuildLabelsOverridesReserved(t *testing.T) {
	s := SeriesInput{Name: "http_requests_total", Labels: map[string]string{
		"method":   "GET",
		"job":      "evil",   // must be overridden
		"instance": "evil",   // must be overridden
	}}
	got := buildLabels(s, "realjob", "realinst")
	want := model.FromStrings(
		model.MetricName, "http_requests_total",
		"method", "GET",
		"job", "realjob",
		"instance", "realinst",
	)
	if !got.Equal(want) {
		t.Errorf("buildLabels = %s, want %s", got.String(), want.String())
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/push/ -run 'TestRequestValidate|TestBuildLabels'`
Expected: FAIL — `Request`, `SeriesInput`, `SamplePoint`, `validate`, `buildLabels` undefined.

- [ ] **Step 3: Write minimal implementation**

```go
// Request is the JSON push body. The /api/v1/ route pins the schema version.
type Request struct {
	Job      string        `json:"job"`
	Instance string        `json:"instance"`
	Series   []SeriesInput `json:"series"`
}

// SeriesInput is one metric in a push: a name, optional extra labels, and either
// a single shorthand Value (stamped at request time) or explicit Samples.
type SeriesInput struct {
	Name    string            `json:"name"`
	Labels  map[string]string `json:"labels"`
	Value   *Value            `json:"value"`
	Samples []SamplePoint     `json:"samples"`
}

// SamplePoint is one explicit observation. TimestampMs == 0 means request time.
type SamplePoint struct {
	TimestampMs int64 `json:"timestamp_ms"`
	Value       Value `json:"value"`
}

// reservedLabels are set by the server and may not be supplied by a client; if
// present in Labels they are silently dropped (server values win).
var reservedLabels = map[string]string{model.MetricName: "", "job": "", "instance": ""}

func (r *Request) validate() error {
	if r.Job == "" {
		return fmt.Errorf("job must not be empty")
	}
	if len(r.Series) == 0 {
		return fmt.Errorf("series must not be empty")
	}
	for i := range r.Series {
		if err := r.Series[i].validate(); err != nil {
			return fmt.Errorf("series[%d]: %w", i, err)
		}
	}
	return nil
}

func (s *SeriesInput) validate() error {
	if !isValidMetricName(s.Name) {
		return fmt.Errorf("invalid metric name %q", s.Name)
	}
	hasValue := s.Value != nil
	hasSamples := len(s.Samples) > 0
	switch {
	case hasValue && hasSamples:
		return fmt.Errorf("set exactly one of value or samples, not both")
	case !hasValue && !hasSamples:
		// An explicitly empty samples array also lands here.
		return fmt.Errorf("set exactly one of value or samples")
	}
	for name := range s.Labels {
		if _, reserved := reservedLabels[name]; reserved {
			continue // dropped/overridden by the server, not an error
		}
		if !isValidLabelName(name) {
			return fmt.Errorf("invalid label name %q", name)
		}
	}
	return nil
}

// buildLabels assembles the stored label set: __name__ from Name, the client's
// non-reserved labels, then the server-owned job and instance (which override any
// client-supplied values).
func buildLabels(s SeriesInput, job, instance string) model.Labels {
	m := make(map[string]string, len(s.Labels)+3)
	for k, v := range s.Labels {
		if _, reserved := reservedLabels[k]; reserved {
			continue
		}
		m[k] = v
	}
	m[model.MetricName] = s.Name
	m["job"] = job
	m["instance"] = instance
	return model.FromMap(m)
}

// samplePoints expands a series into concrete (timestamp, value) pairs, using
// nowMs for the shorthand Value and for any sample with TimestampMs == 0.
func (s *SeriesInput) samplePoints(nowMs int64) []SamplePoint {
	if s.Value != nil {
		return []SamplePoint{{TimestampMs: nowMs, Value: *s.Value}}
	}
	out := make([]SamplePoint, len(s.Samples))
	for i, p := range s.Samples {
		if p.TimestampMs == 0 {
			p.TimestampMs = nowMs
		}
		out[i] = p
	}
	return out
}

func isValidMetricName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := c == '_' || c == ':' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		if i > 0 {
			ok = ok || (c >= '0' && c <= '9')
		}
		if !ok {
			return false
		}
	}
	return true
}

func isValidLabelName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		if i > 0 {
			ok = ok || (c >= '0' && c <= '9')
		}
		if !ok {
			return false
		}
	}
	return true
}
```

Add the `model` import to the existing `import` block in `push.go`:

```go
import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"

	"github.com/pod32g/omni-metrics/internal/model"
)
```

> Note: `value.go` already declares `package push` with the `bytes/encoding/json/fmt/math` imports. Put the schema types, validators, and `model` import in `push.go`; do not duplicate the `Value` type. If the compiler reports an unused import in either file, move shared imports to the file that uses them.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/push/`
Expected: PASS (all push tests so far).

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/push/
git add internal/push/push.go internal/push/push_test.go
git -c user.name=pod32g -c user.email=3311662+pod32g@users.noreply.github.com commit -m "feat(push): request schema, validation, reserved-label-safe label building"
```

---

## Task 3: Ingester, health registry, atomic append

**Files:**
- Create: `internal/push/ingest.go`
- Test: `internal/push/ingest_test.go`

- [ ] **Step 1: Write the failing test**

```go
package push

import (
	"errors"
	"sync"
	"testing"

	"github.com/pod32g/omni-metrics/internal/model"
	"github.com/pod32g/omni-metrics/internal/tsdb"
)

func newDB(t *testing.T, maxSeries int) *tsdb.DB {
	t.Helper()
	db, err := tsdb.Open(tsdb.Options{MaxSeries: maxSeries})
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func ptr(f float64) *Value { v := Value(f); return &v }

func TestIngestAppendsSeries(t *testing.T) {
	db := newDB(t, 0)
	ing := NewIngester(db, 0)
	req := &Request{Job: "app", Series: []SeriesInput{
		{Name: "reqs_total", Value: ptr(5)},
		{Name: "latency", Samples: []SamplePoint{{TimestampMs: 1000, Value: ptr2(0.1)}, {TimestampMs: 2000, Value: ptr2(0.2)}}},
	}}
	res, err := ing.Ingest(req, "10.0.0.9", 9999)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if res.SeriesTouched != 2 || res.SamplesAppended != 3 {
		t.Fatalf("res = %+v, want 2 series / 3 samples", res)
	}
	// instance defaulted to remote host
	q := db.Querier()
	ss := q.Select(0, 10000, model.Matcher{Type: model.MatchEqual, Name: "instance", Value: "10.0.0.9"})
	got := 0
	for ss.Next() {
		got++
	}
	if got != 2 {
		t.Errorf("series with instance=10.0.0.9 = %d, want 2", got)
	}
}

// ptr2 is a samples-field helper: SamplePoint.Value is a Value, not *Value.
func ptr2(f float64) Value { return Value(f) }

func TestIngestRollsBackOnCardinalityCap(t *testing.T) {
	db := newDB(t, 1) // head holds only 1 series
	ing := NewIngester(db, 0)
	req := &Request{Job: "app", Series: []SeriesInput{
		{Name: "a", Value: ptr(1)},
		{Name: "b", Value: ptr(2)}, // creating this exceeds the cap
	}}
	_, err := ing.Ingest(req, "h", 100)
	if err == nil {
		t.Fatal("expected cardinality error")
	}
	var ie *IngestError
	if !errors.As(err, &ie) || ie.Kind != ErrInternal {
		t.Fatalf("want ErrInternal IngestError, got %v", err)
	}
	if !errors.Is(err, tsdb.ErrTooManySeries) {
		t.Errorf("error should unwrap to ErrTooManySeries: %v", err)
	}
	// Atomic: nothing from this push should be queryable.
	q := db.Querier()
	ss := q.Select(0, 1000, model.Matcher{Type: model.MatchEqual, Name: "job", Value: "app"})
	if ss.Next() {
		t.Error("rolled-back push left data behind")
	}
}

func TestIngestSampleLimit(t *testing.T) {
	db := newDB(t, 0)
	ing := NewIngester(db, 1) // at most 1 sample per push
	req := &Request{Job: "app", Series: []SeriesInput{{Name: "a", Value: ptr(1)}, {Name: "b", Value: ptr(2)}}}
	_, err := ing.Ingest(req, "h", 100)
	var ie *IngestError
	if !errors.As(err, &ie) || ie.Kind != ErrBadData {
		t.Fatalf("want ErrBadData, got %v", err)
	}
}

func TestIngestValidationError(t *testing.T) {
	db := newDB(t, 0)
	ing := NewIngester(db, 0)
	_, err := ing.Ingest(&Request{Job: "", Series: nil}, "h", 1)
	var ie *IngestError
	if !errors.As(err, &ie) || ie.Kind != ErrBadData {
		t.Fatalf("want ErrBadData, got %v", err)
	}
}

func TestSourcesHealth(t *testing.T) {
	db := newDB(t, 0)
	ing := NewIngester(db, 0)
	_, _ = ing.Ingest(&Request{Job: "app", Instance: "i1", Series: []SeriesInput{{Name: "a", Value: ptr(1)}}}, "h", 100)
	_, _ = ing.Ingest(&Request{Job: "app", Instance: "i1", Series: []SeriesInput{{Name: "a", Value: ptr(2)}}}, "h", 200)
	srcs := ing.Sources()
	if len(srcs) != 1 {
		t.Fatalf("sources = %d, want 1", len(srcs))
	}
	s := srcs[0]
	if s.Job != "app" || s.Instance != "i1" {
		t.Errorf("identity = %s/%s", s.Job, s.Instance)
	}
	if s.PushesTotal != 2 || s.SamplesTotal != 2 || s.LastError != "" {
		t.Errorf("health = %+v", s)
	}
}

func TestIngestConcurrent(t *testing.T) {
	db := newDB(t, 0)
	ing := NewIngester(db, 0)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			req := &Request{Job: "app", Instance: "i", Series: []SeriesInput{{Name: "a", Samples: []SamplePoint{{TimestampMs: int64(n + 1), Value: ptr2(float64(n))}}}}}
			_, _ = ing.Ingest(req, "h", int64(n+1))
		}(i)
	}
	wg.Wait()
	if len(ing.Sources()) != 1 {
		t.Errorf("expected 1 source")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/push/ -run 'TestIngest|TestSources'`
Expected: FAIL — `NewIngester`, `Ingester`, `IngestError`, `ErrInternal`, `ErrBadData`, `Result`, `Source` undefined.

- [ ] **Step 3: Write minimal implementation**

```go
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
```

- [ ] **Step 4: Run tests (with race) to verify they pass**

Run: `go test ./internal/push/ -race`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/push/
git add internal/push/ingest.go internal/push/ingest_test.go
git -c user.name=pod32g -c user.email=3311662+pod32g@users.noreply.github.com commit -m "feat(push): atomic Ingester with cardinality rollback and source health"
```

---

## Task 4: `PushConfig` in config

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/config/config_test.go`:

```go
func TestPushConfigDefaults(t *testing.T) {
	// No push block => enabled with the default 16 MiB body cap.
	c, err := LoadBytes([]byte("web:\n  listen: 127.0.0.1:9090\n"))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	if !c.Push.IsEnabled() {
		t.Error("push should default to enabled")
	}
	if c.Push.BodyLimit() != 16<<20 {
		t.Errorf("default body limit = %d, want %d", c.Push.BodyLimit(), 16<<20)
	}
	if c.Push.SampleLimit != 0 {
		t.Errorf("default sample limit = %d, want 0", c.Push.SampleLimit)
	}
}

func TestPushConfigExplicit(t *testing.T) {
	yaml := `
push:
  enabled: false
  sample_limit: 500
  max_body_bytes: 1048576
  auth_token: s3cr3t
`
	c, err := LoadBytes([]byte(yaml))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	if c.Push.IsEnabled() {
		t.Error("push should be disabled")
	}
	if c.Push.BodyLimit() != 1<<20 {
		t.Errorf("body limit = %d", c.Push.BodyLimit())
	}
	if c.Push.SampleLimit != 500 || c.Push.AuthToken != "s3cr3t" {
		t.Errorf("push config = %+v", c.Push)
	}
}

func TestDefaultEnablesPush(t *testing.T) {
	if !Default().Push.IsEnabled() {
		t.Error("Default() push should be enabled")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run TestPush`
Expected: FAIL — `Config.Push`, `PushConfig`, `IsEnabled`, `BodyLimit` undefined.

- [ ] **Step 3: Write minimal implementation**

In `internal/config/config.go`, add `Push` to the root `Config`:

```go
// Config is the root configuration.
type Config struct {
	Global        GlobalConfig   `yaml:"global"`
	Storage       StorageConfig  `yaml:"storage"`
	Web           WebConfig      `yaml:"web"`
	ScrapeConfigs []ScrapeConfig `yaml:"scrape_configs"`
	Push          PushConfig     `yaml:"push"`
}
```

Add the type and accessors (e.g. after `WebConfig`):

```go
// PushConfig configures the JSON push-ingestion endpoint. Enabled is a *bool so
// an omitted block defaults to enabled (nil) rather than false.
type PushConfig struct {
	Enabled      *bool  `yaml:"enabled"`
	SampleLimit  int    `yaml:"sample_limit"`
	MaxBodyBytes int64  `yaml:"max_body_bytes"`
	AuthToken    string `yaml:"auth_token"`
}

// DefaultPushBodyBytes bounds a push request body (16 MiB).
const DefaultPushBodyBytes = 16 << 20

// IsEnabled reports whether the push endpoint should be served (default true).
func (p PushConfig) IsEnabled() bool { return p.Enabled == nil || *p.Enabled }

// BodyLimit returns the configured request-body cap, or the default when unset.
func (p PushConfig) BodyLimit() int64 {
	if p.MaxBodyBytes > 0 {
		return p.MaxBodyBytes
	}
	return DefaultPushBodyBytes
}
```

> No change to `applyDefaults`/`validate` is required — the accessors supply defaults, so both `Default()` (which skips `applyDefaults`) and `LoadBytes` behave correctly.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/config/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/config/
git add internal/config/config.go internal/config/config_test.go
git -c user.name=pod32g -c user.email=3311662+pod32g@users.noreply.github.com commit -m "feat(config): push block (enabled, sample_limit, max_body_bytes, auth_token)"
```

---

## Task 5: Push self-metrics

**Files:**
- Modify: `internal/api/selfmetrics.go`
- Test: `internal/api/selfmetrics_test.go` (create)

- [ ] **Step 1: Write the failing test**

```go
package api

import (
	"strings"
	"testing"
)

func TestSelfMetricsPushExposition(t *testing.T) {
	s := NewSelfMetrics("test", nil)
	s.IncPushRequest(true)
	s.IncPushRequest(false)
	s.AddPushSamples(7)
	s.IncPushRejected()

	var b strings.Builder
	s.WriteExposition(&b)
	out := b.String()
	for _, want := range []string{
		`omni_push_requests_total{status="success"} 1`,
		`omni_push_requests_total{status="error"} 1`,
		`omni_push_samples_appended_total 7`,
		`omni_push_series_rejected_total 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("exposition missing %q\n---\n%s", want, out)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/api/ -run TestSelfMetricsPush`
Expected: FAIL — `IncPushRequest`, `AddPushSamples`, `IncPushRejected` undefined.

- [ ] **Step 3: Write minimal implementation**

In `internal/api/selfmetrics.go`, add fields to `SelfMetrics`:

```go
	queries        int64
	queryErrors    int64
	pushOK         int64
	pushErr        int64
	pushSamples    int64
	pushRejected   int64
```

Add methods:

```go
// IncPushRequest counts a push request, flagging success or failure.
func (s *SelfMetrics) IncPushRequest(ok bool) {
	s.mu.Lock()
	if ok {
		s.pushOK++
	} else {
		s.pushErr++
	}
	s.mu.Unlock()
}

// AddPushSamples adds to the count of successfully appended pushed samples.
func (s *SelfMetrics) AddPushSamples(n int) {
	s.mu.Lock()
	s.pushSamples += int64(n)
	s.mu.Unlock()
}

// IncPushRejected counts a push rejected by the head cardinality cap.
func (s *SelfMetrics) IncPushRejected() {
	s.mu.Lock()
	s.pushRejected++
	s.mu.Unlock()
}
```

In `WriteExposition`, after the `omni_query_errors_total` block and before the `headSeries` block, add:

```go
	fmt.Fprintf(w, "# HELP omni_push_requests_total Total push requests by status.\n")
	fmt.Fprintf(w, "# TYPE omni_push_requests_total counter\n")
	fmt.Fprintf(w, "omni_push_requests_total{status=%q} %d\n", "success", s.pushOK)
	fmt.Fprintf(w, "omni_push_requests_total{status=%q} %d\n", "error", s.pushErr)

	fmt.Fprintf(w, "# HELP omni_push_samples_appended_total Total samples appended via push.\n")
	fmt.Fprintf(w, "# TYPE omni_push_samples_appended_total counter\n")
	fmt.Fprintf(w, "omni_push_samples_appended_total %d\n", s.pushSamples)

	fmt.Fprintf(w, "# HELP omni_push_series_rejected_total Push appends rejected by the head series cap.\n")
	fmt.Fprintf(w, "# TYPE omni_push_series_rejected_total counter\n")
	fmt.Fprintf(w, "omni_push_series_rejected_total %d\n", s.pushRejected)
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/api/ -run TestSelfMetricsPush`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/api/
git add internal/api/selfmetrics.go internal/api/selfmetrics_test.go
git -c user.name=pod32g -c user.email=3311662+pod32g@users.noreply.github.com commit -m "feat(api): push self-metrics counters"
```

---

## Task 6: Push HTTP handler, auth, and sources endpoint

**Files:**
- Create: `internal/api/push.go`
- Modify: `internal/api/api.go`
- Test: `internal/api/push_test.go`

- [ ] **Step 1: Write the failing test**

```go
package api_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pod32g/omni-metrics/internal/api"
	"github.com/pod32g/omni-metrics/internal/promql"
	"github.com/pod32g/omni-metrics/internal/push"
	"github.com/pod32g/omni-metrics/internal/tsdb"
)

func buildPushAPI(t *testing.T, maxSeries, sampleLimit int, token string) (http.Handler, *tsdb.DB) {
	t.Helper()
	db, err := tsdb.Open(tsdb.Options{MaxSeries: maxSeries})
	if err != nil {
		t.Fatal(err)
	}
	ing := push.NewIngester(db, sampleLimit)
	h := api.New(api.Options{
		Engine:      promql.NewEngine(db),
		Storage:     db,
		Version:     "test",
		HeadSeries:  db.HeadSeries,
		Push:        ing,
		PushSources: ing,
		PushConfig:  api.PushConfig{Enabled: true, MaxBodyBytes: 1 << 20, AuthToken: token},
	})
	return h, db
}

func postJSON(t *testing.T, h http.Handler, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestPushSuccess(t *testing.T) {
	h, db := buildPushAPI(t, 0, 0, "")
	body := `{"job":"app","instance":"i1","series":[{"name":"reqs_total","value":5},{"name":"q","value":2}]}`
	rec := postJSON(t, h, "/api/v1/push", "", body)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"status":"success"`) {
		t.Fatalf("code %d body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"samplesAppended":2`) {
		t.Errorf("missing count: %s", rec.Body.String())
	}
	if db.HeadSeries() != 2 {
		t.Errorf("head series = %d, want 2", db.HeadSeries())
	}
}

func TestPushBadJSON(t *testing.T) {
	h, _ := buildPushAPI(t, 0, 0, "")
	rec := postJSON(t, h, "/api/v1/push", "", `{not json`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"errorType":"bad_data"`) {
		t.Fatalf("code %d body %s", rec.Code, rec.Body.String())
	}
}

func TestPushValidationError(t *testing.T) {
	h, _ := buildPushAPI(t, 0, 0, "")
	rec := postJSON(t, h, "/api/v1/push", "", `{"job":"","series":[]}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"errorType":"bad_data"`) {
		t.Fatalf("code %d body %s", rec.Code, rec.Body.String())
	}
}

func TestPushCardinalityCapIs503(t *testing.T) {
	h, _ := buildPushAPI(t, 1, 0, "")
	body := `{"job":"app","series":[{"name":"a","value":1},{"name":"b","value":2}]}`
	rec := postJSON(t, h, "/api/v1/push", "", body)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), `"errorType":"internal"`) {
		t.Fatalf("code %d body %s", rec.Code, rec.Body.String())
	}
}

func TestPushBodyTooLargeIs413(t *testing.T) {
	h, _ := buildPushAPI(t, 0, 0, "")
	big := `{"job":"app","series":[{"name":"a","value":1}],"pad":"` + strings.Repeat("x", 2<<20) + `"}`
	rec := postJSON(t, h, "/api/v1/push", "", big)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code %d body %s", rec.Code, rec.Body.String())
	}
}

func TestPushAuth(t *testing.T) {
	h, _ := buildPushAPI(t, 0, 0, "s3cr3t")
	body := `{"job":"app","series":[{"name":"a","value":1}]}`
	// missing token
	if rec := postJSON(t, h, "/api/v1/push", "", body); rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing token: code %d", rec.Code)
	}
	// wrong token
	if rec := postJSON(t, h, "/api/v1/push", "nope", body); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: code %d", rec.Code)
	}
	// right token
	if rec := postJSON(t, h, "/api/v1/push", "s3cr3t", body); rec.Code != 200 {
		t.Fatalf("right token: code %d body %s", rec.Code, rec.Body.String())
	}
}

func TestPushSourcesEndpoint(t *testing.T) {
	h, _ := buildPushAPI(t, 0, 0, "")
	postJSON(t, h, "/api/v1/push", "", `{"job":"app","instance":"i1","series":[{"name":"a","value":1}]}`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/push/sources", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"job":"app"`) {
		t.Fatalf("sources: code %d body %s", rec.Code, rec.Body.String())
	}
}

func TestPushDisabledNotRegistered(t *testing.T) {
	db, _ := tsdb.Open(tsdb.Options{})
	h := api.New(api.Options{
		Engine:     promql.NewEngine(db),
		Storage:    db,
		Version:    "test",
		HeadSeries: db.HeadSeries,
		// Push/PushSources nil, PushConfig.Enabled false
	})
	rec := postJSON(t, h, "/api/v1/push", "", `{"job":"app","series":[{"name":"a","value":1}]}`)
	if rec.Code == 200 {
		t.Errorf("push should not be served when disabled")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/api/ -run TestPush`
Expected: FAIL — `api.PushConfig`, `Options.Push`, `Options.PushSources`, routes undefined.

- [ ] **Step 3: Write minimal implementation**

Create `internal/api/push.go`:

```go
package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/pod32g/omni-metrics/internal/push"
)

// Pusher ingests a decoded push request (satisfied by *push.Ingester).
type Pusher interface {
	Ingest(req *push.Request, remoteHost string, nowMs int64) (push.Result, error)
}

// PushSourcesProvider supplies push-source health for /api/v1/push/sources.
type PushSourcesProvider interface {
	Sources() []push.Source
}

// PushConfig configures the push handler at the HTTP layer.
type PushConfig struct {
	Enabled      bool
	MaxBodyBytes int64
	AuthToken    string // empty = no auth
}

func (a *API) handlePush(w http.ResponseWriter, r *http.Request) {
	a.self.IncHTTP("push")
	if !a.authorizePush(r) {
		a.self.IncPushRequest(false)
		writeError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token")
		return
	}
	limit := a.opts.PushConfig.MaxBodyBytes
	if limit <= 0 {
		limit = 16 << 20
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)

	var req push.Request
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&req); err != nil {
		a.self.IncPushRequest(false)
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "bad_data", "request body too large")
			return
		}
		writeError(w, http.StatusBadRequest, "bad_data", "invalid JSON: "+err.Error())
		return
	}

	res, err := a.opts.Push.Ingest(&req, remoteHost(r), time.Now().UnixMilli())
	if err != nil {
		a.self.IncPushRequest(false)
		var ie *push.IngestError
		if errors.As(err, &ie) && ie.Kind == push.ErrInternal {
			if errors.Is(err, errTooManySeries()) {
				a.self.IncPushRejected()
			}
			writeError(w, http.StatusServiceUnavailable, "internal", ie.Msg)
			return
		}
		msg := err.Error()
		if ie != nil {
			msg = ie.Msg
		}
		writeError(w, http.StatusBadRequest, "bad_data", msg)
		return
	}
	a.self.IncPushRequest(true)
	a.self.AddPushSamples(res.SamplesAppended)
	writeData(w, map[string]int{"samplesAppended": res.SamplesAppended, "seriesTouched": res.SeriesTouched})
}

func (a *API) handlePushSources(w http.ResponseWriter, r *http.Request) {
	a.self.IncHTTP("push_sources")
	var srcs []push.Source
	if a.opts.PushSources != nil {
		srcs = a.opts.PushSources.Sources()
	}
	writeData(w, srcs)
}

// authorizePush returns true when no token is configured or the request carries
// the matching bearer token (constant-time compared).
func (a *API) authorizePush(r *http.Request) bool {
	want := a.opts.PushConfig.AuthToken
	if want == "" {
		return true
	}
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func remoteHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
```

The handler must detect the cardinality cap without importing `tsdb` redundantly. Add a tiny helper in `push.go` (Task 3 file) so the API can check it via the wrapped error. Add to `internal/push/ingest.go`:

```go
// ErrTooManySeries re-exports the storage cardinality-cap sentinel so callers
// (e.g. the API) can classify a push rejection without importing tsdb.
var ErrTooManySeries = tsdb.ErrTooManySeries
```

Then in `internal/api/push.go`, replace `errTooManySeries()` usage with `push.ErrTooManySeries`:

```go
			if errors.Is(err, push.ErrTooManySeries) {
				a.self.IncPushRejected()
			}
```

(Delete the `errTooManySeries()` placeholder — use `push.ErrTooManySeries` directly.)

In `internal/api/api.go`, extend `Options`:

```go
// Options configures the API server.
type Options struct {
	Engine      *promql.Engine
	Storage     StorageQ
	Targets     TargetsProvider
	Web         http.Handler
	Version     string
	HeadSeries  func() int
	Push        Pusher
	PushSources PushSourcesProvider
	PushConfig  PushConfig
}
```

In `api.go` `routes()`, register the push routes when enabled (after the `/metrics` line, before the health routes):

```go
	if a.opts.PushConfig.Enabled && a.opts.Push != nil {
		a.mux.HandleFunc("POST /api/v1/push", a.handlePush)
		a.mux.HandleFunc("GET /api/v1/push/sources", a.handlePushSources)
	}
```

- [ ] **Step 4: Run tests (with race) to verify they pass**

Run: `go test ./internal/api/ -race`
Expected: PASS (existing api tests + all new push tests).

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/
git add internal/api/push.go internal/api/api.go internal/api/push_test.go internal/push/ingest.go
git -c user.name=pod32g -c user.email=3311662+pod32g@users.noreply.github.com commit -m "feat(api): POST /api/v1/push handler with auth, limits, and /push/sources"
```

---

## Task 7: Wire the Ingester into the server

**Files:**
- Modify: `cmd/omni/main.go`

- [ ] **Step 1: Add the import**

In `cmd/omni/main.go` imports, add:

```go
	"github.com/pod32g/omni-metrics/internal/push"
```

- [ ] **Step 2: Construct the ingester and wire the API**

After the scraper is created (`mgr := scrape.NewManager(db, 0)` / `go mgr.Run(...)`) and before building the API handler, add:

```go
	// Push ingestion: clients that cannot be scraped POST samples here.
	ingester := push.NewIngester(db, cfg.Push.SampleLimit)
```

Replace the `api.New(api.Options{...})` call with one that includes the push fields:

```go
	handler := api.New(api.Options{
		Engine:      promql.NewEngine(db),
		Storage:     db,
		Targets:     mgr,
		Web:         web.Handler(),
		Version:     version,
		HeadSeries:  db.HeadSeries,
		Push:        ingester,
		PushSources: ingester,
		PushConfig: api.PushConfig{
			Enabled:      cfg.Push.IsEnabled(),
			MaxBodyBytes: cfg.Push.BodyLimit(),
			AuthToken:    cfg.Push.AuthToken,
		},
	})
```

- [ ] **Step 3: Build and run the existing tests**

Run: `go build ./... && go test ./cmd/... ./internal/... -race`
Expected: builds clean; all tests PASS.

- [ ] **Step 4: Commit**

```bash
gofmt -w cmd/
git add cmd/omni/main.go
git -c user.name=pod32g -c user.email=3311662+pod32g@users.noreply.github.com commit -m "feat(omni): wire push ingester into the server"
```

---

## Task 8: "Pushers" web console view

**Files:**
- Modify: `web/assets/index.html`
- Modify: `web/assets/app.js`
- Test: `web/web_test.go`

- [ ] **Step 1: Write the failing test**

Append to `web/web_test.go` inside `TestHandlerServesIndexAndAssets`'s `cases` slice the two new expectations, OR add a focused test:

```go
func TestPushersViewShips(t *testing.T) {
	h := Handler()
	cases := []struct{ path, want string }{
		{"/app.js", "renderPushers"},
		{"/", `data-route="pushers"`},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if !strings.Contains(rec.Body.String(), tc.want) {
			t.Errorf("%s: missing %q", tc.path, tc.want)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./web/ -run TestPushersViewShips`
Expected: FAIL — neither string present yet.

- [ ] **Step 3a: Add the nav link**

In `web/assets/index.html`, change the `<nav>` block to include Pushers after Targets:

```html
      <nav class="nav" id="nav">
        <a href="#/graph" data-route="graph">Graph</a>
        <a href="#/targets" data-route="targets">Targets</a>
        <a href="#/pushers" data-route="pushers">Pushers</a>
        <a href="#/status" data-route="status">Status</a>
      </nav>
```

- [ ] **Step 3b: Route to the new view**

In `web/assets/app.js`, update the router to handle `pushers`:

```js
    if (name === "targets") renderTargets(view);
    else if (name === "pushers") renderPushers(view);
    else if (name === "status") renderStatus(view);
    else renderGraph(view);
```

- [ ] **Step 3c: Implement `renderPushers`**

Add this function next to `renderTargets` in `web/assets/app.js`:

```js
  /* ---------- pushers view ---------- */
  function renderPushers(view) {
    api("/api/v1/push/sources").then(function (sources) {
      sources = sources || [];
      var ok = sources.filter(function (s) { return !s.lastError; }).length;
      view.appendChild(el("div", { class: "page-head" }, [
        el("div", {}, [
          el("div", { class: "eyebrow" }, ["Push"]),
          el("div", { class: "summary" }, [
            el("h1", {}, ["Pushers"]),
            el("span", { class: "mono up" }, [ok + " ok"]),
            el("span", { class: "mono down" }, [(sources.length - ok) + " err"]),
          ]),
        ]),
      ]));
      var panel = el("div", { class: "panel" }, [
        el("div", { class: "col-head" }, [
          el("span", { style: "flex:1.4" }, ["Source"]),
          el("span", { style: "width:90px" }, ["State"]),
          el("span", { style: "width:120px" }, ["Last push"]),
          el("span", { style: "width:90px;text-align:right" }, ["Pushes"]),
          el("span", { style: "width:110px;text-align:right" }, ["Samples"]),
          el("span", { style: "flex:1", "data-dim": "1" }, ["Error"]),
        ]),
      ]);
      if (!sources.length) panel.appendChild(el("div", { class: "empty" }, ["No pushers have reported yet."]));
      sources.forEach(function (s) {
        var fresh = !s.lastError;
        panel.appendChild(el("div", { class: "row" }, [
          el("span", { style: "flex:1.4" }, ['job="' + s.job + '" instance="' + s.instance + '"']),
          el("span", { style: "width:90px" }, [okPill(fresh)]),
          el("span", { style: "width:120px" }, [s.lastPush && new Date(s.lastPush).getTime() ? ago(s.lastPush) : "–"]),
          el("span", { class: "mono", style: "width:90px;text-align:right;display:inline-block" }, [String(s.pushesTotal || 0)]),
          el("span", { class: "mono", style: "width:110px;text-align:right;display:inline-block" }, [String(s.samplesTotal || 0)]),
          el("span", { style: "flex:1" }, [s.lastError || ""]),
        ]));
      });
      view.appendChild(panel);
    }).catch(function (err) { view.appendChild(el("div", { class: "error-banner" }, [String(err.message || err)])); });
  }

  function okPill(ok) {
    var pill = el("span", { class: "pill " + (ok ? "up" : "down") }, [el("span", { class: "dot " + (ok ? "ok" : "err") }, [])]);
    pill.appendChild(document.createTextNode(ok ? "OK" : "ERR"));
    return pill;
  }
```

> Reuses the existing `pill`/`dot`/`row`/`panel`/`col-head`/`empty`/`mono` classes (already AA-contrast-verified in both themes) and the existing `ago()` helper. All API strings are inserted via `el(...)` text nodes — the file's XSS-safe pattern.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./web/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add web/assets/index.html web/assets/app.js web/web_test.go
git -c user.name=pod32g -c user.email=3311662+pod32g@users.noreply.github.com commit -m "feat(web): Pushers console view backed by /api/v1/push/sources"
```

---

## Task 9: Documentation

**Files:**
- Modify: `examples/omni.yml`
- Modify: `README.md`

- [ ] **Step 1: Add a push block to the example config**

Append to `examples/omni.yml`:

```yaml
# Push ingestion: clients that cannot be scraped POST JSON samples to
# /api/v1/push. Append semantics (each push builds the time series).
push:
  enabled: true
  sample_limit: 0          # max samples per request; 0 = unlimited
  max_body_bytes: 16777216 # 16 MiB request-body cap
  auth_token: ""           # when set, requires "Authorization: Bearer <token>"
```

- [ ] **Step 2: Document the endpoint in the README**

Add a "Push ingestion" subsection under the API section of `README.md`:

````markdown
### Push ingestion

A process that has no HTTP server to scrape can push instead:

```sh
curl -XPOST http://127.0.0.1:9090/api/v1/push \
  -H 'Content-Type: application/json' \
  -d '{"job":"batch","instance":"worker-7","series":[
        {"name":"records_processed_total","value":1500},
        {"name":"queue_depth","labels":{"queue":"high"},"value":12}
      ]}'
```

Each push **appends** samples (building a real time series), so `rate()` works on
pushed counters. Per-series, supply either `value` (one sample at receive time) or
`samples: [{"timestamp_ms":…, "value":…}]`. `value` accepts a number or the
strings `"NaN"`, `"+Inf"`, `"-Inf"`. The server injects `job`/`instance` and a
client cannot override `__name__`/`job`/`instance`. Push-source health is shown on
the **Pushers** console page and at `GET /api/v1/push/sources`. Configure limits
and an optional bearer token via the `push:` config block.
````

- [ ] **Step 3: Verify docs build is consistent (no code change to test)**

Run: `go build ./...`
Expected: builds clean (sanity only).

- [ ] **Step 4: Commit**

```bash
git add examples/omni.yml README.md
git -c user.name=pod32g -c user.email=3311662+pod32g@users.noreply.github.com commit -m "docs: document push ingestion endpoint and config"
```

---

## Task 10: Quality gate and end-to-end verification

**Files:** none (verification only).

- [ ] **Step 1: Full quality gate**

Run:
```bash
gofmt -l . && go vet ./... && go test ./... -race
```
Expected: `gofmt -l .` prints nothing; vet clean; all tests PASS.

- [ ] **Step 2: End-to-end push → query proof**

Run (in two shells or background the server):
```bash
go build -o /tmp/omni ./cmd/omni
/tmp/omni -listen 127.0.0.1:9099 -storage /tmp/omni-push-data &
sleep 1
curl -s -XPOST http://127.0.0.1:9099/api/v1/push -H 'Content-Type: application/json' \
  -d '{"job":"demo","instance":"w1","series":[{"name":"jobs_done_total","value":1}]}'
curl -s -XPOST http://127.0.0.1:9099/api/v1/push -H 'Content-Type: application/json' \
  -d '{"job":"demo","instance":"w1","series":[{"name":"jobs_done_total","value":4}]}'
curl -s 'http://127.0.0.1:9099/api/v1/query?query=jobs_done_total'
curl -s 'http://127.0.0.1:9099/api/v1/push/sources'
curl -s http://127.0.0.1:9099/metrics | grep omni_push
```
Expected: the query returns a `jobs_done_total{job="demo",instance="w1"}` vector; `/push/sources` shows one source with `pushesTotal: 2`; `/metrics` shows `omni_push_*` counters.

- [ ] **Step 3: WAL crash-recovery proof (push writes go through the WAL)**

Run:
```bash
kill -9 %1            # hard-kill the server
/tmp/omni -listen 127.0.0.1:9099 -storage /tmp/omni-push-data &
sleep 1
curl -s 'http://127.0.0.1:9099/api/v1/query?query=jobs_done_total'
kill %1; rm -rf /tmp/omni-push-data
```
Expected: after restart the pushed `jobs_done_total` samples are still queryable (recovered from the WAL).

- [ ] **Step 4: Dark/light console audit**

Open `http://127.0.0.1:9099/#/pushers` in a browser; toggle dark/light. Confirm the Pushers table is legible, OK/ERR pills use the same `--ok`/`--err` tokens as Targets, and AA contrast holds in both themes.

- [ ] **Step 5: Adversarial review**

Dispatch a reviewer (subagent) prompted to **refute**, targeting: push cardinality DoS (cap + rollback), partial-write atomicity, reserved-label forging (`__name__`/`job`/`up`), auth bypass/timing, body-limit bypass, and dark-mode contrast on the new view. Triage findings: apply cheap correctness/observability fixes; document larger ones as explicit deferrals. No final commit on red.

---

## Self-Review (completed during planning)

- **Spec coverage:** endpoint + JSON schema (T1, T2, T6) · append semantics & timestamps (T2, T3) · atomic append + cardinality rollback (T3) · reserved-label protection (T2) · safety guards/error map (T3, T6) · optional bearer token (T6) · self-metrics (T5) · source registry + `/push/sources` (T3, T6) · Pushers UI (T8) · config block (T4) · wiring (T7) · docs (T9) · testing/verify/review (every task + T10). Client follow-up is intentionally out of this plan (separate repo/cycle).
- **Placeholder scan:** the `errTooManySeries()` stand-in in T6 Step 3 is explicitly replaced with `push.ErrTooManySeries` in the same step; no TODO/TBD remain.
- **Type consistency:** `Value` (T1) used as `*Value` in `SeriesInput.Value` and `Value` in `SamplePoint.Value` (T2) — test helpers `ptr`/`ptr2` reflect this. `IngestError{Kind,Msg,Err}` + `ErrBadData`/`ErrInternal` (T3) consumed by the handler (T6). `push.ErrTooManySeries` (T6 added to T3 file) matches `errors.Is` checks. Config accessors `IsEnabled()`/`BodyLimit()` (T4) used in wiring (T7); `api.PushConfig.Enabled bool` (T6) is distinct from `config.PushConfig.Enabled *bool` and bridged via `IsEnabled()` in T7.
