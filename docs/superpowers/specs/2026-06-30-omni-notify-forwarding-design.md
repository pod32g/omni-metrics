# omni-notify alert forwarding — design

**Date:** 2026-06-30
**Status:** approved (build)
**Depends on:** alerting engine (`internal/alerts/`, 2026-06-22)

## Goal

Forward omni-metrics alert state transitions to **omni-notify**
(`https://github.com/pod32g/omni-notify`), a generic event router that delivers
notifications. The alerting engine is detection-only; this adds the outbound
delivery seam so a firing/resolved alert becomes a notification.

omni-notify ingests via `POST /api/v1/events` (Bearer auth, returns `202`) using
its own event schema — **not** Alertmanager's webhook format. It dedups on
`fingerprint` and tracks a `firing`→`resolved` lifecycle per fingerprint.

## Decisions (locked)

- **Delivery: best-effort, in-memory + retry.** An async bounded queue with a
  background worker and bounded retry/backoff. If omni-metrics crashes while
  omni-notify is unreachable, queued notifications are lost; the SQLite alert
  history remains the durable audit record. No persistent outbox.
- **Filtering: global on/off + min-severity.** A single `alerting.notify` config
  block. Forward `firing` and `resolved` transitions (never `pending`), gated by
  an optional minimum severity. No per-rule controls.

## Topology (deployment fact, chizu)

- omni-notify runs as the `omni-notify` container, published on the host at
  `:8088` (internal `:8080`). omni-metrics is a separate container on its own
  Docker network, so it reaches omni-notify over the host LAN IP
  `http://192.168.68.34:8088` — the same way it already scrapes omni-identity at
  `192.168.68.34:8081`.
- The bearer token is injected as `${OMNI_NOTIFY_TOKEN}` (env → container),
  never committed, matching the `${OMNI_IDENTITY_TOKEN}` pattern.

## Architecture

The evaluator already computes every state transition in
`evaluator.EvaluateRule` with full context (rule, merged labels, annotations,
value, time). We surface those transitions to the `Service`, which forwards the
`firing`/`resolved` ones to an async dispatcher.

Rejected alternative: a history-feed poller reading the `Events(since)` cursor.
That is the right design for a *durable* outbox, but (a) we chose best-effort,
and (b) resolved instances are deleted, so their per-series labels are gone from
history — the inline seam produces richer events with less machinery.

### New package `internal/alerts/notify`

- **`Notification`** — neutral value the engine emits per forwarded transition:
  `RuleID, RuleName, Fingerprint, Status (firing|resolved), Severity,
  Labels, Annotations, Value, Time`.
- **`Config`** — `Enabled, URL, Token, Source, MinSeverity, Timeout, QueueSize,
  MaxRetries`.
- **`Client`** — maps `Notification` → omni-notify event JSON and POSTs to
  `<URL>/api/v1/events` with `Authorization: Bearer <Token>`. Classifies the
  response: `2xx` success; `4xx` permanent (drop + log, no retry); `5xx` /
  transport error retryable. Sets `Content-Type: application/json` and a real
  `Content-Length` (the alerting engine had a prior bug sending bodies without
  one — avoid repeating it).
- **`Dispatcher`** — owns a buffered channel (`QueueSize`) and one background
  worker. `Enqueue` is **non-blocking**: if the buffer is full it drops the
  incoming notification and increments a metric (best-effort). The worker applies
  the min-severity filter, then sends via the `Client` with bounded retry
  (`MaxRetries`, capped exponential backoff). `Start(ctx)` launches the worker;
  `Stop()` stops accepting and does a short best-effort drain. A `nil`
  dispatcher (feature disabled) is a no-op everywhere it is used.
