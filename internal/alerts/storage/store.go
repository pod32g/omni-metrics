// Package storage persists alert rules, datasources, active alert instances, and
// the append-only alert history. The Store interface is backend-agnostic; the
// default implementation is SQLite (sqlite.go), and any implementation can be
// verified against the shared conformance suite (conformance.go).
package storage

import (
	"context"
	"errors"

	"github.com/pod32g/omni-metrics/internal/alerts/models"
)

// ErrNotFound is returned when a requested entity does not exist.
var ErrNotFound = errors.New("not found")

// HistoryFilter narrows a history query. Since selects entries with id > Since
// (0 = from the beginning); Limit caps the result (<=0 = a sane default).
type HistoryFilter struct {
	RuleID string
	Since  int64
	Limit  int
}

// Store is the persistence contract for the alerting engine.
type Store interface {
	// Datasources.
	PutDatasource(ctx context.Context, d models.Datasource) error
	GetDatasource(ctx context.Context, id string) (models.Datasource, error)
	GetDatasourceByName(ctx context.Context, name string) (models.Datasource, error)
	ListDatasources(ctx context.Context) ([]models.Datasource, error)
	DeleteDatasource(ctx context.Context, id string) error

	// Rules.
	PutRule(ctx context.Context, r models.Rule) error
	GetRule(ctx context.Context, id string) (models.Rule, error)
	ListRules(ctx context.Context) ([]models.Rule, error)
	DeleteRule(ctx context.Context, id string) error

	// Active alert instances.
	UpsertInstance(ctx context.Context, in models.Instance) error
	DeleteInstance(ctx context.Context, id string) error
	ListActiveInstances(ctx context.Context) ([]models.Instance, error)
	ListInstancesByRule(ctx context.Context, ruleID string) ([]models.Instance, error)

	// Append-only history / events feed.
	AppendHistory(ctx context.Context, h models.HistoryEntry) (int64, error)
	History(ctx context.Context, f HistoryFilter) ([]models.HistoryEntry, error)
	Events(ctx context.Context, since int64, limit int) ([]models.HistoryEntry, error)

	Close() error
}
