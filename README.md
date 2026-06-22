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
- **PromQL**: instant & range queries; label matchers (`= != =~ !~`);
  aggregations (`sum avg min max count topk bottomk quantile stddev stdvar group
  count_values` with `by`/`without`); vector matching (`on`/`ignoring` +
  `group_left`/`group_right`); set ops (`and`/`or`/`unless`); the `bool` modifier;
  `offset`/`@` and subqueries; and a broad function library including
  `rate`/`irate`/`increase`/`delta`/`deriv`/`predict_linear`, `histogram_quantile`,
  `label_replace`/`label_join`, math, and time functions.
- **Grafana-compatible**: works as a drop-in **Prometheus data source** — GET+POST
  query endpoints, `status/buildinfo`, `metadata`, `match[]` label filtering, and a
  Prometheus JSON envelope. (See [Grafana](#grafana).)
- **Prometheus-compatible API**: `/api/v1/query`, `/query_range`, `/series`,
  `/labels`, `/label/<name>/values`, `/targets`, `/status/buildinfo`, `/metadata`,
  and self-instrumentation at `/metrics`.
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

### Secure scraping

Scrape jobs can authenticate and use TLS with a Prometheus-shaped surface:

```yaml
scrape_configs:
  - job_name: omni-identity
    scheme: https                       # http (default) | https
    authorization:                      # bearer auth
      type: Bearer                      # header type; default Bearer
      credentials: ${OMNI_IDENTITY_TOKEN}
      # credentials_file: /run/secrets/token
    # basic_auth:                       # mutually exclusive with authorization
    #   username: scraper
    #   password_file: /run/secrets/pw
    tls_config:
      ca_file: /etc/ssl/ca.pem          # custom CA; omit for system roots
      cert_file: /etc/ssl/client.pem    # mTLS: cert + key together
      key_file:  /etc/ssl/client.key
      server_name: omni-identity.internal
      insecure_skip_verify: false       # last resort; prefer ca_file
    static_configs:
      - targets: [omni-identity:8081]
```

Secrets reach the scraper three ways, so they need never be committed:

- **Inline** — a literal value (fine for non-secret fields).
- **`${ENV}` expansion** — `${VAR}` or `${VAR:-default}` in any credential or
  file-path field. A `${VAR}` that is referenced but unset (with no default) is a
  **load error** — the scraper fails loudly rather than authenticating with an
  empty token.
- **`<field>_file`** — read the secret from a file (Docker secrets, mounted
  volumes). A field and its `_file` twin set together is a config error.

`authorization` and `basic_auth` are mutually exclusive, as are `cert_file`
without `key_file`. Secrets and certificates are resolved once at startup;
rotating a token or certificate requires a restart.

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

## Grafana

omni-metrics is a drop-in **Prometheus data source** for Grafana. Add a data source
of type **Prometheus** with the URL pointing at omni (e.g. `http://localhost:9090`);
Grafana's "Save & test" succeeds, the query builder browses metrics/labels, and
dashboards built for Prometheus work. Verified end-to-end against Grafana with the
real *Node Exporter Full* dashboard — all 284 of its panel queries are accepted by
omni's PromQL engine.

Make sure omni is reachable from Grafana: bind a routable address with
`-listen 0.0.0.0:9090` when Grafana runs elsewhere (a container reaches the host via
`http://host.docker.internal:9090`). The query endpoints accept Grafana's default
`POST` method, so no data-source tweaks are needed.

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

M2 on-disk blocks + retention/compaction · M4 recording/alerting rules ·
M5 service discovery · M6 remote-write/federation · later: auth/TLS, clustering,
native histograms.

(M3 — deeper PromQL + `histogram_quantile` — is largely delivered as part of
Grafana compatibility.)
