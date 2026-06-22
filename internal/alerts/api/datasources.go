package api

import (
	"net/http"
	"strings"

	"github.com/pod32g/omni-metrics/internal/alerts/models"
)

// datasourceRequest is the writable shape of a datasource. Credentials and the
// basic-auth password are write-only: they are accepted here but never rendered
// back (models.Datasource hides them with json:"-").
type datasourceRequest struct {
	Name        string            `json:"name"`
	Type        string            `json:"type"`
	BaseURL     string            `json:"base_url"`
	AuthType    models.AuthType   `json:"auth_type"`
	Credentials string            `json:"credentials"`
	BasicUser   string            `json:"basic_user"`
	BasicPass   string            `json:"basic_pass"`
	Headers     map[string]string `json:"headers"`
	TimeoutMS   int               `json:"timeout_ms"`
	Enabled     *bool             `json:"enabled"`
}

func (h *handler) listDatasources(w http.ResponseWriter, r *http.Request) {
	list, err := h.d.Store.ListDatasources(r.Context())
	if err != nil {
		mapStoreErr(w, err)
		return
	}
	if list == nil {
		list = []models.Datasource{}
	}
	writeData(w, http.StatusOK, list)
}

func (h *handler) getDatasource(w http.ResponseWriter, r *http.Request) {
	ds, err := h.d.Store.GetDatasource(r.Context(), r.PathValue("id"))
	if err != nil {
		mapStoreErr(w, err)
		return
	}
	writeData(w, http.StatusOK, ds)
}

func (h *handler) createDatasource(w http.ResponseWriter, r *http.Request) {
	var req datasourceRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if msg := validateDatasource(&req); msg != "" {
		writeErr(w, http.StatusBadRequest, "bad_data", msg)
		return
	}
	now := h.d.Now()
	ds := models.Datasource{
		ID:          newID(),
		Name:        strings.TrimSpace(req.Name),
		Type:        defaultType(req.Type),
		BaseURL:     strings.TrimSpace(req.BaseURL),
		AuthType:    normalizeAuth(req.AuthType),
		Credentials: req.Credentials,
		BasicUser:   req.BasicUser,
		BasicPass:   req.BasicPass,
		Headers:     req.Headers,
		TimeoutMS:   req.TimeoutMS,
		Enabled:     req.Enabled == nil || *req.Enabled,
		Source:      models.SourceAPI,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := h.d.Store.PutDatasource(r.Context(), ds); err != nil {
		// A duplicate name is a client error, not a server error.
		writeErr(w, http.StatusConflict, "conflict", err.Error())
		return
	}
	writeData(w, http.StatusCreated, ds)
}

func (h *handler) updateDatasource(w http.ResponseWriter, r *http.Request) {
	existing, err := h.d.Store.GetDatasource(r.Context(), r.PathValue("id"))
	if err != nil {
		mapStoreErr(w, err)
		return
	}
	if !existing.Editable() {
		writeErr(w, http.StatusConflict, "read_only", "datasource is managed by configuration and cannot be modified via the API")
		return
	}
	var req datasourceRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if msg := validateDatasource(&req); msg != "" {
		writeErr(w, http.StatusBadRequest, "bad_data", msg)
		return
	}
	existing.Name = strings.TrimSpace(req.Name)
	existing.Type = defaultType(req.Type)
	existing.BaseURL = strings.TrimSpace(req.BaseURL)
	existing.AuthType = normalizeAuth(req.AuthType)
	// Only overwrite secrets when a non-empty value is supplied, so clients can
	// update other fields without re-sending credentials.
	if req.Credentials != "" {
		existing.Credentials = req.Credentials
	}
	if req.BasicPass != "" {
		existing.BasicPass = req.BasicPass
	}
	existing.BasicUser = req.BasicUser
	existing.Headers = req.Headers
	existing.TimeoutMS = req.TimeoutMS
	if req.Enabled != nil {
		existing.Enabled = *req.Enabled
	}
	existing.UpdatedAt = h.d.Now()
	if err := h.d.Store.PutDatasource(r.Context(), existing); err != nil {
		writeErr(w, http.StatusConflict, "conflict", err.Error())
		return
	}
	writeData(w, http.StatusOK, existing)
}

func (h *handler) deleteDatasource(w http.ResponseWriter, r *http.Request) {
	existing, err := h.d.Store.GetDatasource(r.Context(), r.PathValue("id"))
	if err != nil {
		mapStoreErr(w, err)
		return
	}
	if !existing.Editable() {
		writeErr(w, http.StatusConflict, "read_only", "datasource is managed by configuration and cannot be deleted via the API")
		return
	}
	if err := h.d.Store.DeleteDatasource(r.Context(), existing.ID); err != nil {
		mapStoreErr(w, err)
		return
	}
	writeData(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (h *handler) testDatasource(w http.ResponseWriter, r *http.Request) {
	ds, err := h.d.Store.GetDatasource(r.Context(), r.PathValue("id"))
	if err != nil {
		mapStoreErr(w, err)
		return
	}
	if h.d.TestDatasource == nil {
		writeErr(w, http.StatusServiceUnavailable, "unavailable", "datasource testing not available")
		return
	}
	if err := h.d.TestDatasource(r.Context(), ds); err != nil {
		writeErr(w, http.StatusBadGateway, "datasource_unreachable", err.Error())
		return
	}
	writeData(w, http.StatusOK, map[string]string{"status": "ok"})
}

func validateDatasource(req *datasourceRequest) string {
	if strings.TrimSpace(req.Name) == "" {
		return "name is required"
	}
	if strings.TrimSpace(req.BaseURL) == "" {
		return "base_url is required"
	}
	switch normalizeAuth(req.AuthType) {
	case models.AuthNone, models.AuthBearer, models.AuthBasic:
	default:
		return "auth_type must be one of none, bearer, basic"
	}
	return ""
}

func normalizeAuth(a models.AuthType) models.AuthType {
	if a == "" {
		return models.AuthNone
	}
	return a
}

func defaultType(t string) string {
	if strings.TrimSpace(t) == "" {
		return "prometheus"
	}
	return t
}
