# omni-metrics — Push write model design

**Date:** 2026-06-15
**Status:** Proposed (brainstormed with user; awaiting spec review)
**Module:** `github.com/pod32g/omni-metrics` · binary `omni` · Go 1.25
**Builds on:** [M1 vertical slice](2026-06-15-omni-metrics-m1-design.md)

## Goal

Add a **push ingestion path** so a process that cannot be scraped can still get its
metrics into omni-metrics. The driving use-case: **a long-running app that has no
HTTP server** (so it can't expose `/metrics` to be polled) but still needs to emit
metrics. Instead of the server dialing the app, the app dials the server and
`POST`s its samples.

This is the **inverse of scrape**: where the scraper pulls exposition text and
appends it with injected `job`/`instance` labels, the push path accepts samples
over an outbound request from the client and appends them the same way.

## Key decisions (settled during brainstorming)

- **Wire format:** native **JSON** (not exposition text, not remote-write
  protobuf). Aligns with the existing `{status,data}` envelope and the
  `omni-client-go` JSON style; keeps the binary dependency-light (no protobuf or
  snappy). We own a small, versioned request schema.
- **Semantics:** **append**. Each push appends new sample(s), building a real time
  series per metric — `rate()`/`increase()` work on pushed counters and the graph
  shows history. *Not* Pushgateway-style last-value replacement.
- **Auth:** an **optional static bearer token** (`push.auth_token`), off by
  default. When set, the write endpoint requires `Authorization: Bearer <token>`,
  constant-time compared (`crypto/subtle`, no new deps). TLS stays deferred (front
  with a reverse proxy if exposing publicly).
- **Observability:** self-metrics counters **plus** a per-source health registry,
  a `GET /api/v1/push/sources` JSON endpoint, and a **"Pushers" web console view**
  alongside the existing Targets page.
- **Client side:** the server endpoint ships in this repo. A companion
  `Push`/registry API in **`omni-client-go`** follows as a second piece (separate
  repo → its own short spec/plan/TDD cycle), since the app needs an HTTP *client*
  to do the outbound POST. The JSON contract defined here is the interface between
  the two.

## Architecture & seams

Push plugs into the **same `tsdb.Appender`** the scraper uses; no storage changes.

```
            ┌──────────────── omni server ─────────────────┐
 app (no    │  POST /api/v1/push (JSON)                     │
 HTTP srv)  │      │  auth ─► decode ─► validate            │
   ─────────┼──►   ▼                                        │
            │  push.Ingester ─► tsdb.Appender ─► Commit     │
            │      │  (also updates push-source health)     │
            │      └─► /api/v1/push/sources  ─► Pushers UI   │
            └───────────────────────────────────────────────┘
```

Dependency order (extends M1): `model` ← `tsdb` ← **`push`** ← `api` ← `web`.
`push` depends only on `model` and `tsdb` (a `tsdb.Appendable`), exactly like
`scrape`, and is unit-testable in isolation.

## Components

### `internal/push` (new)

Owns the request schema, decode, validation, the append, and the source-health
registry. Mirrors `internal/scrape`'s shape (a manager-like type over a
`tsdb.Appendable`).

```go
// Request is the JSON push body (apiVersion-less in v1; the path /api/v1/ pins it).
type Request struct {
    Job      string         // required, non-empty
    Instance string         // optional; defaults to the client's remote host
    Series   []SeriesInput  // required, non-empty
}

type SeriesInput struct {
    Name    string            // required; becomes __name__
    Labels  map[string]string // optional extra labels
    Value   *Value            // shorthand: a single sample at request time
    Samples []SamplePoint     // explicit/back-fill samples; mutually exclusive with Value
}

type SamplePoint struct {
    TimestampMs int64 // 0/absent => request receive-time
    Value       Value
}

// Value decodes either a JSON number or one of "NaN","+Inf","-Inf"
// (JSON has no native non-finite floats). Custom UnmarshalJSON.
type Value float64

type Ingester struct { /* app tsdb.Appendable; sampleLimit int; health registry (mu-guarded) */ }

func NewIngester(app tsdb.Appendable, sampleLimit int) *Ingester
// Ingest validates, appends atomically, updates health, and returns a Result
// (counts) or a classified *IngestError (see error mapping).
func (i *Ingester) Ingest(req *Request, remoteHost string, nowMs int64) (Result, error)
func (i *Ingester) Sources() []Source // health snapshot, sorted by job then instance
```

`Result{SamplesAppended int, SeriesTouched int}`.

### `internal/api`

- New route `POST /api/v1/push` → `handlePush`: applies the bearer-token check
  (when configured), wraps the body in `http.MaxBytesReader`, decodes JSON, calls
  `Ingester.Ingest`, maps the result/error to the `{status,data}` envelope, and
  bumps self-metrics. Registered only when push is enabled.
