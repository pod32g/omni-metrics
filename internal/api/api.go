// Package api exposes a Prometheus-compatible HTTP surface over the storage and
// query engine: instant and range queries, series/label introspection, scrape
// target health, the server's own /metrics, and the embedded web UI. Responses
// use Prometheus' {status,data} JSON envelope.
package api

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/pod32g/omni-metrics/internal/model"
	"github.com/pod32g/omni-metrics/internal/promql"
	"github.com/pod32g/omni-metrics/internal/scrape"
	"github.com/pod32g/omni-metrics/internal/tsdb"
)

// StorageQ is the read side of storage used for series/label introspection.
type StorageQ interface {
	Querier() tsdb.Querier
}

// TargetsProvider supplies scrape target health for /api/v1/targets.
type TargetsProvider interface {
	Targets() []scrape.TargetHealth
}

// Options configures the API server.
type Options struct {
	Engine     *promql.Engine
	Storage    StorageQ
	Targets    TargetsProvider
	Web        http.Handler
	Version    string
	HeadSeries func() int
}

// API is the HTTP handler for the omni-metrics server.
type API struct {
	opts Options
	mux  *http.ServeMux
	self *SelfMetrics
}

// New builds the API and registers routes.
func New(opts Options) *API {
	a := &API{
		opts: opts,
		mux:  http.NewServeMux(),
		self: NewSelfMetrics(opts.Version, opts.HeadSeries),
	}
	a.routes()
	return a
}

func (a *API) routes() {
	a.mux.HandleFunc("GET /api/v1/query", a.handleQuery)
	a.mux.HandleFunc("GET /api/v1/query_range", a.handleQueryRange)
	a.mux.HandleFunc("GET /api/v1/series", a.handleSeries)
	a.mux.HandleFunc("GET /api/v1/labels", a.handleLabels)
	a.mux.HandleFunc("GET /api/v1/label/{name}/values", a.handleLabelValues)
	a.mux.HandleFunc("GET /api/v1/targets", a.handleTargets)
	a.mux.HandleFunc("GET /metrics", a.handleMetrics)
	a.mux.HandleFunc("GET /-/healthy", a.handleHealth)
	a.mux.HandleFunc("GET /-/ready", a.handleHealth)
	if a.opts.Web != nil {
		a.mux.Handle("/", a.opts.Web)
	}
}

func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) { a.mux.ServeHTTP(w, r) }

// --- response envelope ---

type apiResponse struct {
	Status    string      `json:"status"`
	Data      interface{} `json:"data,omitempty"`
	ErrorType string      `json:"errorType,omitempty"`
	Error     string      `json:"error,omitempty"`
}

type queryData struct {
	ResultType string      `json:"resultType"`
	Result     interface{} `json:"result"`
}

func writeJSON(w http.ResponseWriter, code int, v apiResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeData(w http.ResponseWriter, data interface{}) {
	writeJSON(w, http.StatusOK, apiResponse{Status: "success", Data: data})
}

func writeError(w http.ResponseWriter, code int, errType, msg string) {
	writeJSON(w, code, apiResponse{Status: "error", ErrorType: errType, Error: msg})
}

// --- handlers ---

func (a *API) handleQuery(w http.ResponseWriter, r *http.Request) {
	a.self.IncHTTP("query")
	q := r.FormValue("query")
	if q == "" {
		writeError(w, http.StatusBadRequest, "bad_data", "missing query parameter")
		return
	}
	ts, err := parseTimeParam(r.FormValue("time"), time.Now())
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_data", err.Error())
		return
	}
	res, err := a.opts.Engine.InstantQuery(r.Context(), q, ts)
	a.self.IncQuery(err != nil)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_data", err.Error())
		return
	}
	writeData(w, resultToJSON(res))
}

func (a *API) handleQueryRange(w http.ResponseWriter, r *http.Request) {
	a.self.IncHTTP("query_range")
	q := r.FormValue("query")
	if q == "" {
		writeError(w, http.StatusBadRequest, "bad_data", "missing query parameter")
		return
	}
	start, err := parseTimeParam(r.FormValue("start"), time.Time{})
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_data", "invalid start: "+err.Error())
		return
	}
	end, err := parseTimeParam(r.FormValue("end"), time.Time{})
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_data", "invalid end: "+err.Error())
		return
	}
	step, err := parseStepParam(r.FormValue("step"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_data", "invalid step: "+err.Error())
		return
	}
	res, err := a.opts.Engine.RangeQuery(r.Context(), q, start, end, step)
	a.self.IncQuery(err != nil)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_data", err.Error())
		return
	}
	writeData(w, resultToJSON(res))
}

