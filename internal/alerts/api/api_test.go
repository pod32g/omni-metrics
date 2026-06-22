package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pod32g/omni-metrics/internal/alerts/api"
	"github.com/pod32g/omni-metrics/internal/alerts/models"
	"github.com/pod32g/omni-metrics/internal/alerts/storage"
)

func testHandler(t *testing.T) (http.Handler, storage.Store, *int) {
	t.Helper()
	st, err := storage.OpenSQLite(":memory:")
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	reconciles := 0
	h := api.New(api.Deps{
		Store:          st,
		Evaluate:       func(context.Context, string) (api.EvalResult, error) { return api.EvalResult{Active: 1}, nil },
		EvaluateAll:    func(context.Context) (int, error) { return 1, nil },
		TestDatasource: func(context.Context, models.Datasource) error { return nil },
		OnRulesChanged: func() { reconciles++ },
		Now:            func() time.Time { return time.Unix(1000, 0).UTC() },
	})
	return h, st, &reconciles
}

func do(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func decodeData(t *testing.T, w *httptest.ResponseRecorder, dst any) {
	t.Helper()
	var env struct {
		Status string          `json:"status"`
		Data   json.RawMessage `json:"data"`
		Error  string          `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v (body=%s)", err, w.Body.String())
	}
	if env.Status != "success" {
		t.Fatalf("status=%s error=%s code=%d", env.Status, env.Error, w.Code)
	}
	if dst != nil {
		if err := json.Unmarshal(env.Data, dst); err != nil {
			t.Fatalf("decode data: %v", err)
		}
	}
}

func seedDatasource(t *testing.T, st storage.Store, id, name, source string) {
	t.Helper()
	_ = st.PutDatasource(context.Background(), models.Datasource{ID: id, Name: name, Type: "prometheus", BaseURL: "http://x", AuthType: models.AuthNone, Enabled: true, Source: source})
}

func TestCreateGetListRule(t *testing.T) {
	h, st, recon := testHandler(t)
	seedDatasource(t, st, "ds1", "local", models.SourceBuiltin)

	body := `{"name":"High errors","datasource_id":"ds1","promql":"up == 0","evaluation_interval_seconds":15,"for_duration_seconds":60,"severity":"critical","labels":{"team":"x"}}`
	w := do(t, h, "POST", "/api/v1/alerts", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("create code = %d, body=%s", w.Code, w.Body.String())
	}
	var created models.Rule
	decodeData(t, w, &created)
	if created.ID == "" || created.Name != "High errors" || created.PromQL != "up == 0" {
		t.Fatalf("created = %+v", created)
	}
	if *recon == 0 {
		t.Error("OnRulesChanged not called on create")
	}

	w = do(t, h, "GET", "/api/v1/alerts/"+created.ID, "")
	if w.Code != http.StatusOK {
		t.Fatalf("get code = %d", w.Code)
	}
	w = do(t, h, "GET", "/api/v1/alerts", "")
	var list []models.Rule
	decodeData(t, w, &list)
	if len(list) != 1 {
		t.Fatalf("list = %d", len(list))
	}
}

func TestCreateRuleValidation(t *testing.T) {
	h, _, _ := testHandler(t)
	// Missing promql.
	w := do(t, h, "POST", "/api/v1/alerts", `{"name":"x","evaluation_interval_seconds":15,"severity":"warning"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
	// Missing name.
	w = do(t, h, "POST", "/api/v1/alerts", `{"promql":"up==0","evaluation_interval_seconds":15,"severity":"warning"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing name, got %d", w.Code)
	}
	// Non-existent datasource.
	w = do(t, h, "POST", "/api/v1/alerts", `{"name":"x","promql":"up==0","evaluation_interval_seconds":15,"severity":"warning","datasource_id":"nope"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad datasource, got %d", w.Code)
	}
}

func TestUpdateDeleteRule(t *testing.T) {
	h, st, _ := testHandler(t)
	seedDatasource(t, st, "ds1", "local", models.SourceBuiltin)
	w := do(t, h, "POST", "/api/v1/alerts", `{"name":"n","datasource_id":"ds1","promql":"up==0","evaluation_interval_seconds":15,"severity":"warning"}`)
	var r models.Rule
	decodeData(t, w, &r)

	w = do(t, h, "PUT", "/api/v1/alerts/"+r.ID, `{"name":"renamed","datasource_id":"ds1","promql":"up==1","evaluation_interval_seconds":30,"severity":"critical"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("update code = %d body=%s", w.Code, w.Body.String())
	}
	got, _ := st.GetRule(context.Background(), r.ID)
	if got.Name != "renamed" || got.PromQL != "up==1" || got.EvalIntervalS != 30 {
		t.Fatalf("update not applied: %+v", got)
	}

	w = do(t, h, "DELETE", "/api/v1/alerts/"+r.ID, "")
	if w.Code != http.StatusOK && w.Code != http.StatusNoContent {
		t.Fatalf("delete code = %d", w.Code)
	}
	if _, err := st.GetRule(context.Background(), r.ID); err == nil {
		t.Error("rule not deleted")
	}

	w = do(t, h, "GET", "/api/v1/alerts/missing", "")
	if w.Code != http.StatusNotFound {
		t.Errorf("get missing = %d, want 404", w.Code)
	}
}

func TestEnableDisable(t *testing.T) {
	h, st, _ := testHandler(t)
	seedDatasource(t, st, "ds1", "local", models.SourceBuiltin)
	w := do(t, h, "POST", "/api/v1/alerts", `{"name":"n","datasource_id":"ds1","promql":"up==0","evaluation_interval_seconds":15,"severity":"warning","enabled":true}`)
	var r models.Rule
	decodeData(t, w, &r)

	do(t, h, "POST", "/api/v1/alerts/"+r.ID+"/disable", "")
	got, _ := st.GetRule(context.Background(), r.ID)
	if got.Enabled {
		t.Error("disable failed")
	}
	do(t, h, "POST", "/api/v1/alerts/"+r.ID+"/enable", "")
	got, _ = st.GetRule(context.Background(), r.ID)
	if !got.Enabled {
		t.Error("enable failed")
	}
}

func TestEvaluateEndpoints(t *testing.T) {
	h, st, _ := testHandler(t)
	seedDatasource(t, st, "ds1", "local", models.SourceBuiltin)
	w := do(t, h, "POST", "/api/v1/alerts", `{"name":"n","datasource_id":"ds1","promql":"up==0","evaluation_interval_seconds":15,"severity":"warning"}`)
	var r models.Rule
	decodeData(t, w, &r)

	w = do(t, h, "POST", "/api/v1/alerts/"+r.ID+"/evaluate", "")
	if w.Code != http.StatusOK {
		t.Fatalf("evaluate one = %d", w.Code)
	}
	w = do(t, h, "POST", "/api/v1/alerts/evaluate", "")
	if w.Code != http.StatusOK {
		t.Fatalf("evaluate all = %d", w.Code)
	}
}

func TestActiveHistoryEvents(t *testing.T) {
	h, st, _ := testHandler(t)
	ctx := context.Background()
	now := time.Unix(1000, 0).UTC()
	_ = st.UpsertInstance(ctx, models.Instance{ID: "i1", RuleID: "r1", Fingerprint: "fp", State: models.StateFiring, StateName: "firing", ActiveAt: now, StartedAt: now, UpdatedAt: now})
	_, _ = st.AppendHistory(ctx, models.HistoryEntry{RuleID: "r1", Fingerprint: "fp", PrevName: "ok", NewName: "firing", Timestamp: now})
	_, _ = st.AppendHistory(ctx, models.HistoryEntry{RuleID: "r1", Fingerprint: "fp", PrevName: "firing", NewName: "resolved", Timestamp: now})

	var active []models.Instance
	decodeData(t, do(t, h, "GET", "/api/v1/alerts/active", ""), &active)
	if len(active) != 1 {
		t.Fatalf("active = %d", len(active))
	}
	var hist []models.HistoryEntry
	decodeData(t, do(t, h, "GET", "/api/v1/alerts/history", ""), &hist)
	if len(hist) != 2 {
		t.Fatalf("history = %d", len(hist))
	}
	var events []models.HistoryEntry
	decodeData(t, do(t, h, "GET", "/api/v1/alerts/events?since="+itoa(hist[0].ID), ""), &events)
	if len(events) != 1 || events[0].ID != hist[1].ID {
		t.Fatalf("events since cursor = %+v", events)
	}
}

func TestDatasourceCRUDAndReadOnly(t *testing.T) {
	h, st, _ := testHandler(t)
	// API create.
	w := do(t, h, "POST", "/api/v1/datasources", `{"name":"remote","type":"prometheus","base_url":"http://r","auth_type":"bearer","credentials":"secret","timeout_ms":5000,"enabled":true}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create ds = %d body=%s", w.Code, w.Body.String())
	}
	var ds models.Datasource
	decodeData(t, w, &ds)
	if ds.ID == "" || ds.Source != models.SourceAPI {
		t.Fatalf("created ds = %+v", ds)
	}
	// Response must NOT leak the credential.
	if strings.Contains(w.Body.String(), "secret") {
		t.Error("credential leaked in datasource response")
	}
	// But it is persisted.
	stored, _ := st.GetDatasource(context.Background(), ds.ID)
	if stored.Credentials != "secret" {
		t.Errorf("credential not persisted: %q", stored.Credentials)
	}

	// Config-sourced datasource is read-only via API.
	seedDatasource(t, st, "cfg1", "configured", models.SourceConfig)
	w = do(t, h, "PUT", "/api/v1/datasources/cfg1", `{"name":"configured","type":"prometheus","base_url":"http://y","auth_type":"none"}`)
	if w.Code != http.StatusConflict {
		t.Errorf("edit config ds = %d, want 409", w.Code)
	}
	w = do(t, h, "DELETE", "/api/v1/datasources/cfg1", "")
	if w.Code != http.StatusConflict {
		t.Errorf("delete config ds = %d, want 409", w.Code)
	}

	// Test endpoint.
	w = do(t, h, "POST", "/api/v1/datasources/"+ds.ID+"/test", "")
	if w.Code != http.StatusOK {
		t.Errorf("test ds = %d", w.Code)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	h, _, _ := testHandler(t)
	w := do(t, h, "DELETE", "/api/v1/alerts", "")
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE /alerts = %d, want 405", w.Code)
	}
}

func itoa(n int64) string {
	var b bytes.Buffer
	_ = json.NewEncoder(&b).Encode(n)
	return strings.TrimSpace(b.String())
}
