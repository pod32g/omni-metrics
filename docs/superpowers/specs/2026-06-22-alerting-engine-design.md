# M4: Alerting Engine — Design

Status: approved 2026-06-22. Milestone M4 (alerting; recording rules deferred).

## Objective

A production-ready, built-in alerting engine for omni-metrics that **detects alert
conditions and manages alert state**. It does **not** send notifications — delivery
(Discord/Slack/Email/Telegram/Webhooks), routing, escalation, silences, and
maintenance windows belong to a future, separate service (Omni-Notify). The engine
exposes alert events so Omni-Notify can consume them later.

The engine is **provider-agnostic**: it evaluates PromQL against any
Prometheus-compatible datasource over HTTP. Omni-Metrics itself is just one possible
datasource — no code path assumes the datasource is the local instance.

## Decisions (locked)

- **Persistence:** pure-Go SQLite via `modernc.org/sqlite` (no cgo, keeps the static
  distroless build). Behind a `Store` interface with a conformance suite, so the
  backend stays swappable (data-safety rule).
- **Datasources:** config-seeded **and** API-managed. YAML `alerting.datasources`
  entries are upserted on boot (source=`config`); the API does full CRUD on
  `api`-sourced datasources. Config/builtin datasources are **read-only via the API**
  (a runtime edit would be overwritten on next boot) — the API returns 409 for them.
- **Event exposure:** pull-based REST. `GET /api/v1/alerts/active` + `GET
  /api/v1/alerts/history`, plus a dedicated machine feed `GET /api/v1/alerts/events`
  (append-only transition stream, id cursor). No outbound connections.

## Architecture & packages

Self-contained under `internal/alerts/`. Coupled to the rest of omni only at two
seams: it queries datasources over HTTP (never the local tsdb/engine directly), and
`cmd/omni` mounts its routes + metrics.

```
internal/alerts/
  models/      Rule, Datasource, Instance, HistoryEntry, State, Severity, Auth
  datasource/  Datasource interface + Prometheus HTTP client (instant query)
  state/       pure state machine (OK->PENDING->FIRING->RESOLVED), no I/O
  storage/     Store interface + modernc-sqlite impl + conformance suite
  evaluator/   evaluate one rule -> apply state machine -> persist
  scheduler/   per-rule goroutines, reconciliation, graceful shutdown
  metrics/     alert-engine self-metrics (text exposition sub-collector)
  api/         HTTP handlers for /api/v1/alerts* and /api/v1/datasources*
  alerts.go    Service: wires store+scheduler+datasources; Handler() + Metrics()
```

The whole engine is gated by `alerting.enabled` (**default on**, with the builtin
`local` datasource auto-registered). When disabled, no routes/metrics/scheduler are
wired and no SQLite file is opened.

## Data model (SQLite)

WAL mode, `busy_timeout=5000`, foreign keys on. Writes serialized through the Store
so parallel rule goroutines never hit "database is locked".

- **datasources**: `id` (text, ULID-ish/uuid), `name` (unique), `type`, `base_url`,
  `auth_type` (none|bearer|basic), `credentials`, `basic_user`, `basic_pass`,
  `headers` (JSON), `timeout_ms`, `enabled`, `source` (builtin|config|api),
  `created_at`, `updated_at`.
- **alert_rules**: `id`, `name`, `description`, `datasource_id`, `promql`,
  `eval_interval_s`, `for_s`, `severity`, `labels` (JSON), `annotations` (JSON),
  `enabled`, `created_at`, `updated_at`.
- **alert_instances** (active set): `id`, `rule_id`, `fingerprint`, `state`,
  `current_value`, `active_at`, `started_at`, `updated_at`, `resolved_at`,
  `labels` (JSON), `annotations` (JSON). Unique `(rule_id, fingerprint)`.
- **alert_history** (append-only, never auto-deleted): `id` (autoincrement = event
  cursor), `rule_id`, `fingerprint`, `prev_state`, `new_state`, `timestamp`,
  `value`, `reason`.

A `schema_version` table tracks migrations (append-only, idempotent on boot).

## Datasources

`Datasource` interface:

```go
type Datasource interface {
    Query(ctx context.Context, promql string, ts time.Time) (Result, error)
}
```

