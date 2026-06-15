# omni-metrics — Milestone 1 (vertical slice) design

**Date:** 2026-06-15
**Status:** Approved (user: "implement this project")
**Module:** `github.com/pod32g/omni-metrics` · binary `omni` · Go 1.25

## Goal

Build a Prometheus-shaped metrics system in Go as a single self-contained binary. M1
is a **thin but end-to-end vertical slice**: pull-based scraping → custom head+WAL
storage → PromQL-subset query → Prometheus-compatible HTTP API → an embedded Web UI
with **dark and light themes from the start** (designed in Paper first).

## Key decisions

- **First slice:** end-to-end vertical slice (every layer real but shallow).
- **Ingestion:** pull / scrape, Prometheus text exposition format.
- **Query:** PromQL subset (instant + range), TDD-able scope.
- **Storage:** custom in-memory head block + append-only WAL with crash recovery.
- **UI charting:** hand-rolled, theme-aware SVG line chart (no JS chart dependency).
- **Histograms:** parse + store bucket series in M1; defer `histogram_quantile` to M3.
- **Single node, no auth/TLS** in M1; binds `127.0.0.1` by default.

## Architecture & seams

Six in-process components behind narrow interfaces, each independently testable:

```
 scrape targets        ┌──────────── omni server ────────────┐
 (HTTP /metrics) ─────►│ Scraper ─► Storage(TSDB) ◄─ Query   │
                       │              │  ▲ WAL      engine    │
                       │            disk│           (PromQL)  │
                       │                └── HTTP API ─────────┘
                       │                      │               │
                       │                 Web UI (embed.FS)    │
                       └──────────────────────────────────────┘
```

Dependency order: `model` ← `exposition` ← `scrape`; `model` ← `tsdb` ← `scrape`,
`promql`; `{tsdb,promql,scrape}` ← `api` ← `web`. `config` is standalone.

## Components

### `internal/model`
Shared vocabulary, no dependencies.
- `Label{Name, Value string}`; `Labels []Label` kept **sorted by name**, deduped.
- `Labels.Hash() uint64` — stable identity (FNV-1a over `name\xffvalue\xff…`).
- `Labels.Get(name)`, `Labels.String()` (`{a="1",b="2"}`), `Labels.Map()`.
- `Sample{T int64 /*ms*/, V float64}`.
- `MetricType` enum: counter, gauge, histogram, summary, untyped.
- Matcher types: `MatchEqual, MatchNotEqual, MatchRegexp, MatchNotRegexp` +
  `Matcher{Type, Name, Value, re}` with compiled regexp; `Matcher.Matches(s string) bool`.
- The metric name lives in the reserved label `__name__`.

### `internal/exposition`
Pure parser for the Prometheus text exposition format → `[]Series`-ish output.
- Input: `io.Reader` (scrape body). Output: list of `(Labels, Value, *Timestamp)` plus
  `# TYPE`/`# HELP` metadata keyed by metric family.
- Handles: comments, `# HELP`, `# TYPE`, samples with/without labels, optional
  explicit timestamp, `+Inf/-Inf/NaN`, escaped label values (`\\`, `\"`, `\n`),
  histogram `_bucket{le=…}`/`_sum`/`_count`, summary quantiles.
- Errors are line-scoped and surfaced (no silent drops) but a single bad line does not
  abort the whole scrape unless configured strict.
- Fixture-driven, table tests.

### `internal/tsdb`
Custom storage: in-memory head + WAL. Interfaces (backend-agnostic):
```go
type Appender interface {
    Append(l model.Labels, t int64, v float64) (uint64, error) // returns series ref
    Commit() error
    Rollback() error
}
type Querier interface {
    Select(mint, maxt int64, matchers ...model.Matcher) SeriesSet
    LabelValues(name string, matchers ...model.Matcher) []string
    LabelNames(matchers ...model.Matcher) []string
}
type Storage interface { Appender() Appender; Querier() Querier; Close() error }
```
- **Head:** `map[ref]*memSeries{labels, samples []Sample}`; inverted index
  `map[label]map[value]postingsList(ref)` for matcher resolution; `map[hash]ref` for
  dedupe. Time-bounded retention: drop samples older than `now − retention`.
- **WAL:** segmented append-only files under the data dir. Record kinds:
  `recSeries{ref, labels}` and `recSamples{[]{ref,t,v}}`, length-prefixed +
  CRC32-checked. `Commit()` fsyncs the active segment. On open, **replay all segments**
  to rebuild head (idempotent: replaying twice yields identical head → guards the
  WAL-replay-duplication bug class). Truncated/corrupt trailing record at EOF is
  tolerated (partial write from a crash) and logged, not fatal.
- **Conformance suite:** `tsdb/conformance.go` — a reusable `func TestStorage(t, factory)`
  that any `Storage` implementation must pass (append/select round-trips, matcher
  semantics, retention, label introspection).
- M1 deferral: no on-disk compacted blocks; head window only.

