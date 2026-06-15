# omni-metrics — Grafana compatibility design

**Date:** 2026-06-15
**Status:** In progress (goal: "make this fully grafana compatible")
**Module:** `github.com/pod32g/omni-metrics`

## Goal

Make omni-metrics a drop-in **Prometheus data source** for Grafana: Grafana connects
with the default settings, the Explore/query builder browses metrics and labels, and
**real community dashboards render without query errors**. Verified end-to-end against
an actual Grafana container.

## What Grafana's Prometheus data source requires

### HTTP API (the connection + browsing surface)
- **Queries via POST.** Grafana defaults to `POST` (form-encoded) for
  `/api/v1/query` and `/api/v1/query_range` (and sometimes `/api/v1/series`). omni
  currently registers `GET` only, so POST falls through to the SPA and returns
  `200 text/html`. **Fix:** accept both GET and POST on all read endpoints.
- **Clean `/api/**` errors.** Unknown `/api/...` paths and wrong methods must return
  the Prometheus JSON error envelope with `404`/`405`, not the console HTML. **Fix:**
  scope the SPA fallback to non-`/api` paths.
- **`GET /api/v1/status/buildinfo`** — Grafana probes this to detect the Prometheus
  version and enable features. Return a minimal Prometheus-shaped payload.
- **`GET /api/v1/metadata`** — metric type/help for the UI. Return JSON synthesized
  from scrape metadata (empty `{}` is acceptable but we can do better).
- **`match[]` on `/api/v1/label/{name}/values` and `/api/v1/labels`** — Grafana's
  `label_values(metric, label)` template uses it. Currently ignored.
- Envelope/format already match (`{status,data}`, `[unix_seconds,"value"]`).

### PromQL surface (what dashboards actually use)
The engine is currently a thin subset. To run real dashboards we add:

- **Selectors:** `__name__` matchers and regex on `__name__`; `offset`; `@ <ts>`
  / `@ start()` / `@ end()`; **subqueries** `expr[range:resolution]`.
- **Binary ops:** vector matching `on(...)` / `ignoring(...)` with
  `group_left(...)` / `group_right(...)`; set operators `and` / `or` / `unless`;
  the `bool` modifier on comparisons; scalar-scalar comparisons with `bool`.
- **Aggregations (add):** `topk`, `bottomk`, `quantile`, `stddev`, `stdvar`,
  `group`, `count_values` (and parameterized `topk(k, v)` form).
- **Functions (add):**
  - label: `label_replace`, `label_join`.
  - math: `abs`, `ceil`, `floor`, `round`, `exp`, `ln`, `log2`, `log10`, `sqrt`,
    `sgn`, `clamp`, `clamp_max`, `clamp_min`.
  - shape: `scalar`, `vector`, `sort`, `sort_desc`, `absent`, `absent_over_time`.
  - time: `time`, `timestamp`, `day_of_month`, `day_of_week`, `day_of_year`,
    `days_in_month`, `hour`, `minute`, `month`, `year`.
  - range: `delta`, `idelta`, `deriv`, `predict_linear`, `changes`, `resets`,
    `stddev_over_time`, `stdvar_over_time`, `last_over_time`, `present_over_time`,
    `quantile_over_time`, `histogram_quantile`.

Deliberately deferred (rare in dashboards, large to build): native histograms and
their `histogram_*` functions, `holt_winters`, `@`-with-step-aligned subquery
edge cases, exemplars, remote_read. Documented as deferrals.

## Approach

- **Storage/`model` unchanged.** All work is in `internal/promql` (parser, engine,
  functions) and `internal/api` (routes). The TSDB already stores histogram bucket
  series as ordinary series, so `histogram_quantile` is computed from `le`-labelled
  bucket vectors at query time — no storage change.
- **Architecture:** extend the AST with `Offset`/`At` on selectors, `SubqueryExpr`,
  `VectorMatching` on `BinaryExpr`, a `Param` on `AggregateExpr` (for topk/quantile),
  and multi-arg `Call`. The evaluator gains a generic instant-vector function table
  (vector→vector), a scalar-arg mechanism, vector matching with group_left/right,
  set-op handling, and time-shift for offset/@/subqueries.
- **Conformance:** a broad table-test corpus of `query → expected` cases mirroring
  Prometheus semantics, plus the existing query/storage parity test.

## Testing & verification

- **TDD** throughout; table-driven; `-race` where concurrent. Quality gate
  (`gofmt`/`vet`/`test -race`) green before every commit.
- **Real-Grafana E2E (the compatibility proof):** run `grafana/grafana` in Docker
  with omni bound to `0.0.0.0`; provision a Prometheus data source at
  `host.docker.internal`; assert datasource **health is green**; run a spread of
  queries via Grafana's `/api/ds/query`; import a community dashboard and confirm
  panels return data (no query errors). Capture evidence.
- **Adversarial review** of PromQL correctness vs Prometheus semantics before ship.

## Adversarial review — outcome

A 5-dimension adversarial review (PromQL vs Prometheus semantics + API) ran after
implementation. Confirmed findings, all fixed test-first:

- **`clamp(v,min,max)` with `min>max`** now returns an empty vector (was emitting
  the min for every series).
- **`sort`/`sort_desc`** order finite values correctly and push `NaN` to the end
  (the old comparator let `NaN` freeze ordering).
- **`histogram_quantile`** repairs non-monotonic cumulative buckets (scrape-race
  artifacts) before interpolating, as Prometheus does.
- **`timestamp()`** reports the sample's own timestamp rather than the eval time
  (selectInstant carries it; the instant result is normalized to eval time).
- **`/metrics` and the health probes** return `405` + `Allow` on a wrong method
  instead of falling through to the SPA HTML.

Spot-checked and confirmed correct (review noise): `group_right`, colon
recording-rule names coexisting with subquery `:`, `min`/`max`, set ops, topk/
bottomk, count_values, subqueries, the `bool` modifier.

**Known approximation (deferred):** `rate`/`increase`/`irate` use the sampled
window rather than Prometheus' extrapolation to the range boundaries, so their
values differ slightly from Prometheus. Queries still return valid rates and
dashboards render; full extrapolation is a later refinement.

## Repo hygiene

Public repo; commit identity `pod32g` (noreply), no `Co-Authored-By` trailer.
