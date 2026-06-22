# Alerting Engine Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A built-in, provider-agnostic alerting engine that stores PromQL alert rules, evaluates them on a per-rule schedule against Prometheus-compatible datasources, runs an OK→PENDING→FIRING→RESOLVED state machine, persists every transition, and exposes REST + UI + metrics — stopping short of notification delivery.

**Architecture:** Self-contained `internal/alerts/` packages layered bottom-up (models → datasource → state → storage → evaluator → scheduler → metrics → api → Service). Persistence is `modernc.org/sqlite` behind a `Store` interface with a conformance suite. Evaluation goes through a `Datasource` HTTP abstraction; the local omni is just a builtin datasource. `cmd/omni` constructs the `alerts.Service`, mounts its `http.Handler` and metrics collector, and runs its scheduler.

**Tech Stack:** Go stdlib (`net/http`, `database/sql`, `net/http/httptest`), `modernc.org/sqlite`, `github.com/google/uuid`, `gopkg.in/yaml.v3`, vanilla JS UI.

Spec: `docs/superpowers/specs/2026-06-22-alerting-engine-design.md`

---

## File Structure

- `internal/alerts/models/models.go` — `State`, `Severity`, `AuthType`, `Datasource`, `Rule`, `Instance`, `HistoryEntry`, `Result`/`Sample`, `Fingerprint`.
- `internal/alerts/datasource/datasource.go` — `Datasource` interface, `Prometheus` HTTP client, `New` from a `models.Datasource`.
- `internal/alerts/state/state.go` — pure `Next(...)` transition fn + `Decision` type.
- `internal/alerts/storage/store.go` — `Store` interface.
- `internal/alerts/storage/sqlite.go` — modernc-sqlite `Store` impl + migrations.
- `internal/alerts/storage/conformance.go` — `RunConformance(t, factory)` shared suite.
- `internal/alerts/evaluator/evaluator.go` — `Evaluator.EvaluateRule(ctx, rule) Outcome`.
- `internal/alerts/scheduler/scheduler.go` — per-rule goroutine scheduler + reconcile.
- `internal/alerts/metrics/metrics.go` — `Metrics` collector + `WriteExposition`.
- `internal/alerts/api/api.go` — alert + datasource HTTP handlers, `{status,data,error}` envelope.
- `internal/alerts/alerts.go` — `Service` assembling everything; `Handler()`, `Collector()`, `Start/Stop`.
- `internal/config/config.go` — add `Alerting` block (+ datasource config, reuse auth shapes).
- `internal/api/api.go` — accept optional alert handler + extra metrics collector to mount.
- `cmd/omni/main.go` — build datasources from config, open store, start Service, wire API.
- `web/assets/{index.html,app.js,styles.css}` — Alerts / Active / History / Rule editor / Datasources views.
- `examples/omni.yml`, `README.md` — config docs.

---

## Task 1: Core models

**Files:** Create `internal/alerts/models/models.go`, `internal/alerts/models/models_test.go`

- [ ] **Step 1: Failing test** — `models_test.go`: state string round-trip, severity validation, label-set `Fingerprint` stability (order-independent), `Result` kind classification (vector/scalar/empty).

```go
func TestStateString(t *testing.T) {
	for s, want := range map[models.State]string{
		models.StateOK: "ok", models.StatePending: "pending",
		models.StateFiring: "firing", models.StateResolved: "resolved",
	} {
		if s.String() != want { t.Errorf("%d => %q want %q", s, s.String(), want) }
	}
}
func TestFingerprintOrderIndependent(t *testing.T) {
	a := models.Fingerprint(map[string]string{"a":"1","b":"2"})
	b := models.Fingerprint(map[string]string{"b":"2","a":"1"})
	if a != b { t.Fatalf("fingerprint not stable: %s vs %s", a, b) }
	if a == models.Fingerprint(map[string]string{"a":"1"}) { t.Fatal("collision") }
}
```

