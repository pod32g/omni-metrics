package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/pod32g/omni-metrics/internal/alerts/models"
	"github.com/pod32g/omni-metrics/internal/alerts/storage"
)

// ruleRequest is the writable shape of a rule. id/created_at/updated_at are
// assigned server-side.
type ruleRequest struct {
	Name          string            `json:"name"`
	Description   string            `json:"description"`
	DatasourceID  string            `json:"datasource_id"`
	PromQL        string            `json:"promql"`
	EvalIntervalS int               `json:"evaluation_interval_seconds"`
	ForS          int               `json:"for_duration_seconds"`
	Severity      models.Severity   `json:"severity"`
	Labels        map[string]string `json:"labels"`
	Annotations   map[string]string `json:"annotations"`
	Enabled       *bool             `json:"enabled"`
}

func (h *handler) listRules(w http.ResponseWriter, r *http.Request) {
	rules, err := h.d.Store.ListRules(r.Context())
	if err != nil {
		mapStoreErr(w, err)
		return
	}
	if rules == nil {
		rules = []models.Rule{}
	}
	writeData(w, http.StatusOK, rules)
}

func (h *handler) getRule(w http.ResponseWriter, r *http.Request) {
	rule, err := h.d.Store.GetRule(r.Context(), r.PathValue("id"))
	if err != nil {
		mapStoreErr(w, err)
		return
	}
	instances, _ := h.d.Store.ListInstancesByRule(r.Context(), rule.ID)
	if instances == nil {
		instances = []models.Instance{}
	}
	writeData(w, http.StatusOK, map[string]interface{}{"rule": rule, "instances": instances})
}

