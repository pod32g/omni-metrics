// Package models defines the core domain types of the alerting engine: alert
// rules, datasources, alert instances, history entries, the alert state enum,
// and the query Result an evaluation produces. The types are plain data shared
// across the datasource, state, storage, evaluator, scheduler, and api packages
// so none of those depend on each other for their vocabulary.
package models

import (
	"fmt"
	"hash/fnv"
	"sort"
	"time"
)

// State is a point in the alert lifecycle.
type State int

const (
	// StateOK means the alert condition is not currently true.
	StateOK State = iota
	// StatePending means the condition became true but has not yet satisfied
	// the rule's "for" duration.
	StatePending
	// StateFiring means the condition has held for the "for" duration.
	StateFiring
	// StateResolved means a previously firing/pending instance is no longer true.
	StateResolved
)

// String renders the state in its lowercase wire form.
func (s State) String() string {
	switch s {
	case StateOK:
		return "ok"
	case StatePending:
		return "pending"
	case StateFiring:
		return "firing"
	case StateResolved:
		return "resolved"
	default:
		return "unknown"
	}
}

// ParseState parses a wire-form state string.
func ParseState(s string) (State, error) {
	switch s {
	case "ok":
		return StateOK, nil
	case "pending":
		return StatePending, nil
	case "firing":
		return StateFiring, nil
	case "resolved":
		return StateResolved, nil
	default:
		return StateOK, fmt.Errorf("invalid alert state %q", s)
	}
}

// Severity is a free-form alert severity (e.g. "critical", "warning").
type Severity string

// AuthType selects how a datasource authenticates HTTP requests.
type AuthType string

const (
	// AuthNone sends no credentials.
	AuthNone AuthType = "none"
	// AuthBearer sends an Authorization: Bearer header.
	AuthBearer AuthType = "bearer"
	// AuthBasic sends HTTP basic auth.
	AuthBasic AuthType = "basic"
)

// Datasource source values distinguish how a datasource was created, which gates
// whether the API may mutate it.
const (
	SourceBuiltin = "builtin"
	SourceConfig  = "config"
	SourceAPI     = "api"
)

// Datasource describes a Prometheus-compatible query backend.
type Datasource struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Type        string            `json:"type"`
	BaseURL     string            `json:"base_url"`
	AuthType    AuthType          `json:"auth_type"`
	Credentials string            `json:"-"`
	BasicUser   string            `json:"basic_user,omitempty"`
	BasicPass   string            `json:"-"`
	Headers     map[string]string `json:"headers,omitempty"`
	TimeoutMS   int               `json:"timeout_ms"`
	Enabled     bool              `json:"enabled"`
	Source      string            `json:"source"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

// Editable reports whether the API may mutate this datasource. Config- and
// builtin-sourced datasources are owned by the config/process and would be
// overwritten on the next boot, so they are read-only at runtime.
func (d Datasource) Editable() bool { return d.Source == SourceAPI }

// Rule is a stored alert rule. The PromQL is evaluated exactly as written; the
// threshold comparison lives inside the expression.
type Rule struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	Description   string            `json:"description"`
	DatasourceID  string            `json:"datasource_id"`
	PromQL        string            `json:"promql"`
	EvalIntervalS int               `json:"evaluation_interval_seconds"`
	ForS          int               `json:"for_duration_seconds"`
	Severity      Severity          `json:"severity"`
	Labels        map[string]string `json:"labels,omitempty"`
	Annotations   map[string]string `json:"annotations,omitempty"`
	Enabled       bool              `json:"enabled"`
	CreatedAt     time.Time         `json:"created_at"`
	UpdatedAt     time.Time         `json:"updated_at"`
}

// Instance is a single active alert element produced by a rule. One rule yields
// one instance per result series (keyed by Fingerprint).
type Instance struct {
	ID           string            `json:"id"`
	RuleID       string            `json:"rule_id"`
	Fingerprint  string            `json:"fingerprint"`
	State        State             `json:"-"`
	StateName    string            `json:"status"`
	CurrentValue float64           `json:"current_value"`
	ActiveAt     time.Time         `json:"active_at"`
	StartedAt    time.Time         `json:"started_at"`
	UpdatedAt    time.Time         `json:"updated_at"`
	ResolvedAt   *time.Time        `json:"resolved_at,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
	Annotations  map[string]string `json:"annotations,omitempty"`
}

// HistoryEntry records a single persisted state transition. ID is a monotonic
// cursor used by the events feed.
type HistoryEntry struct {
	ID          int64     `json:"id"`
	RuleID      string    `json:"rule_id"`
	Fingerprint string    `json:"fingerprint"`
	Prev        State     `json:"-"`
	New         State     `json:"-"`
	PrevName    string    `json:"previous_state"`
	NewName     string    `json:"new_state"`
	Timestamp   time.Time `json:"timestamp"`
	Value       float64   `json:"value"`
	Reason      string    `json:"reason"`
}

// ResultKind classifies what a datasource query returned.
type ResultKind int

const (
	// KindEmpty is an empty vector — no active elements.
	KindEmpty ResultKind = iota
	// KindVector is one or more labelled samples.
	KindVector
	// KindScalar is a single scalar value with no labels.
	KindScalar
)

// Sample is one labelled value from a query result.
type Sample struct {
	Labels map[string]string
	Value  float64
}

// Result is the outcome of evaluating a rule's PromQL against a datasource.
type Result struct {
	Kind    ResultKind
	Samples []Sample
}

// Fingerprint returns a stable, order-independent identity for a label set. An
// empty or nil map yields a fixed sentinel so labelless (scalar) results share
// one instance.
func Fingerprint(labels map[string]string) string {
	h := fnv.New64a()
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		// Length-prefix to avoid "ab"+"c" colliding with "a"+"bc".
		fmt.Fprintf(h, "%d:%s=%d:%s\x00", len(k), k, len(labels[k]), labels[k])
	}
	return fmt.Sprintf("%016x", h.Sum64())
}