- [ ] **Step 2:** Run, expect FAIL (package missing).
- [ ] **Step 3:** Implement. Key types:

```go
type State int
const (StateOK State = iota; StatePending; StateFiring; StateResolved)
func (s State) String() string // ok/pending/firing/resolved
func ParseState(string) (State, error)

type Severity string // free-form; helper Valid() != ""
type AuthType string // "none","bearer","basic"

type Datasource struct {
	ID, Name, Type, BaseURL string
	AuthType AuthType; Credentials, BasicUser, BasicPass string
	Headers map[string]string; TimeoutMS int; Enabled bool
	Source string // builtin|config|api
	CreatedAt, UpdatedAt time.Time
}
type Rule struct {
	ID, Name, Description, DatasourceID, PromQL string
	EvalIntervalS, ForS int; Severity Severity
	Labels, Annotations map[string]string; Enabled bool
	CreatedAt, UpdatedAt time.Time
}
type Instance struct {
	ID, RuleID, Fingerprint string; State State; CurrentValue float64
	ActiveAt, StartedAt, UpdatedAt time.Time; ResolvedAt *time.Time
	Labels, Annotations map[string]string
}
type HistoryEntry struct {
	ID int64; RuleID, Fingerprint string; Prev, New State
	Timestamp time.Time; Value float64; Reason string
}
type Sample struct{ Labels map[string]string; Value float64 }
type ResultKind int // KindEmpty, KindVector, KindScalar
type Result struct{ Kind ResultKind; Samples []Sample }
func Fingerprint(labels map[string]string) string // sorted-key fnv hash, hex
```

- [ ] **Step 4:** Run, expect PASS.
- [ ] **Step 5:** Commit `feat(alerts): core domain models`.

---

## Task 2: Datasource abstraction + Prometheus HTTP client

**Files:** Create `internal/alerts/datasource/datasource.go`, `datasource_test.go`

- [ ] **Step 1: Failing test** — `httptest` server returning Prometheus `{status,data:{resultType,result}}`. Assert:
  - vector result → `KindVector` with samples+labels (`__name__` dropped into `Labels`? keep all labels as returned).
  - scalar result → `KindScalar`, one sample, no labels.
  - empty vector → `KindEmpty`.
  - `status:"error"` body → error.
  - HTTP 401 → error mentioning auth.
  - bearer auth sets `Authorization: Bearer tok`; basic sets basic header; custom headers sent.
  - context timeout → error (server sleeps).
  - malformed JSON → error.