func (h *handler) createRule(w http.ResponseWriter, r *http.Request) {
	var req ruleRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.DatasourceID == "" {
		req.DatasourceID = h.d.DefaultDatasourceID
	}
	if msg := h.validateRule(r, &req); msg != "" {
		writeErr(w, http.StatusBadRequest, "bad_data", msg)
		return
	}
	now := h.d.Now()
	rule := models.Rule{
		ID:            newID(),
		Name:          strings.TrimSpace(req.Name),
		Description:   req.Description,
		DatasourceID:  req.DatasourceID,
		PromQL:        strings.TrimSpace(req.PromQL),
		EvalIntervalS: req.EvalIntervalS,
		ForS:          req.ForS,
		Severity:      req.Severity,
		Labels:        req.Labels,
		Annotations:   req.Annotations,
		Enabled:       req.Enabled == nil || *req.Enabled,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := h.d.Store.PutRule(r.Context(), rule); err != nil {
		mapStoreErr(w, err)
		return
	}
	h.rulesChanged()
	writeData(w, http.StatusCreated, rule)
}

func (h *handler) updateRule(w http.ResponseWriter, r *http.Request) {
	existing, err := h.d.Store.GetRule(r.Context(), r.PathValue("id"))
	if err != nil {
		mapStoreErr(w, err)
		return
	}
	var req ruleRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.DatasourceID == "" {
		req.DatasourceID = h.d.DefaultDatasourceID
	}
	if msg := h.validateRule(r, &req); msg != "" {
		writeErr(w, http.StatusBadRequest, "bad_data", msg)
		return
	}
	existing.Name = strings.TrimSpace(req.Name)
	existing.Description = req.Description
	existing.DatasourceID = req.DatasourceID
	existing.PromQL = strings.TrimSpace(req.PromQL)
	existing.EvalIntervalS = req.EvalIntervalS
	existing.ForS = req.ForS
	existing.Severity = req.Severity
	existing.Labels = req.Labels
	existing.Annotations = req.Annotations
	if req.Enabled != nil {
		existing.Enabled = *req.Enabled
	}
	existing.UpdatedAt = h.d.Now()
	if err := h.d.Store.PutRule(r.Context(), existing); err != nil {
		mapStoreErr(w, err)
		return
	}
	h.rulesChanged()
	writeData(w, http.StatusOK, existing)
}

func (h *handler) deleteRule(w http.ResponseWriter, r *http.Request) {
	if err := h.d.Store.DeleteRule(r.Context(), r.PathValue("id")); err != nil {
		mapStoreErr(w, err)
		return
	}
	h.rulesChanged()
	writeData(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (h *handler) enableRule(w http.ResponseWriter, r *http.Request)  { h.setEnabled(w, r, true) }
func (h *handler) disableRule(w http.ResponseWriter, r *http.Request) { h.setEnabled(w, r, false) }

func (h *handler) setEnabled(w http.ResponseWriter, r *http.Request, enabled bool) {
	rule, err := h.d.Store.GetRule(r.Context(), r.PathValue("id"))
	if err != nil {
		mapStoreErr(w, err)
		return
	}
	rule.Enabled = enabled
	rule.UpdatedAt = h.d.Now()
	if err := h.d.Store.PutRule(r.Context(), rule); err != nil {
		mapStoreErr(w, err)
		return
	}
	h.rulesChanged()
	writeData(w, http.StatusOK, rule)
}

func (h *handler) evaluateOne(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := h.d.Store.GetRule(r.Context(), id); err != nil {
		mapStoreErr(w, err)
		return
	}
	if h.d.Evaluate == nil {
		writeErr(w, http.StatusServiceUnavailable, "unavailable", "evaluation not available")
		return
	}
	res, err := h.d.Evaluate(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "evaluation_failed", err.Error())
		return
	}
	writeData(w, http.StatusOK, res)
}

func (h *handler) evaluateAll(w http.ResponseWriter, r *http.Request) {
	if h.d.EvaluateAll == nil {
		writeErr(w, http.StatusServiceUnavailable, "unavailable", "evaluation not available")
		return
	}
	n, err := h.d.EvaluateAll(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, "evaluation_failed", err.Error())
		return
	}
	writeData(w, http.StatusOK, map[string]int{"evaluated": n})
}

func (h *handler) listActive(w http.ResponseWriter, r *http.Request) {
	active, err := h.d.Store.ListActiveInstances(r.Context())
	if err != nil {
		mapStoreErr(w, err)
		return
	}
	if active == nil {
		active = []models.Instance{}
	}
	writeData(w, http.StatusOK, active)
}

func (h *handler) listHistory(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	entries, err := h.d.Store.History(r.Context(), storage.HistoryFilter{
		RuleID: q.Get("rule_id"),
		Since:  parseSince(q.Get("since")),
		Limit:  parseLimit(q.Get("limit")),
	})
	if err != nil {
		mapStoreErr(w, err)
		return
	}
	if entries == nil {
		entries = []models.HistoryEntry{}
	}
	writeData(w, http.StatusOK, entries)
}

func (h *handler) listEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	entries, err := h.d.Store.Events(r.Context(), parseSince(q.Get("since")), parseLimit(q.Get("limit")))
	if err != nil {
		mapStoreErr(w, err)
		return
	}
	if entries == nil {
		entries = []models.HistoryEntry{}
	}
	writeData(w, http.StatusOK, entries)
}

// validateRule returns a non-empty message describing the first problem.
func (h *handler) validateRule(r *http.Request, req *ruleRequest) string {
	if strings.TrimSpace(req.Name) == "" {
		return "name is required"
	}
	if strings.TrimSpace(req.PromQL) == "" {
		return "promql is required"
	}
	if req.EvalIntervalS <= 0 {
		return "evaluation_interval_seconds must be positive"
	}
	if req.ForS < 0 {
		return "for_duration_seconds must not be negative"
	}
	if strings.TrimSpace(string(req.Severity)) == "" {
		return "severity is required"
	}
	if req.DatasourceID != "" {
		if _, err := h.d.Store.GetDatasource(r.Context(), req.DatasourceID); err != nil {
			return "datasource_id does not reference a known datasource"
		}
	}
	return ""
}

func (h *handler) rulesChanged() {
	if h.d.OnRulesChanged != nil {
		h.d.OnRulesChanged()
	}
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_data", "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func newID() string { return uuid.NewString() }