The Prometheus impl issues an instant query to `{base_url}/api/v1/query`, applies
bearer/basic auth and custom headers, and uses the per-datasource timeout. `Result`
distinguishes vector / scalar / empty. Auth/header/timeout config is built into a
reusable `*http.Client` per datasource.

Built-in `local` datasource (source=builtin) points at omni's own listen address and
is the default datasource when a rule omits one. `POST /api/v1/datasources/{id}/test`
runs a trivial connectivity query (`vector(1)` or `/-/healthy`-style probe).

## Evaluation & state machine

PromQL is evaluated **exactly as written** by the datasource — the comparison lives
in the expression (`up == 0`, `... > 5`). The returned instant vector **is** the set
of firing elements; **each result series is one alert instance**, keyed by the
fingerprint of its label set (multi-series rules supported). Empty vector → no active
elements. A scalar result → a single labelless element.

Per evaluation, per series:

- present, untracked → **PENDING** (`active_at = now`); if `for == 0` → **FIRING**.
- present, tracked, `now - active_at >= for` → **FIRING**.
- tracked but absent this eval → **RESOLVED**, then removed from the active set.

**Correctness rules (adversarial-review-minded):**

- A query / HTTP / auth / timeout / invalid-PromQL failure **does not resolve**
  alerts. Prior state is held; the failure surfaces via the evaluation `Outcome`
  → `omni_alert_evaluation_failures_total{reason}` + a structured log. Failures
  are **not** written to `alert_history` — history stays strictly a state-
  transition log (the clean event feed Omni-Notify consumes); operational
  failure telemetry lives in metrics/logs. Resolving on a transient outage would
  be a false "all clear".
- **Restart recovery:** the active set is loaded from SQLite on boot; `active_at`
  timers resume, so PENDING windows and FIRING state survive restarts.
- **Per-rule active-instance cap** (default 1000) guards against a high-cardinality
  result exploding the instance table (mirrors omni's existing cardinality-DoS
  guard). Over-cap: evaluation records a failure reason `instance_cap` and does not
  create further instances that tick.
- The rule's `labels`/`annotations` are merged onto each instance (rule labels
  override series labels on conflict, matching Prometheus).

State transitions are emitted as history rows (the consumable event). The state
machine in `state/` is pure (inputs: current state, condition-true, `active_at`,
`for`, `now`) and exhaustively table-tested.

## Scheduler

**One goroutine per enabled rule**, each with its own ticker at the rule's interval
→ independent intervals, and a slow datasource only blocks its own rule. Each tick
evaluates under a context timeout (the datasource timeout). On rule
create/update/enable/disable the scheduler **reconciles** (start/stop/restart the
goroutine). Graceful shutdown via context cancel + WaitGroup. Evaluation is
idempotent given persisted state → retry-safe. `omni_alert_scheduler_duration_seconds`
records the last evaluation duration.

## REST API

`{status,data,error}` envelope, matching the existing API. Mounted by `cmd/omni`.

Rules:
- `GET    /api/v1/alerts` — list rules (+ state summary)
- `POST   /api/v1/alerts` — create
- `GET    /api/v1/alerts/{id}` — rule + its instances
- `PUT    /api/v1/alerts/{id}` — update
- `DELETE /api/v1/alerts/{id}` — delete (drops active instances; history retained)
- `POST   /api/v1/alerts/{id}/enable`
- `POST   /api/v1/alerts/{id}/disable`
- `POST   /api/v1/alerts/{id}/evaluate` — evaluate this rule now (sync)
- `POST   /api/v1/alerts/evaluate` — evaluate all enabled rules now (sync)

Views & events:
- `GET /api/v1/alerts/active` — active (pending+firing) instances
- `GET /api/v1/alerts/history?rule_id=&since=&limit=` — human transition log
- `GET /api/v1/alerts/events?since=<cursor>&limit=` — machine feed (append-only,
  id-cursor) for Omni-Notify; same backing table as history

Datasources:
- `GET/POST /api/v1/datasources`, `GET/PUT/DELETE /api/v1/datasources/{id}`,
  `POST /api/v1/datasources/{id}/test`

Go 1.22 ServeMux precedence makes `/alerts/active|history|events|evaluate` win over
`/alerts/{id}` cleanly; method-specific patterns return 405 with `Allow`.