```go
func TestPrometheusQueryVector(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer tok" { t.Errorf("auth=%q", got) }
		_ = r.ParseForm()
		w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"instance":"a"},"value":[1,"0"]}]}}`))
	}))
	defer srv.Close()
	ds := datasource.New(models.Datasource{BaseURL: srv.URL, AuthType: "bearer", Credentials: "tok", TimeoutMS: 2000})
	res, err := ds.Query(context.Background(), "up==0", time.Unix(1,0))
	// assert KindVector, 1 sample, Labels["instance"]=="a", Value==0
}
```

- [ ] **Step 2:** Run, expect FAIL.
- [ ] **Step 3:** Implement: `New(models.Datasource) Datasource` builds an `*http.Client{Timeout}`. `Query` GETs `{BaseURL}/api/v1/query?query=&time=`, applies auth/headers, decodes the Prom envelope, maps resultType. `value` is `[<ts float>, "<str>"]`; parse the string with `strconv.ParseFloat` (handle `NaN/+Inf/-Inf`). Non-2xx or `status!=success` → error including any `error` field.
- [ ] **Step 4:** Run, expect PASS.
- [ ] **Step 5:** Commit `feat(alerts): prometheus datasource client`.

---

## Task 3: Pure state machine

**Files:** Create `internal/alerts/state/state.go`, `state_test.go`

- [ ] **Step 1: Failing test** — table-driven `Next`:

```go
type tc struct{ cur models.State; condTrue bool; activeAt, now time.Time; forD time.Duration; want models.State }
// OK + cond=false -> OK (no transition)
// OK + cond=true, for=0 -> Firing
// OK + cond=true, for>0 -> Pending (activeAt set by caller)
// Pending + cond=true, now-activeAt>=for -> Firing
// Pending + cond=true, now-activeAt<for -> Pending
// Pending + cond=false -> Resolved
// Firing + cond=true -> Firing
// Firing + cond=false -> Resolved
// Resolved is terminal-in-eval (caller drops instance)
```

`Next` returns `(next State, changed bool)`. It does NOT do I/O or set timers; caller owns `activeAt`.

- [ ] **Step 2:** Run, expect FAIL.
- [ ] **Step 3:** Implement `func Next(cur models.State, condTrue bool, activeAt, now time.Time, forD time.Duration) (models.State, bool)`.
- [ ] **Step 4:** Run, expect PASS.
- [ ] **Step 5:** Commit `feat(alerts): alert state machine`.

---

## Task 4: Store interface + SQLite impl + conformance suite

**Files:** Create `internal/alerts/storage/{store.go,sqlite.go,conformance.go,sqlite_test.go}`

- [ ] **Step 1: Failing test** — `sqlite_test.go` calls `storage.RunConformance(t, func() storage.Store { s,_ := storage.OpenSQLite(":memory:"); return s })`. Conformance covers:
  - datasource CRUD + unique name + list by source.
  - rule CRUD + list + enable/disable + get-missing → `ErrNotFound`.
  - instance upsert by `(rule_id,fingerprint)`, list active, delete, list-by-rule.
  - `AppendHistory` returns increasing id; `History(filter)` paginates by `since`/`limit`/`rule_id`; `Events(since,limit)` returns ordered rows; history survives across reopen (file-backed temp).
  - `LoadActive()` returns persisted instances after reopen.

- [ ] **Step 2:** Run, expect FAIL.
- [ ] **Step 3:** Implement `Store` interface:

```go
type Store interface {
	// datasources
	PutDatasource(context.Context, models.Datasource) error
	GetDatasource(ctx, id string) (models.Datasource, error)
	GetDatasourceByName(ctx, name string) (models.Datasource, error)
	ListDatasources(context.Context) ([]models.Datasource, error)
	DeleteDatasource(ctx, id string) error
	// rules
	PutRule(context.Context, models.Rule) error
	GetRule(ctx, id string) (models.Rule, error)
	ListRules(context.Context) ([]models.Rule, error)
	DeleteRule(ctx, id string) error
	// instances
	UpsertInstance(context.Context, models.Instance) error
	DeleteInstance(ctx, id string) error
	ListActiveInstances(context.Context) ([]models.Instance, error)
	ListInstancesByRule(ctx, ruleID string) ([]models.Instance, error)
	// history / events
	AppendHistory(context.Context, models.HistoryEntry) (int64, error)
	History(ctx, f HistoryFilter) ([]models.HistoryEntry, error)
	Events(ctx, since int64, limit int) ([]models.HistoryEntry, error)
	Close() error
}
var ErrNotFound = errors.New("not found")
type HistoryFilter struct{ RuleID string; Since int64; Limit int }
```

  `OpenSQLite(dsn)` opens with `_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)`, sets `db.SetMaxOpenConns(1)` (serialize writes; simplest correct option), runs idempotent `CREATE TABLE IF NOT EXISTS` migrations + `schema_version`. JSON columns via `encoding/json`. Times stored as unix-millis INTEGER.

- [ ] **Step 4:** Run, expect PASS (`go test ./internal/alerts/storage/...`).
- [ ] **Step 5:** Commit `feat(alerts): sqlite store + conformance suite`.

---

## Task 5: Evaluator

**Files:** Create `internal/alerts/evaluator/{evaluator.go,evaluator_test.go}`

- [ ] **Step 1: Failing test** — fake `Datasource` + in-memory `Store`. Assert:
  - vector with 2 series, for=0 → 2 FIRING instances + 2 history rows (OK→FIRING).
  - second eval same series → no new history (no change), `UpdatedAt`/value refreshed.
  - one series drops out → that instance RESOLVED (history FIRING→RESOLVED) and removed from active.
  - for>0 → first eval PENDING, eval after `activeAt+for` → FIRING.
  - datasource error → returns `Outcome{Err}`, NO instances resolved, history reason recorded, prior state held.
  - per-rule cap (e.g. 2) with 3 series → records `instance_cap` failure, doesn't track beyond cap.
  - rule labels override series labels in merged instance labels.

- [ ] **Step 2:** Run, expect FAIL.
- [ ] **Step 3:** Implement:

```go
type Evaluator struct{ store storage.Store; resolve func(models.Datasource) datasource.Datasource; maxInstances int; now func() time.Time }
type Outcome struct{ Active, Pending int; Transitions int; Err error; FailReason string }
func (e *Evaluator) EvaluateRule(ctx, rule models.Rule, ds models.Datasource) Outcome
```

  Algorithm: query ds; on error → record failure history (reason classified: `query`,`http`,`timeout`,`auth`,`invalid`), return without resolving. On success: build set of present fingerprints; load existing active instances for the rule; for each present sample upsert/advance via `state.Next`; for each previously-active fingerprint now absent → resolve. Append history only when `changed`. Enforce `maxInstances`.

- [ ] **Step 4:** Run, expect PASS (`-race`).
- [ ] **Step 5:** Commit `feat(alerts): rule evaluator + state persistence`.

---

## Task 6: Scheduler

**Files:** Create `internal/alerts/scheduler/{scheduler.go,scheduler_test.go}`

- [ ] **Step 1: Failing test** (`-race`):
  - two rules with different intervals each tick on their own cadence (count calls via atomic over a short window using small intervals like 20ms/50ms).
  - `Reconcile(rules)` starts new, stops removed, restarts changed (interval change).
  - a slow rule (datasource blocks) does not delay another rule's ticks.
  - `Stop()` cancels and returns promptly; no goroutine leak (WaitGroup).

- [ ] **Step 2:** Run, expect FAIL.
- [ ] **Step 3:** Implement: `New(eval func(ctx,ruleID)) *Scheduler`; `Reconcile(rules []models.Rule)` diffs by id+interval+enabled, manages a `map[id]cancelFn`; each rule goroutine: `ticker := time.NewTicker(interval)`, on tick call eval under the rule's own context; `Stop()` cancels root ctx + `wg.Wait()`. Eval callback is provided by Service (looks up rule+datasource, calls Evaluator, updates metrics, logs transitions).
- [ ] **Step 4:** Run, expect PASS (`-race`).
- [ ] **Step 5:** Commit `feat(alerts): per-rule evaluation scheduler`.

---

## Task 7: Metrics collector

**Files:** Create `internal/alerts/metrics/{metrics.go,metrics_test.go}`

- [ ] **Step 1: Failing test** — increment counters/set gauges, `WriteExposition` to a buffer, assert lines incl. `# TYPE` and label sets (`{result="success"}`, `{reason="timeout"}`). Concurrency-safe (`-race`).
- [ ] **Step 2:** Run, expect FAIL.
- [ ] **Step 3:** Implement `Metrics` (mutex-guarded) with `IncEval(result)`, `IncFailure(reason)`, `IncTransition()`, `ObserveDuration(d)`, `SetRules/Active/Pending(n)`, `WriteExposition(io.Writer)` emitting all metrics from the spec.
- [ ] **Step 4:** Run, expect PASS.
- [ ] **Step 5:** Commit `feat(alerts): alert-engine self metrics`.

