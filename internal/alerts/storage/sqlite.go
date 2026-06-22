package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/pod32g/omni-metrics/internal/alerts/models"

	_ "modernc.org/sqlite"
)

// sqliteStore is the SQLite-backed Store. It runs in WAL mode with a busy
// timeout and foreign keys on, and serializes writes via a single connection so
// parallel rule goroutines never hit "database is locked".
type sqliteStore struct {
	db *sql.DB
}

// OpenSQLite opens (creating if needed) a SQLite-backed Store at dsn. Use
// ":memory:" for an ephemeral store or a file path for a durable one.
func OpenSQLite(dsn string) (Store, error) {
	// Pragmas via the modernc DSN query string. busy_timeout avoids transient
	// lock errors; WAL allows a reader concurrent with the single writer; foreign
	// keys enforce instance/history integrity.
	conn := dsn + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)"
	db, err := sql.Open("sqlite", conn)
	if err != nil {
		return nil, fmt.Errorf("opening sqlite: %w", err)
	}
	// A single connection serializes all access — the simplest correct option for
	// SQLite under concurrent goroutines, and the write volume here is tiny.
	db.SetMaxOpenConns(1)
	s := &sqliteStore{db: db}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *sqliteStore) Close() error { return s.db.Close() }

// migrate creates tables idempotently and records the schema version.
func (s *sqliteStore) migrate(ctx context.Context) error {
	const schema = `
CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS datasources (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL UNIQUE,
	type TEXT NOT NULL,
	base_url TEXT NOT NULL,
	auth_type TEXT NOT NULL,
	credentials TEXT NOT NULL DEFAULT '',
	basic_user TEXT NOT NULL DEFAULT '',
	basic_pass TEXT NOT NULL DEFAULT '',
	headers TEXT NOT NULL DEFAULT '{}',
	timeout_ms INTEGER NOT NULL DEFAULT 0,
	enabled INTEGER NOT NULL DEFAULT 1,
	source TEXT NOT NULL DEFAULT 'api',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS alert_rules (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	datasource_id TEXT NOT NULL DEFAULT '',
	promql TEXT NOT NULL,
	eval_interval_s INTEGER NOT NULL,
	for_s INTEGER NOT NULL DEFAULT 0,
	severity TEXT NOT NULL DEFAULT '',
	labels TEXT NOT NULL DEFAULT '{}',
	annotations TEXT NOT NULL DEFAULT '{}',
	enabled INTEGER NOT NULL DEFAULT 1,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS alert_instances (
	id TEXT PRIMARY KEY,
	rule_id TEXT NOT NULL,
	fingerprint TEXT NOT NULL,
	state TEXT NOT NULL,
	current_value REAL NOT NULL DEFAULT 0,
	active_at INTEGER NOT NULL,
	started_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	resolved_at INTEGER,
	labels TEXT NOT NULL DEFAULT '{}',
	annotations TEXT NOT NULL DEFAULT '{}',
	UNIQUE (rule_id, fingerprint)
);
CREATE TABLE IF NOT EXISTS alert_history (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	rule_id TEXT NOT NULL,
	fingerprint TEXT NOT NULL,
	prev_state TEXT NOT NULL,
	new_state TEXT NOT NULL,
	timestamp INTEGER NOT NULL,
	value REAL NOT NULL DEFAULT 0,
	reason TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_history_rule ON alert_history(rule_id);
`
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("migrating schema: %w", err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_version`).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO schema_version(version) VALUES (1)`); err != nil {
			return err
		}
	}
	return nil
}

// --- helpers ---