func (a *API) handleSeries(w http.ResponseWriter, r *http.Request) {
	a.self.IncHTTP("series")
	matches := r.URL.Query()["match[]"]
	if len(matches) == 0 {
		writeError(w, http.StatusBadRequest, "bad_data", "at least one match[] is required")
		return
	}
	start, err := parseTimeParam(r.FormValue("start"), time.UnixMilli(0))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_data", "invalid start: "+err.Error())
		return
	}
	end, err := parseTimeParam(r.FormValue("end"), time.Now().Add(time.Hour))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_data", "invalid end: "+err.Error())
		return
	}

	q := a.opts.Storage.Querier()
	seen := map[string]model.Labels{}
	for _, m := range matches {
		matchers, err := promql.ParseMatchers(m)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_data", err.Error())
			return
		}
		ss := q.Select(start, end, matchers...)
		for ss.Next() {
			l := ss.At().Labels()
			seen[l.String()] = l
		}
	}
	out := make([]map[string]string, 0, len(seen))
	for _, l := range seen {
		out = append(out, l.Map())
	}
	writeData(w, out)
}

func (a *API) handleLabels(w http.ResponseWriter, r *http.Request) {
	a.self.IncHTTP("labels")
	writeData(w, a.opts.Storage.Querier().LabelNames())
}

func (a *API) handleLabelValues(w http.ResponseWriter, r *http.Request) {
	a.self.IncHTTP("label_values")
	name := r.PathValue("name")
	writeData(w, a.opts.Storage.Querier().LabelValues(name))
}

func (a *API) handleTargets(w http.ResponseWriter, r *http.Request) {
	a.self.IncHTTP("targets")
	var targets []scrape.TargetHealth
	if a.opts.Targets != nil {
		targets = a.opts.Targets.Targets()
	}
	writeData(w, targets)
}

func (a *API) handleMetrics(w http.ResponseWriter, r *http.Request) {
	a.self.IncHTTP("metrics")
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	a.self.WriteExposition(w)
}

// handleHealth backs /-/healthy and /-/ready: a liveness/readiness probe used by
// the container healthcheck and the deploy smoke test.
func (a *API) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("omni-metrics is healthy\n"))
}

// --- conversion helpers ---

// resultToJSON converts a typed query result to the Prometheus JSON shape.
func resultToJSON(res promql.Result) queryData {
	switch res.Type {
	case promql.ValueScalar:
		return queryData{ResultType: "scalar", Result: point(res.Scalar.T, res.Scalar.V)}
	case promql.ValueVector:
		out := make([]vectorSampleJSON, 0, len(res.Vector))
		for _, s := range res.Vector {
			out = append(out, vectorSampleJSON{Metric: s.Metric.Map(), Value: point(s.T, s.V)})
		}
		return queryData{ResultType: "vector", Result: out}
	case promql.ValueMatrix:
		out := make([]matrixSeriesJSON, 0, len(res.Matrix))
		for _, s := range res.Matrix {
			pts := make([][2]interface{}, 0, len(s.Points))
			for _, p := range s.Points {
				pts = append(pts, point(p.T, p.V))
			}
			out = append(out, matrixSeriesJSON{Metric: s.Metric.Map(), Values: pts})
		}
		return queryData{ResultType: "matrix", Result: out}
	default:
		return queryData{ResultType: "string", Result: res.String}
	}
}

type vectorSampleJSON struct {
	Metric map[string]string `json:"metric"`
	Value  [2]interface{}    `json:"value"`
}

type matrixSeriesJSON struct {
	Metric map[string]string `json:"metric"`
	Values [][2]interface{}  `json:"values"`
}

// point renders a (ms-timestamp, value) pair as Prometheus does: float seconds
// and a string value.
func point(tMs int64, v float64) [2]interface{} {
	return [2]interface{}{float64(tMs) / 1000, formatValue(v)}
}

func formatValue(v float64) string {
	switch {
	case math.IsNaN(v):
		return "NaN"
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	default:
		return strconv.FormatFloat(v, 'g', -1, 64)
	}
}

// parseTimeParam parses a unix-seconds float (optionally fractional). An empty
// value falls back to def; if def is the zero time, an empty value is an error.
func parseTimeParam(s string, def time.Time) (int64, error) {
	if s == "" {
		if def.IsZero() {
			return 0, errMissingTime
		}
		return def.UnixMilli(), nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, err
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, fmt.Errorf("invalid time value %q", s)
	}
	return int64(f * 1000), nil
}

// parseStepParam accepts a duration string ("15s") or a plain number of seconds.
func parseStepParam(s string) (int64, error) {
	if s == "" {
		return 0, errMissingStep
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return int64(f * 1000), nil
	}
	d, err := parseDurationMillis(s)
	if err != nil {
		return 0, err
	}
	return d, nil
}