### `internal/scrape`
Pull manager.
- Config: jobs `{name, scrape_interval, targets []string, timeout}`.
- Per target: ticker → HTTP GET `/metrics` (ctx timeout) → `exposition.Parse` → append
  with injected `job`,`instance` labels at the scrape timestamp.
- Synthesized per scrape: `up{job,instance}` (1 ok / 0 fail), `scrape_duration_seconds`,
  `scrape_samples_scraped`.
- **Cardinality guard:** configurable `sample_limit` per scrape and a global
  `max_series` cap in the head; exceeding → scrape marked failed, `up=0`, error
  recorded (guards the cardinality-DoS bug class).
- Target health registry exposed for `/api/v1/targets`.
- Concurrency-safe; tested under `-race`.

### `internal/promql`
Lexer → parser (Pratt/precedence) → evaluator. Returns typed `Result`:
`Scalar`, `Vector` (instant), `Matrix` (range).
- **In scope:** instant selector `m{l="v",l=~"re",l!="x"}`; range selector `m[5m]`;
  scalar literals; unary `-`; binary arithmetic `+ - * / % ^` (scalar↔vector,
  same-labelset vector↔vector); comparison ops producing filtered vectors;
  aggregations `sum avg min max count` with `by()`/`without()`; functions
  `rate irate increase`, `{sum,avg,min,max,count}_over_time`; duration literals.
- **Evaluation:** instant query at time `t`; range query over `[start,end]` step `step`
  → matrix (one eval per step, reusing selection).
- **Deferred (M3):** `histogram_quantile`, `topk/bottomk`, `offset`/`@`, subqueries,
  `on/ignoring/group_left`, `__name__` regex, `count_values`, `stddev/stdvar`.
- **Parity test:** instant query `metric{…}` at `t` == direct `Querier.Select` at `t`
  (two paths must agree — the cross-engine parity guard).

### `internal/api`
`net/http` mux, Prometheus-compatible JSON envelope `{status,data:{resultType,result}}`
and `{status:"error",errorType,error}` on failure.
- `GET /api/v1/query?query=&time=`
- `GET /api/v1/query_range?query=&start=&end=&step=`
- `GET /api/v1/series?match[]=&start=&end=`
- `GET /api/v1/labels`, `GET /api/v1/label/{name}/values`
- `GET /api/v1/targets`
- `GET /metrics` — self-instrumentation (request counts, scrape totals, head series).
- `GET /` and `/graph`, `/targets`, `/status` → Web UI.

### `internal/config`
YAML (`gopkg.in/yaml.v3`). Top level: `global{scrape_interval, scrape_timeout}`,
`storage{path, retention}`, `web{listen}`, `scrape_configs[]{job_name, scrape_interval,
static_configs[]{targets[]}}`. Sensible defaults; a config-less run scrapes only the
server's own `/metrics` (`job="omni"`).

### `web/` (embedded)
Vanilla HTML/CSS/JS, embedded via `embed.FS`. **Designed in Paper first**, both themes.
- **Theming:** design tokens as CSS custom properties (`--bg --surface --text
  --text-muted --border --accent --ok --warn --err` + chart series colors).
  `data-theme="light|dark"` on `<html>`, initialised from `prefers-color-scheme`,
  toggle persisted in `localStorage`. **WCAG-AA contrast verified in both modes.**
- **Pages:** *Graph/Explore* (query box → theme-aware SVG line chart + result table),
  *Targets* (scrape health), *Status* (build/runtime info).
- Chart: hand-rolled SVG, axis ticks, multi-series, colors pulled from CSS vars.

### `cmd/omni`
Wire config → open storage (WAL replay) → start scraper → query engine → API+web →
graceful shutdown (SIGINT/SIGTERM: stop scraper, fsync WAL, close).

## Testing & quality

- **TDD** (RED→GREEN), table-driven, mirroring per-package style.
- `-race` on `tsdb`, `scrape`, `api`, anything concurrent.
- Storage **conformance suite**; query/storage **parity test**.
- Quality gate before every commit: `gofmt -w .` → `go vet ./...` →
  `go test ./... -race` all green.
- **Verify with the running binary:** scrape own `/metrics`, run instant+range queries,
  `kill -9` → restart → confirm WAL recovery; full dark/light color/contrast audit.
- **Adversarial review** before integration, prompted to refute, targeting the known
  bug classes: cardinality DoS, WAL replay duplication, query/storage parity
  divergence, dark-mode contrast.

## Repo hygiene

Public repo: no internal hostnames/IPs/absolute home paths (derive from `$HOME`/cwd at
runtime); build artifacts, local config, `*.wal`/data dirs gitignored; no secrets;
commit identity `pod32g`.

## Milestone roadmap

**M1 (this):** vertical slice above.
**Deferred:** M2 on-disk blocks + retention/compaction; M3 deeper PromQL + histogram
quantiles; M4 recording/alerting rules; M5 service discovery; M6 remote-write /
federation; M7 full histograms/exemplars; later: auth/TLS, clustering.