func toJSON(m map[string]string) string {
	if len(m) == 0 {
		return "{}"
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func fromJSON(s string) map[string]string {
	if s == "" || s == "{}" {
		return nil
	}
	var m map[string]string
	_ = json.Unmarshal([]byte(s), &m)
	return m
}

func ms(t time.Time) int64 { return t.UnixMilli() }

func fromMs(v int64) time.Time { return time.UnixMilli(v).UTC() }

// --- datasources ---

func (s *sqliteStore) PutDatasource(ctx context.Context, d models.Datasource) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO datasources (id,name,type,base_url,auth_type,credentials,basic_user,basic_pass,headers,timeout_ms,enabled,source,created_at,updated_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET
	name=excluded.name, type=excluded.type, base_url=excluded.base_url, auth_type=excluded.auth_type,
	credentials=excluded.credentials, basic_user=excluded.basic_user, basic_pass=excluded.basic_pass,
	headers=excluded.headers, timeout_ms=excluded.timeout_ms, enabled=excluded.enabled,
	source=excluded.source, updated_at=excluded.updated_at`,
		d.ID, d.Name, d.Type, d.BaseURL, string(d.AuthType), d.Credentials, d.BasicUser, d.BasicPass,
		toJSON(d.Headers), d.TimeoutMS, boolToInt(d.Enabled), d.Source, ms(d.CreatedAt), ms(d.UpdatedAt))
	if err != nil {
		return fmt.Errorf("put datasource: %w", err)
	}
	return nil
}

func (s *sqliteStore) scanDatasource(row interface{ Scan(...any) error }) (models.Datasource, error) {
	var d models.Datasource
	var authType, headers string
	var enabled int
	var created, updated int64
	if err := row.Scan(&d.ID, &d.Name, &d.Type, &d.BaseURL, &authType, &d.Credentials, &d.BasicUser, &d.BasicPass, &headers, &d.TimeoutMS, &enabled, &d.Source, &created, &updated); err != nil {
		return models.Datasource{}, err
	}
	d.AuthType = models.AuthType(authType)
	d.Headers = fromJSON(headers)
	d.Enabled = enabled != 0
	d.CreatedAt = fromMs(created)
	d.UpdatedAt = fromMs(updated)
	return d, nil
}

const datasourceCols = `id,name,type,base_url,auth_type,credentials,basic_user,basic_pass,headers,timeout_ms,enabled,source,created_at,updated_at`

func (s *sqliteStore) GetDatasource(ctx context.Context, id string) (models.Datasource, error) {
	d, err := s.scanDatasource(s.db.QueryRowContext(ctx, `SELECT `+datasourceCols+` FROM datasources WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return models.Datasource{}, ErrNotFound
	}
	return d, err
}

func (s *sqliteStore) GetDatasourceByName(ctx context.Context, name string) (models.Datasource, error) {
	d, err := s.scanDatasource(s.db.QueryRowContext(ctx, `SELECT `+datasourceCols+` FROM datasources WHERE name=?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return models.Datasource{}, ErrNotFound
	}
	return d, err
}

func (s *sqliteStore) ListDatasources(ctx context.Context) ([]models.Datasource, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+datasourceCols+` FROM datasources ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.Datasource
	for rows.Next() {
		d, err := s.scanDatasource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *sqliteStore) DeleteDatasource(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM datasources WHERE id=?`, id)
	return deleteResult(res, err)
}

// --- rules ---

func (s *sqliteStore) PutRule(ctx context.Context, r models.Rule) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO alert_rules (id,name,description,datasource_id,promql,eval_interval_s,for_s,severity,labels,annotations,enabled,created_at,updated_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET
	name=excluded.name, description=excluded.description, datasource_id=excluded.datasource_id,
	promql=excluded.promql, eval_interval_s=excluded.eval_interval_s, for_s=excluded.for_s,
	severity=excluded.severity, labels=excluded.labels, annotations=excluded.annotations,
	enabled=excluded.enabled, updated_at=excluded.updated_at`,
		r.ID, r.Name, r.Description, r.DatasourceID, r.PromQL, r.EvalIntervalS, r.ForS, string(r.Severity),
		toJSON(r.Labels), toJSON(r.Annotations), boolToInt(r.Enabled), ms(r.CreatedAt), ms(r.UpdatedAt))
	if err != nil {
		return fmt.Errorf("put rule: %w", err)
	}
	return nil
}

const ruleCols = `id,name,description,datasource_id,promql,eval_interval_s,for_s,severity,labels,annotations,enabled,created_at,updated_at`

func (s *sqliteStore) scanRule(row interface{ Scan(...any) error }) (models.Rule, error) {
	var r models.Rule
	var severity, labels, annotations string
	var enabled int
	var created, updated int64
	if err := row.Scan(&r.ID, &r.Name, &r.Description, &r.DatasourceID, &r.PromQL, &r.EvalIntervalS, &r.ForS, &severity, &labels, &annotations, &enabled, &created, &updated); err != nil {
		return models.Rule{}, err
	}
	r.Severity = models.Severity(severity)
	r.Labels = fromJSON(labels)
	r.Annotations = fromJSON(annotations)
	r.Enabled = enabled != 0
	r.CreatedAt = fromMs(created)
	r.UpdatedAt = fromMs(updated)
	return r, nil
}

func (s *sqliteStore) GetRule(ctx context.Context, id string) (models.Rule, error) {
	r, err := s.scanRule(s.db.QueryRowContext(ctx, `SELECT `+ruleCols+` FROM alert_rules WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return models.Rule{}, ErrNotFound
	}
	return r, err
}

func (s *sqliteStore) ListRules(ctx context.Context) ([]models.Rule, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+ruleCols+` FROM alert_rules ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.Rule
	for rows.Next() {
		r, err := s.scanRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *sqliteStore) DeleteRule(ctx context.Context, id string) error {
	// Dropping a rule also drops its active instances; history is retained.
	if _, err := s.db.ExecContext(ctx, `DELETE FROM alert_instances WHERE rule_id=?`, id); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM alert_rules WHERE id=?`, id)
	return deleteResult(res, err)
}

// --- instances ---

func (s *sqliteStore) UpsertInstance(ctx context.Context, in models.Instance) error {
	var resolved sql.NullInt64
	if in.ResolvedAt != nil {
		resolved = sql.NullInt64{Int64: ms(*in.ResolvedAt), Valid: true}
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO alert_instances (id,rule_id,fingerprint,state,current_value,active_at,started_at,updated_at,resolved_at,labels,annotations)
VALUES (?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(rule_id,fingerprint) DO UPDATE SET
	state=excluded.state, current_value=excluded.current_value, active_at=excluded.active_at,
	updated_at=excluded.updated_at, resolved_at=excluded.resolved_at,
	labels=excluded.labels, annotations=excluded.annotations`,
		in.ID, in.RuleID, in.Fingerprint, in.State.String(), in.CurrentValue, ms(in.ActiveAt), ms(in.StartedAt), ms(in.UpdatedAt), resolved, toJSON(in.Labels), toJSON(in.Annotations))
	if err != nil {
		return fmt.Errorf("upsert instance: %w", err)
	}
	return nil
}

const instanceCols = `id,rule_id,fingerprint,state,current_value,active_at,started_at,updated_at,resolved_at,labels,annotations`

func (s *sqliteStore) scanInstance(row interface{ Scan(...any) error }) (models.Instance, error) {
	var in models.Instance
	var stateName, labels, annotations string
	var active, started, updated int64
	var resolved sql.NullInt64
	if err := row.Scan(&in.ID, &in.RuleID, &in.Fingerprint, &stateName, &in.CurrentValue, &active, &started, &updated, &resolved, &labels, &annotations); err != nil {
		return models.Instance{}, err
	}
	st, _ := models.ParseState(stateName)
	in.State = st
	in.StateName = stateName
	in.ActiveAt = fromMs(active)
	in.StartedAt = fromMs(started)
	in.UpdatedAt = fromMs(updated)
	if resolved.Valid {
		ts := fromMs(resolved.Int64)
		in.ResolvedAt = &ts
	}
	in.Labels = fromJSON(labels)
	in.Annotations = fromJSON(annotations)
	return in, nil
}

func (s *sqliteStore) ListActiveInstances(ctx context.Context) ([]models.Instance, error) {
	return s.queryInstances(ctx, `SELECT `+instanceCols+` FROM alert_instances ORDER BY started_at, id`)
}

func (s *sqliteStore) ListInstancesByRule(ctx context.Context, ruleID string) ([]models.Instance, error) {
	return s.queryInstances(ctx, `SELECT `+instanceCols+` FROM alert_instances WHERE rule_id=? ORDER BY started_at, id`, ruleID)
}

func (s *sqliteStore) queryInstances(ctx context.Context, q string, args ...any) ([]models.Instance, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.Instance
	for rows.Next() {
		in, err := s.scanInstance(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

func (s *sqliteStore) DeleteInstance(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM alert_instances WHERE id=?`, id)
	return deleteResult(res, err)
}

// --- history / events ---

func (s *sqliteStore) AppendHistory(ctx context.Context, h models.HistoryEntry) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
INSERT INTO alert_history (rule_id,fingerprint,prev_state,new_state,timestamp,value,reason)
VALUES (?,?,?,?,?,?,?)`,
		h.RuleID, h.Fingerprint, h.Prev.String(), h.New.String(), ms(h.Timestamp), h.Value, h.Reason)
	if err != nil {
		return 0, fmt.Errorf("append history: %w", err)
	}
	return res.LastInsertId()
}

func (s *sqliteStore) History(ctx context.Context, f HistoryFilter) ([]models.HistoryEntry, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 200
	}
	q := `SELECT id,rule_id,fingerprint,prev_state,new_state,timestamp,value,reason FROM alert_history WHERE id > ?`
	args := []any{f.Since}
	if f.RuleID != "" {
		q += ` AND rule_id = ?`
		args = append(args, f.RuleID)
	}
	q += ` ORDER BY id LIMIT ?`
	args = append(args, limit)
	return s.queryHistory(ctx, q, args...)
}

func (s *sqliteStore) Events(ctx context.Context, since int64, limit int) ([]models.HistoryEntry, error) {
	return s.History(ctx, HistoryFilter{Since: since, Limit: limit})
}

func (s *sqliteStore) queryHistory(ctx context.Context, q string, args ...any) ([]models.HistoryEntry, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.HistoryEntry
	for rows.Next() {
		var h models.HistoryEntry
		var prev, nw string
		var tsMs int64
		if err := rows.Scan(&h.ID, &h.RuleID, &h.Fingerprint, &prev, &nw, &tsMs, &h.Value, &h.Reason); err != nil {
			return nil, err
		}
		h.Prev, _ = models.ParseState(prev)
		h.New, _ = models.ParseState(nw)
		h.PrevName = prev
		h.NewName = nw
		h.Timestamp = fromMs(tsMs)
		out = append(out, h)
	}
	return out, rows.Err()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func deleteResult(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
