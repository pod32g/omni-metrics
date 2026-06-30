# omni-notify alert forwarding — implementation plan

Spec: `docs/superpowers/specs/2026-06-30-omni-notify-forwarding-design.md`.
TDD throughout (RED→GREEN). Quality gate before each commit:
`gofmt -w .` → `go vet ./...` → `go test ./...` (`-race` for the dispatcher).

## Step 1 — `notify` package: severity + Config
- `internal/alerts/notify/severity.go`: `Severity` levels, `MapSeverity(string)`,
  `meets(min, sev)` ordering. Tests first.
- `internal/alerts/notify/notify.go`: `Notification`, `Config`, defaults.

## Step 2 — `notify` Client
- `client.go`: `Client.Send(ctx, Notification) error` → POST `/api/v1/events`,
  bearer, Content-Type + Content-Length. Typed errors: `permanentError` (4xx)
  vs retryable (5xx/transport). `httptest` tests: payload/headers/status classes.

## Step 3 — `notify` Dispatcher (+ metrics)
- `metrics.go`: counters/gauge + `WriteExposition`.
- `dispatcher.go`: bounded chan + worker; non-blocking `Enqueue`; min-severity
  filter; bounded retry/backoff; `Start`/`Stop` drain. `-race` tests with a fake
  client (success, retry-then-giveup, drop-on-full, filter, firing/resolved-only,
  graceful stop).

## Step 4 — Evaluator surfaces transitions
- Add `Transition` type + `Outcome.Changes []Transition`; populate in
  `EvaluateRule` (keep `Transitions` count). Update evaluator tests for `Changes`.

## Step 5 — Service wiring
- `alerts.Options.Notify notify.Config`; build dispatcher when enabled; forward
  firing/resolved `Changes` in `evaluate`; include exposition in `Collector`;
  start/stop in `Start`/`Stop`. Test: firing enqueues one (fake sink); disabled
  enqueues none.

## Step 6 — Config
- `NotifyConfig` under `AlertingConfig`; defaults + validation; env-expansion of
  token. Tests for parse/defaults/validation.

## Step 7 — main wiring
- Map `config.NotifyConfig` → `alerts.Options.Notify` in `cmd/omni/main.go`.

## Step 8 — Quality gate + adversarial review
- Full `go vet` + `go test ./... -race`; reviewer prompted to refute; triage.

## Step 9 — Docs + config
- README + alerting docs "Notifications" section; add `alerting.notify` to root
  `omni.yml` (real URL, `${OMNI_NOTIFY_TOKEN}`); generic example in `examples/`.

## Step 10 — Deploy & live verify (chizu)
- `OMNI_NOTIFY_TOKEN` into compose/.env; redeploy; fire a test rule; confirm
  omni-notify receives firing+resolved and `omni_alerts_notify_sent_total` rises.