---

## Task 8: HTTP API

**Files:** Create `internal/alerts/api/{api.go,api_test.go}`

- [ ] **Step 1: Failing test** — wire handlers over an in-memory store + fake evaluator (for the `/evaluate` endpoints). Cover each endpoint: rule CRUD (201/200/404/400 on bad PromQL-less body), enable/disable, active, history (`since`/`limit`), events cursor, datasource CRUD, 409 on editing a config/builtin datasource, 405 on wrong method, evaluate-one + evaluate-all.
- [ ] **Step 2:** Run, expect FAIL.
- [ ] **Step 3:** Implement `New(deps) http.Handler` registering routes on an internal `*http.ServeMux` with the `{status,data,error}` envelope (mirroring `internal/api`). `deps` exposes store + an `Evaluate(ctx, ruleID) (Outcome,error)` + `EvaluateAll(ctx)`. Validation: name+promql required, interval>0, severity non-empty, datasource exists. Config/builtin datasource edits/deletes → 409.
- [ ] **Step 4:** Run, expect PASS.
- [ ] **Step 5:** Commit `feat(alerts): REST API handlers`.

---

## Task 9: Service assembly

**Files:** Create `internal/alerts/{alerts.go,alerts_test.go}`

- [ ] **Step 1: Failing test** — `NewService(Options{StorePath, Datasources, DefaultDS})`: seeds config+builtin datasources (upsert by name, source set), loads active instances, `Handler()` serves API, `Collector()` writes metrics, `Start(ctx)` runs scheduler and reconciles when a rule is created via the API, `Stop()` clean. An end-to-end test: create a datasource pointing at an `httptest` server returning a firing vector, create a rule with for=0 and a tiny interval, assert it becomes FIRING and appears in `/active` and `/events`.
- [ ] **Step 2:** Run, expect FAIL.
- [ ] **Step 3:** Implement `Service`: owns store, datasource resolver (cache id→Datasource, rebuilt on datasource change), evaluator, scheduler, metrics. The eval callback resolves rule+datasource, calls evaluator, updates metrics + structured logs on transitions. API mutations that change rules call `scheduler.Reconcile`. `seedDatasources` upserts builtin `local` (from listen addr) + config entries.
- [ ] **Step 4:** Run, expect PASS (`-race`).
- [ ] **Step 5:** Commit `feat(alerts): service wiring + scheduler reconcile`.