- New route `GET /api/v1/push/sources` → `handlePushSources`: returns the health
  snapshot from a `PushSourcesProvider` (mirrors the existing `TargetsProvider`).
  Read-only; **not** behind the token (matches the unauthenticated read endpoints).
- `Options` gains a push seam: a `Pusher` (the `*push.Ingester`), a
  `PushSourcesProvider`, and `PushConfig{Enabled, MaxBodyBytes, AuthToken}`.
- Self-metrics (`internal/api/selfmetrics.go`) gains:
  `omni_push_requests_total{status="success|error"}` (counter),
  `omni_push_samples_appended_total` (counter),
  `omni_push_series_rejected_total` (counter; cardinality-cap rejections), and the
  handler is counted via `IncHTTP("push")`.

### Source-health registry

Lives inside `push.Ingester` (a `map[job\x00instance]*Source` under a mutex,
exactly like `scrape.Manager.health`). Updated on every push (success or failure).

```go
type Source struct {
    Job          string    `json:"job"`
    Instance     string    `json:"instance"`
    LastPush     time.Time `json:"lastPush"`
    LastError    string    `json:"lastError"`     // "" when the last push succeeded
    PushesTotal  int64     `json:"pushesTotal"`
    SamplesTotal int64     `json:"samplesTotal"`  // cumulative appended
    LastSamples  int       `json:"lastSamples"`   // samples in the most recent push
}
```

Push has no fixed interval, so there is no `up`/`down`: the UI shows an **OK/ERR**
state from `LastError` plus a "last push N ago" freshness, rather than a liveness
pill.

### `internal/config`

New optional `push` block; sensible defaults so an unconfigured run still serves
the endpoint on loopback.

```yaml
push:
  enabled: true              # false => endpoint not registered
  sample_limit: 0            # max samples per request across all series; 0 = unlimited
  max_body_bytes: 16777216   # 16 MiB request-body cap
  auth_token: ""             # empty = no auth; when set, Bearer token required
```

`PushConfig` with defaults applied in `applyDefaults`; validated (e.g.
`max_body_bytes` > 0 when enabled). The default `web.listen` stays loopback —
exposing the writer is an explicit operator choice.

### `web/` (Pushers view)

A new SPA view mirroring the Targets view in `web/assets/app.js`:

- Nav entry `data-route="pushers"` added to `web/assets/index.html`; router gains a
  `pushers` case calling `renderPushers(view)`.
- `renderPushers` fetches `/api/v1/push/sources` and renders a panel with columns:
  **Source** (`job` / `instance`), **State** (OK/ERR pill from `lastError`), **Last
  push** (`ago(lastPush)`), **Pushes**, **Samples** (cumulative), **Error**.
- Reuses existing CSS design tokens and row/panel classes; all API strings inserted
  via `textContent` (same XSS-safe pattern the file documents). WCAG-AA contrast
  preserved in both themes (uses the same `--ok`/`--err`/text tokens).

### `cmd/omni`

Wire a `push.Ingester` over the `*tsdb.DB`, pass it (and `PushConfig` from the
loaded config) into `api.Options`. No change to storage open/replay or shutdown.

## Data model & request handling

### Identity and reserved-label protection

- The server sets `__name__` from each series' `Name`, and injects `job` +
  `instance` for every appended series.
- `instance` defaults to the request's remote-addr **host** when the body omits it
  (always a non-empty identity).
- Any client-supplied `__name__`, `job`, or `instance` inside `Labels` is
  **overridden** by the server values — a pusher cannot forge reserved series such
  as `up` or impersonate another job/instance. (Same protection the exposition
  parser and scraper already enforce.)
- Label names must match `^[a-zA-Z_][a-zA-Z0-9_]*$` and must **not** start with
  `__` (reserved). Violations reject the whole request (`bad_data`).

### Timestamps

- A `SamplePoint.TimestampMs > 0` is honored as-is; `0`/absent uses a single
  `nowMs` captured once per request, so a multi-metric snapshot stays time-aligned.
- The head already tolerates out-of-order inserts and de-dupes equal timestamps
  (keeping the first), so a bursty or slightly-reordered push stream is safe and
  WAL replay stays idempotent — no storage changes required.

### Atomicity

One `tsdb.Appender` per request: every (series × sample) is `Append`ed, then a
single `Commit`. If any `Append` returns `tsdb.ErrTooManySeries`, the appender is
`Rollback`ed and the request fails — **never a partial, half-applied push**. This
is the scraper's atomic-batch pattern reused verbatim.

## Safety guards & error mapping

Guards target the known cardinality-DoS / partial-write bug classes:

| Condition | HTTP | errorType | Notes |
| --- | --- | --- | --- |
| Body exceeds `max_body_bytes` | 413 | `bad_data` | via `http.MaxBytesReader` |
| Malformed JSON | 400 | `bad_data` | decode error |
| Empty `job`, empty `series`, or a series with empty `name` | 400 | `bad_data` | atomic reject |
| Series has neither or both of `value`/`samples`, or empty `samples` | 400 | `bad_data` | exactly one required |
| Invalid/`__`-prefixed label name | 400 | `bad_data` | reserved-name guard |
| Total samples > `sample_limit` (when set) | 400 | `bad_data` | nothing written |
| Head cardinality cap hit (`ErrTooManySeries`) | 503 | `internal` | appender rolled back; capacity guard |
| Missing/invalid bearer token (when configured) | 401 | `unauthorized` | constant-time compare |
| Non-POST method | 405 | — | served by the mux |

Success: `200` `{"status":"success","data":{"samplesAppended":N,"seriesTouched":M}}`.

## Example

```
POST /api/v1/push
Authorization: Bearer s3cr3t        # only when push.auth_token is set
Content-Type: application/json

{
  "job": "batch-importer",
  "instance": "worker-7",
  "series": [
    { "name": "records_processed_total", "value": 1500 },
    { "name": "queue_depth", "labels": {"queue": "high"}, "value": 12 },
    { "name": "import_latency_seconds", "samples": [
        {"timestamp_ms": 1718450000000, "value": 0.12},
        {"timestamp_ms": 1718450015000, "value": 0.20}
    ]}
  ]
}
```

Queryable immediately, e.g. `records_processed_total{job="batch-importer"}` and
`rate(records_processed_total[5m])`.

## Testing & quality (TDD, RED→GREEN)

Table-driven, mirroring each package's existing style; `-race` where concurrent.

- **`internal/push` — decode:** numbers, `"NaN"`/`"+Inf"`/`"-Inf"`, missing fields,
  `value` vs `samples` exclusivity, unknown/garbage JSON.
- **`internal/push` — validate/identity:** empty job/series/name, bad label names,
  `__`-prefix rejection, reserved-label override (client `job`/`__name__` ignored),
  default-instance from remote host.
- **`internal/push` — ingest:** append round-trip; atomic rollback on
  `ErrTooManySeries` (no partial write); sample-limit rejection; per-source health
  updates; concurrent pushes under `-race`.
- **`internal/api` — handler:** success envelope + counts; each error row in the
  mapping table (status code + errorType); `MaxBytesReader` 413; bearer-token
  accept/reject (401, constant-time); self-metric counters increment;
  `GET /api/v1/push/sources` shape.
- **End-to-end (real binary):** `POST` a snapshot → `GET /api/v1/query` proves the
  series is present and `rate()`-able; push twice → confirm two samples (history);
  `kill -9` → restart → WAL replay still recovers pushed samples (push writes go
  through the same WAL); Pushers page renders in dark + light with AA contrast.
- **Quality gate before every commit:** `gofmt -w .` → `go vet ./...` →
  `go test ./... -race` all green.
- **Adversarial review** before integrating: prompted to refute, targeting
  cardinality DoS via push, partial-write/rollback correctness, reserved-label
  forging, auth bypass/timing, and dark-mode contrast on the new view.

## Security & repo hygiene

- Write endpoint is **off the network by default** (loopback bind); exposing it is
  explicit. Optional bearer token guards writes when exposed; compared with
  `crypto/subtle.ConstantTimeCompare`.
- No secrets, internal hostnames/IPs, or absolute home paths committed; example
  `auth_token` values in docs are obvious placeholders.
- Commit identity `pod32g` (noreply email), **no `Co-Authored-By` trailer**.

## Client follow-up (`omni-client-go`, separate cycle)

After the server ships and is verified, add the client side to
`github.com/pod32g/omni-client-go` (its own short spec → plan → TDD → verify):

- A small in-process registry (counters/gauges) plus `Push(ctx)` that serializes
  the registry to the `Request` JSON above and POSTs it to `/api/v1/push`, with a
  `WithPushAuth(token)` option. Dependency-free, stdlib `net/http`, runnable godoc
  example, verified live against the deployed server.

The JSON schema in this spec is the contract between the two repos.

## Deferrals

- **Exposition-text and Prometheus remote-write** ingestion (remote-write remains
  the M6 interop story; protobuf/snappy deps intentionally avoided now).
- **gzip request bodies** (`Content-Encoding: gzip`) — easy later add.
- **Pushgateway-style replace/grouping** semantics (only append in v1).
- **TLS / per-source tokens / rate-limiting** — single shared token only.
