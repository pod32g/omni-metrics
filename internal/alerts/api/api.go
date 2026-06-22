// Package api exposes the alerting engine's REST surface: rule CRUD,
// enable/disable, synchronous evaluation, the active-instance and history views,
// the machine-readable events feed, and datasource management. Responses use the
// same {status,data,error} envelope as the core omni API. The handler is mounted
// by the core server under /api/v1/alerts and /api/v1/datasources.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/pod32g/omni-metrics/internal/alerts/models"
	"github.com/pod32g/omni-metrics/internal/alerts/storage"
)

// EvalResult is the summary returned by a synchronous evaluation request.
type EvalResult struct {
	Active      int `json:"active"`
	Pending     int `json:"pending"`
	Transitions int `json:"transitions"`
}

// Deps are the collaborators the handlers need. The Evaluate/EvaluateAll/
// TestDatasource closures and the OnRulesChanged callback are supplied by the
// Service so this package stays decoupled from the scheduler and evaluator.
type Deps struct {
	Store          storage.Store
	Evaluate       func(ctx context.Context, ruleID string) (EvalResult, error)
	EvaluateAll    func(ctx context.Context) (int, error)
	TestDatasource func(ctx context.Context, ds models.Datasource) error
	OnRulesChanged func()
	Now            func() time.Time
	// DefaultDatasourceID is applied to a rule that is created/updated without an
	// explicit datasource_id.
	DefaultDatasourceID string
}

type handler struct {
	d   Deps
	mux *http.ServeMux
}

// New builds the alerting HTTP handler.
func New(d Deps) http.Handler {
	if d.Now == nil {
		d.Now = time.Now
	}
	h := &handler{d: d, mux: http.NewServeMux()}
	h.routes()
	return h
}

func (h *handler) routes() {
	// Rules. The literal sub-paths (active/history/events/evaluate) are more
	// specific than /{id}, so Go 1.22 mux precedence routes them correctly.
	h.mux.HandleFunc("GET /api/v1/alerts", h.listRules)
	h.mux.HandleFunc("POST /api/v1/alerts", h.createRule)
	h.mux.HandleFunc("GET /api/v1/alerts/active", h.listActive)
	h.mux.HandleFunc("GET /api/v1/alerts/history", h.listHistory)
	h.mux.HandleFunc("GET /api/v1/alerts/events", h.listEvents)
	h.mux.HandleFunc("POST /api/v1/alerts/evaluate", h.evaluateAll)
	h.mux.HandleFunc("GET /api/v1/alerts/{id}", h.getRule)
	h.mux.HandleFunc("PUT /api/v1/alerts/{id}", h.updateRule)
	h.mux.HandleFunc("DELETE /api/v1/alerts/{id}", h.deleteRule)
	h.mux.HandleFunc("POST /api/v1/alerts/{id}/enable", h.enableRule)
	h.mux.HandleFunc("POST /api/v1/alerts/{id}/disable", h.disableRule)
	h.mux.HandleFunc("POST /api/v1/alerts/{id}/evaluate", h.evaluateOne)

	// Datasources.
	h.mux.HandleFunc("GET /api/v1/datasources", h.listDatasources)
	h.mux.HandleFunc("POST /api/v1/datasources", h.createDatasource)
	h.mux.HandleFunc("GET /api/v1/datasources/{id}", h.getDatasource)
	h.mux.HandleFunc("PUT /api/v1/datasources/{id}", h.updateDatasource)
	h.mux.HandleFunc("DELETE /api/v1/datasources/{id}", h.deleteDatasource)
	h.mux.HandleFunc("POST /api/v1/datasources/{id}/test", h.testDatasource)
}

// ServeHTTP delegates to the mux but rewrites the mux's default *plaintext* 404
// into the JSON error envelope, preserving the API's envelope invariant for
// unknown subpaths. The mux's automatic 405 (method mismatch on a known path) is
// left untouched. Our own handlers' 404s already set Content-Type=application/
// json, so they pass through unchanged.
func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(&jsonNotFound{ResponseWriter: w, path: r.URL.Path}, r)
}

// jsonNotFound intercepts a default-mux 404 (text/plain) and replaces it with a
// JSON error envelope.
type jsonNotFound struct {
	http.ResponseWriter
	path        string
	intercepted bool
}

func (j *jsonNotFound) WriteHeader(code int) {
	ct := j.ResponseWriter.Header().Get("Content-Type")
	if code == http.StatusNotFound && (ct == "" || !hasJSONPrefix(ct)) {
		j.intercepted = true
		writeErr(j.ResponseWriter, http.StatusNotFound, "not_found", "unknown alerting path "+j.path)
		return
	}
	j.ResponseWriter.WriteHeader(code)
}

func (j *jsonNotFound) Write(b []byte) (int, error) {
	if j.intercepted {
		return len(b), nil // swallow the mux's "404 page not found" body
	}
	return j.ResponseWriter.Write(b)
}

func hasJSONPrefix(ct string) bool {
	return len(ct) >= 16 && ct[:16] == "application/json"
}

// --- envelope ---

type envelope struct {
	Status    string      `json:"status"`
	Data      interface{} `json:"data,omitempty"`
	ErrorType string      `json:"errorType,omitempty"`
	Error     string      `json:"error,omitempty"`
}

func writeData(w http.ResponseWriter, code int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(envelope{Status: "success", Data: data})
}

func writeErr(w http.ResponseWriter, code int, errType, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(envelope{Status: "error", ErrorType: errType, Error: msg})
}

// mapStoreErr turns ErrNotFound into a 404 and anything else into a 500.
func mapStoreErr(w http.ResponseWriter, err error) {
	if errors.Is(err, storage.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	writeErr(w, http.StatusInternalServerError, "internal", err.Error())
}

func parseLimit(s string) int {
	if s == "" {
		return 0
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func parseSince(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	if n < 0 {
		return 0
	}
	return n
}