---

## Task 10: Config block

**Files:** Modify `internal/config/config.go`; `internal/config/config_test.go`

- [ ] **Step 1: Failing test** — parse an `alerting:` block: enabled, storage_path, default_datasource, datasources[] with url/type/timeout/authorization/basic_auth/headers/enabled; defaults (enabled defaults true via `*bool`; datasource timeout default). Reuse existing secret resolution for credentials.
- [ ] **Step 2:** Run, expect FAIL.
- [ ] **Step 3:** Implement `AlertingConfig` + `AlertDatasourceConfig` structs, `applyDefaults`, validation (unique datasource names, valid auth, url required for non-builtin), and hook secret resolution for datasource credentials in `resolveSecrets`.
- [ ] **Step 4:** Run, expect PASS.
- [ ] **Step 5:** Commit `feat(config): alerting + datasource configuration`.

---

## Task 11: Mount in API + cmd wiring

**Files:** Modify `internal/api/api.go` (+ `selfmetrics.go` to chain collectors), `cmd/omni/main.go`; tests in `internal/api/api_test.go`, `cmd/omni/main_test.go`

- [ ] **Step 1: Failing test** — `internal/api`: with `Options.AlertHandler` set, `/api/v1/alerts*` routes to it; `/metrics` includes a registered `ExtraCollector`'s output. `cmd/omni`: `buildAlertDatasources(cfg)` maps config → `models.Datasource`.
- [ ] **Step 2:** Run, expect FAIL.
- [ ] **Step 3:** Implement: add `AlertHandler http.Handler` + `ExtraCollectors []func(io.Writer)` to `api.Options`; route `/api/v1/alerts/` and `/api/v1/datasources/` to `AlertHandler` (registered before the `/api/` not-found); `/metrics` writes self then extras. In `main.go`: when `cfg.Alerting.IsEnabled()`, build datasources, `alerts.NewService`, `go svc.Start(ctx)`, pass `svc.Handler()` + `svc.Collector()` to `api.New`, `defer svc.Stop()`.
- [ ] **Step 4:** Run, expect PASS; then `go build -o omni ./cmd/omni` and run the binary, create a rule via curl, observe FIRING + `/metrics`.
- [ ] **Step 5:** Commit `feat(alerts): mount alerting routes + metrics in server`.