- **Metrics** (own `WriteExposition`, folded into the engine's `/metrics`):
  - `omni_alerts_notify_sent_total` (counter)
  - `omni_alerts_notify_failed_total{reason}` — `permanent` (4xx), `giveup`
    (retryable failure after exhausting `MaxRetries`), `canceled` (aborted on
    shutdown)
  - `omni_alerts_notify_dropped_total{reason}` — `queue_full`
  - `omni_alerts_notify_filtered_total` — below min-severity
  - `omni_alerts_notify_retries_total`
  - `omni_alerts_notify_queue_depth` (gauge) — refreshed on both enqueue and
    dequeue

### Event mapping

```json
{
  "event_id":    "<ruleID>:<fingerprint>",
  "fingerprint": "<ruleID>:<fingerprint>",
  "type":        "alert",
  "source":      "<config.Source, default omni-metrics>",
  "status":      "firing | resolved",
  "severity":    "<mapped>",
  "title":       "<RuleName>",
  "summary":     "<annotations.summary, else RuleName>",
  "description": "<annotations.description, omitted if empty>",
  "timestamp":   "<transition time, RFC3339>",
  "labels":      { ...merged series+rule labels... },
  "annotations": { ...rule annotations... }
}
```

- `firing` and `resolved` for the same series share `fingerprint`, so omni-notify
  correlates the lifecycle and auto-resolves.
- The triggering sample value is added to `annotations.value` on `firing` events
  (a resolved transition has no meaningful value); a user-supplied `value`
  annotation is preserved.
- **Severity mapping:** omni-notify accepts `critical|error|warning|info|debug`.
  The rule severity is lowercased and passed through if in that set; anything
  unknown/empty maps to `warning`.
- **Min-severity filter:** ordering `critical > error > warning > info > debug`.
  A notification is sent only if its mapped severity is `>=` the configured
  minimum. Empty `min_severity` = send all.

### Evaluator change

Add `Changes []Transition` to `evaluator.Outcome` (keep the existing
`Transitions int` count so current call sites and the `api.EvalResult` response
are untouched). `Transition` carries `RuleID, RuleName, Severity, Fingerprint,
Prev, New, Value, Labels, Annotations, Time`. `EvaluateRule` appends one per
recorded transition (all states, including pending); `Service.evaluate` forwards
only `firing`/`resolved` to the dispatcher. Pending is intentionally not
notified.

### Wiring

- `alerts.Options` gains `Notify notify.Config`. `NewService` builds a
  `Dispatcher` when `Notify.Enabled`, threads it into the evaluate path, includes
  its exposition in `Collector()`, and starts/stops it in `Start`/`Stop`.
- `cmd/omni/main.go` maps `config.NotifyConfig` → `alerts.Options.Notify`.

### Config (`internal/config`)

```yaml
alerting:
  notify:
    enabled: true
    url: http://192.168.68.34:8088     # omni-notify base URL
    token: ${OMNI_NOTIFY_TOKEN}        # bearer; env-expanded, never committed
    source: omni-metrics               # default
    min_severity: ""                   # default: all; e.g. "warning"
    timeout: 5s                        # default per-request
    queue_size: 1024                   # default
    max_retries: 3                     # default
```

- `Enabled *bool`, defaults **false** (opt-in).
- Validation when enabled: `url` required and parseable (http/https, host, no
  embedded userinfo); `token` required; `min_severity` empty or a known level.
  Defaults applied for source/timeout/queue_size/max_retries.
- `token` flows through the existing `expandEnv` (a no-default `${VAR}` that is
  unset/empty is a fail-loud config error, per secure-scraping precedent).

## Testing (TDD, RED→GREEN; `-race` for the dispatcher)

- **Client:** payload shape + headers + bearer via `httptest`; `2xx` success,
  `4xx` permanent, `5xx`/transport retryable; Content-Length present.
- **Dispatcher:** enqueue→send through a fake client; retry on retryable failure
  up to `MaxRetries` then giveup; drop + metric on full queue; min-severity
  filter; only firing/resolved enqueued; graceful `Stop` drain; `-race` clean.
- **Severity:** map + filter table tests.
- **Config:** parse the `notify` block, defaults, validation (enabled requires
  url+token; bad min_severity rejected), env-expansion of the token.
- **Evaluator:** `Outcome.Changes` populated with the right transitions
  (firing, resolved present; values/labels correct).
- **Service:** a firing transition enqueues exactly one notification (inject a
  fake sink); disabled config enqueues nothing.

## Deploy & verify (chizu, after merge)

1. Add `OMNI_NOTIFY_TOKEN` to the omni-metrics container env (compose + host
   `.env`).
2. Add the `alerting.notify` block to the deployed `omni.yml`.
3. Redeploy via CI; fire a test rule (e.g. `vector(1) > 0`); confirm omni-notify
   receives the `firing` then `resolved` event (omni-notify UI / logs) and the
   `omni_alerts_notify_sent_total` metric increments.

## Docs

Add a "Notifications (omni-notify)" subsection to the alerting docs and README,
matching existing style; use a generic host placeholder in examples.

## Out of scope / deferred

- Persistent outbox / guaranteed delivery (chosen best-effort).
- Per-rule notify controls and routing (omni-notify owns routing).
- Templated notification bodies beyond the field mapping above.
