# M1 adversarial review — findings & resolutions

**Date:** 2026-06-15
**Method:** A multi-agent adversarial review fanned four refute-first reviewers across
the subsystems (tsdb, promql, ingest, api/web/config); each finding was then handed to
an independent verifier prompted to refute it. 17 raw findings → **15 confirmed**. All
15 were fixed test-first (a failing test reproducing the defect, then the fix).

## High

| # | Finding | Resolution |
|---|---------|-----------|
| 1 | **Retention truncate never reclaimed emptied series** — churned label values permanently consumed the cardinality budget (cardinality-DoS). | `head.truncate` now GCs any series left empty, removing it from the series map, hash chain, and inverted index (`removeSeriesLocked`). |
| 2 | **Scrape ignored `Append` errors** — a full head silently dropped samples while reporting `up=1`. | `scrapeOnce` checks each `Append`; a cardinality-cap rejection fails the scrape (`up=0`), rolls back, and is recorded in target health. `scrape_samples_scraped` now reports stored count. |
| 3 | **Target could forge a reserved series** via a smuggled `{__name__="up"}` label. | The exposition parser drops any explicit `__name__` label inside braces; the positional metric name always wins. |
| 4 | **Unbounded range-query step count** — a tiny `step` over a huge range was a CPU/memory DoS. | `RangeQuery` caps resolution at 11,000 points and rejects beyond it. |
| 5 | **`parseTimeParam` accepted `Inf`/`NaN`/overflow** — corrupted timestamps (Inf→MaxInt64, NaN→epoch). | Non-finite time params are now rejected with HTTP 400. |

## Medium

| # | Finding | Resolution |
|---|---------|-----------|
| 6 | **Duplicate-timestamp samples stored** — broke the one-value-per-timestamp invariant and WAL sample-replay idempotency. | `appendSample` drops a sample whose timestamp already exists (keeps the first); replay is now idempotent for samples too. |
| 7 | **Range loop `int64` overflow** near MaxInt64 bounds. | Loop iterates by integer counter `start + i*step`; `end<start` and overflowing spans are rejected. |
| 8 | **Scalar↔scalar comparison** silently returned the LHS. | Rejected with an explicit error (the `bool` modifier is deferred). |
| 9 | **Scrape swallowed exposition parse errors** — a target serving garbage looked healthy. | Parse warnings are surfaced into `TargetHealth.LastError` (good series still ingested; `up` stays 1 when reachable). |
| 10 | **16 MiB body silently truncated** mid-line. | Read one byte past the limit; an over-size body fails the scrape with an explicit error. |
| 11 | **`/series` swallowed invalid `start`/`end`** → wrong window, HTTP 200. | Parse errors are surfaced as HTTP 400, matching the sibling handlers. |
| 12 | **`query_range` accepted `start > end`** → empty success. | Rejected with HTTP 400 (via the engine's `end<start` guard). |

## Low

| # | Finding | Resolution |
|---|---------|-----------|
| 13 | **WAL segment create not durable** (no directory fsync). | `openSegment` fsyncs the WAL directory after creating a segment. |
| 14 | **Mid-list WAL corruption** left later segments → inconsistent recovery across reopens. | On corruption, later segments are quarantined (removed) so recovery is deterministic. |
| 15 | **Unary minus bound tighter than `^`** — `-2^2` gave `4` instead of `-4`. | Unary minus now parses its operand at pow precedence: `-2^2 = -(2^2)`. |

All fixes verified: `gofmt`/`go vet` clean and `go test ./... -race` green, plus a live
binary smoke test confirming the new HTTP guards (NaN→400, excessive range→400,
`end<start`→400).