---

## Task 12: Web UI

**Files:** Modify `web/assets/{index.html,app.js,styles.css}`; touch `web/web_test.go` if it asserts routes.

- [ ] **Step 1:** Add nav links (Alerts, Active, History) + hash routes `#/alerts`, `#/alerts/active`, `#/alerts/history`, `#/alerts/new`, `#/alerts/:id`, `#/datasources`.
- [ ] **Step 2:** Implement render functions calling the new APIs: rules table (name, severity, state, last eval, last transition, enabled toggle); active table (status, severity, rule, current value, started-at, duration); history table; rule editor form (PromQL textarea, datasource `<select>`, severity, interval, for, labels/annotations key-value rows, enable checkbox, Save/Delete/Evaluate-now); datasources list/editor for api-sourced ones.
- [ ] **Step 3:** Style to match existing tokens (reuse `.card`, `.table`, badges; add `.sev-critical/.sev-warning`, `.state-firing/.pending/.ok` color chips honoring dark+light).
- [ ] **Step 4:** Verify in the browser preview (start server, drive the pages, screenshot dark+light).
- [ ] **Step 5:** Commit `feat(web): alerting console (rules, active, history, editor)`.

---

## Task 13: Docs + examples

**Files:** Modify `examples/omni.yml`, `README.md`

- [ ] **Step 1:** Add a commented `alerting:` block to `examples/omni.yml` (local + a remote datasource with bearer).
- [ ] **Step 2:** README: alerting section — concepts (datasources, rules, states), the REST endpoints incl. the events feed for Omni-Notify, metrics list, config reference.
- [ ] **Step 3:** Commit `docs: alerting engine configuration + API`.

---

## Task 14: Quality gate + verification + adversarial review

- [ ] **Step 1:** `gofmt -w .` → `go vet ./...` → `go test ./... -race` all green.
- [ ] **Step 2:** Build static (`CGO_ENABLED=0 go build`) to confirm modernc keeps the distroless-compatible static binary; run the binary end-to-end (create datasource+rule, force a firing condition, confirm PENDING→FIRING→RESOLVED in `/active`, `/history`, `/events`, `/metrics`; restart and confirm state recovery). Capture evidence.
- [ ] **Step 3:** Adversarial review (reviewer prompted to refute): focus on failure-doesn't-resolve, restart recovery, SQLite locking under parallel ticks, cap enforcement, datasource auth leak in logs, mux precedence, graceful shutdown. Triage: fix cheap correctness/observability findings test-first; document larger deferrals.
- [ ] **Step 4:** Update memory (`omni-metrics-project.md`) with the M4 ship note.

---

## Self-Review notes

- **Spec coverage:** datasources (T2,T10), rules CRUD (T4,T8), state machine (T3), scheduler (T6), evaluation/failures (T5), instances/multi-series (T5), history/events (T4,T8), REST (T8,T11), UI (T12), metrics (T7,T11), logging (T9), tests (every task). ✓
- **Types consistent** across tasks: `Store`, `Datasource`, `Evaluator.EvaluateRule`, `state.Next`, `Outcome`, `HistoryFilter`.
- **No notification delivery** anywhere — events feed is pull-only (T8). ✓