## UI

Extends the existing vanilla-JS hash router + dark/light styles. New nav: **Alerts**
(rules: state, severity, last eval, last transition, duration), **Active** (status,
severity, rule, current value, started-at, duration), **History**. **Rule editor**
(PromQL textarea, datasource picker, severity, eval interval, for-duration,
labels/annotations key-value editors, enable toggle). A small **Datasources** view
for API-managed datasources. Matches `app.js` / `styles.css` conventions.

## Metrics

Exposed at `/metrics` via a registered sub-collector (the API `/metrics` handler
writes SelfMetrics then any registered collectors — no tight coupling):

- `omni_alert_rules_total` (gauge)
- `omni_alerts_active` (gauge), `omni_alerts_pending` (gauge)
- `omni_alert_evaluations_total{result}` (counter)
- `omni_alert_evaluation_failures_total{reason}` (counter)
- `omni_alert_state_transitions_total` (counter)
- `omni_alert_evaluation_duration_seconds_sum` / `_count` (counters) +
  `omni_alert_scheduler_duration_seconds` (gauge, last tick)

## Logging

Structured `key=value` logs for: state transitions (the signal), datasource
failures, scheduler start/stop/reconcile. Routine still-OK evaluations are not
logged.

## Config

New `alerting` block:

```yaml
alerting:
  enabled: true
  storage_path: ""          # sqlite file; default <storage.path>/alerts.db, or temp when in-memory
  default_datasource: local
  datasources:
    - name: local           # builtin is auto-added if omitted; explicit entry overrides URL
      type: prometheus
      url: http://127.0.0.1:9090
      timeout: 30s
      authorization: { credentials: "${TOKEN}" }   # reuses existing auth shapes
      headers: { X-Scope-OrgID: "tenant-a" }
      enabled: true
```

Reuses the existing `Authorization` / `BasicAuth` env-expansion + `_file` machinery.

## Testing (TDD, RED→GREEN)

- **state/**: table-driven — every transition incl. `for==0`, restart resume,
  failure-holds-state, resolve-then-refire.
- **datasource/**: `httptest` — vector/scalar/empty/401/timeout/malformed/headers/auth.
- **storage/**: Store conformance suite — CRUD, unique constraints, append-only
  history, cursor pagination, restart reload.
- **evaluator/**: multi-series, instance cap, failure handling, label merge.
- **scheduler/** (`-race`): independent intervals, reconcile on rule change, graceful
  shutdown, slow-datasource isolation.
- **api/**: every endpoint incl. 404/405/409/validation.
- **metrics/**: exposition format + counter/gauge correctness.

## Known limitations / deferrals

Surfaced by the adversarial review (2026-06-22); triaged as acceptable for this
milestone:

- **History growth under label churn.** `alert_history` is append-only and never
  auto-pruned (by design — it is the audit/event log). A rule whose result series
  fully rotate every evaluation (high-cardinality `instance`/`pod` labels) writes a
  RESOLVED transition per vanished series per tick, so history grows unbounded even
  though the per-rule *active-instance* cap holds. The cap bounds the active set and
  memory, not history. **Deferred:** a history-retention/pruning policy (e.g.
  `alerting.history_retention`) — a follow-up feature, not a correctness bug.
- **`POST /api/v1/alerts/evaluate` (all) reports only a count.** Per-rule failures
  are metered (`omni_alert_evaluation_failures_total`) and logged, but the
  synchronous bulk endpoint returns `{"evaluated": N}` without a per-rule failure
  breakdown. Single-rule `/{id}/evaluate` does surface its error. **Deferred:**
  richer bulk-evaluation reporting.
- **In-memory store throughput.** The default config-less run uses an in-memory
  SQLite store with `SetMaxOpenConns(1)`; access is serialized (safe, no deadlock —
  verified under `-race` with concurrent goroutines) but slower than a file store.
  Production deployments set `storage.path`, which puts `alerts.db` on disk in WAL
  mode. No change needed.

## Non-goals (explicit)

No notification delivery, routing, escalation, silences, maintenance windows, HA
clustering, or recording rules. The engine stops at evaluation + state + history +
event exposure.
