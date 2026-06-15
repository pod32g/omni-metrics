# omni-metrics

A Prometheus-shaped metrics system in Go — a single self-contained binary that
**scrapes** targets, **stores** samples in an in-memory head block backed by a
write-ahead log, evaluates a **PromQL** subset, and serves a
Prometheus-compatible **HTTP API** plus an embedded **web console** with dark and
light themes.

> Milestone 1: a thin but end-to-end vertical slice. See
> [the design spec](docs/superpowers/specs/2026-06-15-omni-metrics-m1-design.md)
> for scope and deferrals.

## Features

- **Pull-based scraping** of the Prometheus text exposition format, with injected
  `job`/`instance` labels and synthesized `up`, `scrape_duration_seconds`, and
  `scrape_samples_scraped` series.
- **JSON push ingestion** (`POST /api/v1/push`) for processes that can't be
  scraped: append semantics, per-source health, an optional bearer token, and a
  **Pushers** console view.
- **Custom TSDB**: in-memory head with an inverted index, plus a segmented,
  CRC-checked **write-ahead log** with crash recovery (survives `kill -9`).
- **PromQL subset**: instant & range queries, label matchers (`= != =~ !~`),
  aggregations (`sum avg min max count` with `by`/`without`), range functions
  (`rate irate increase`, `{sum,avg,min,max,count}_over_time`), and scalar/vector
  arithmetic and comparison.
- **Prometheus-compatible API**: `/api/v1/query`, `/query_range`, `/series`,
  `/labels`, `/label/<name>/values`, `/targets`, and self-instrumentation at
  `/metrics`.
- **Embedded web console**: query & graph (hand-rolled theme-aware SVG chart),
  scrape targets, and runtime status — **dark + light** from a single set of CSS
  design tokens, AA-contrast verified.
- A **cardinality guard** (head series cap + per-scrape sample limit) to bound
  runaway label cardinality.

## Quick start

```sh
go build -o omni ./cmd/omni

# Run with the default config: scrapes its own /metrics on 127.0.0.1:9090
./omni

# Or with a config and a persistent WAL directory
./omni -config examples/omni.yml -storage ./data
```

Then open the console at <http://127.0.0.1:9090/> and try a query such as
`rate(omni_http_requests_total[1m])`.

### Flags

| Flag | Description |
| --- | --- |
| `-config` | Path to a YAML config (optional; defaults to a self-scrape setup) |
| `-listen` | Override the web/API listen address |
| `-storage` | Override the WAL directory (empty = in-memory only, no durability) |

## Configuration

```yaml
global:
  scrape_interval: 15s
  scrape_timeout: 10s
storage:
  path: ./data        # WAL directory; omit for in-memory only
  retention: 6h       # head retention window
web:
  listen: 127.0.0.1:9090
scrape_configs:
  - job_name: node
    scrape_interval: 30s
    static_configs:
      - targets: [node-01:9100, node-02:9100]
```

Durations accept `s m h d w y` units (e.g. `15s`, `90s`, `2h`, `7d`).

## API

Responses use Prometheus' envelope: `{"status":"success","data":{...}}` or
`{"status":"error","errorType":"...","error":"..."}`.

```sh
curl 'http://127.0.0.1:9090/api/v1/query?query=up'
curl 'http://127.0.0.1:9090/api/v1/query_range?query=rate(omni_http_requests_total[1m])&start=<unix>&end=<unix>&step=15'
curl 'http://127.0.0.1:9090/api/v1/targets'
```

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
pushed counters. Per series, supply either `value` (one sample at receive time) or
`samples: [{"timestamp_ms":…, "value":…}]`. `value` accepts a number or the
strings `"NaN"`, `"+Inf"`, `"-Inf"`. The server injects `job`/`instance` and a
client cannot override `__name__`/`job`/`instance`. Push-source health is shown on
the **Pushers** console page and at `GET /api/v1/push/sources`. Configure limits
and an optional bearer token via the `push:` config block.

## Architecture

```
 scrape targets        ┌──────────── omni server ────────────┐
 (HTTP /metrics) ─────►│ Scraper ─► Storage(TSDB) ◄─ Query   │
                       │              │  ▲ WAL      engine    │
                       │            disk│           (PromQL)  │
                       │                └── HTTP API ─────────┘
                       │                 Web UI (embed.FS)    │
                       └──────────────────────────────────────┘
```

| Package | Responsibility |
| --- | --- |
| `internal/model` | Labels (sorted, hashed), samples, matchers |
| `internal/exposition` | Text exposition-format parser |
| `internal/tsdb` | Head block + WAL; `Storage`/`Appender`/`Querier` interfaces + conformance suite |
| `internal/promql` | Lexer, parser, evaluator |
| `internal/scrape` | Pull manager + target health |
| `internal/push` | JSON push ingestion + per-source health |
| `internal/config` | YAML config + validation |
| `internal/api` | HTTP handlers + self-instrumentation |
| `web` | Embedded console (HTML/CSS/JS) |
| `cmd/omni` | Wiring + graceful shutdown |

## Development

Test-driven throughout. The quality gate before every commit:

```sh
gofmt -w .
go vet ./...
go test ./... -race
```

## Roadmap (deferred from M1)

M2 on-disk blocks + retention/compaction · M3 deeper PromQL + histogram quantiles ·
M4 recording/alerting rules · M5 service discovery · M6 remote-write/federation ·
later: auth/TLS, clustering.
