package api_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pod32g/omni-metrics/internal/api"
	"github.com/pod32g/omni-metrics/internal/promql"
	"github.com/pod32g/omni-metrics/internal/push"
	"github.com/pod32g/omni-metrics/internal/tsdb"
)

func buildPushAPI(t *testing.T, maxSeries, sampleLimit int, token string) (http.Handler, *tsdb.DB) {
	t.Helper()
	db, err := tsdb.Open(tsdb.Options{MaxSeries: maxSeries})
	if err != nil {
		t.Fatal(err)
	}
	ing := push.NewIngester(db, sampleLimit)
	h := api.New(api.Options{
		Engine:      promql.NewEngine(db),
		Storage:     db,
		Version:     "test",
		HeadSeries:  db.HeadSeries,
		Push:        ing,
		PushSources: ing,
		PushConfig:  api.PushConfig{Enabled: true, MaxBodyBytes: 1 << 20, AuthToken: token},
	})
	return h, db
}

func postJSON(t *testing.T, h http.Handler, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestPushSuccess(t *testing.T) {
	h, db := buildPushAPI(t, 0, 0, "")
	body := `{"job":"app","instance":"i1","series":[{"name":"reqs_total","value":5},{"name":"q","value":2}]}`
	rec := postJSON(t, h, "/api/v1/push", "", body)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"status":"success"`) {
		t.Fatalf("code %d body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"samplesAppended":2`) {
		t.Errorf("missing count: %s", rec.Body.String())
	}
	if db.HeadSeries() != 2 {
		t.Errorf("head series = %d, want 2", db.HeadSeries())
	}
}

func TestPushBadJSON(t *testing.T) {
	h, _ := buildPushAPI(t, 0, 0, "")
	rec := postJSON(t, h, "/api/v1/push", "", `{not json`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"errorType":"bad_data"`) {
		t.Fatalf("code %d body %s", rec.Code, rec.Body.String())
	}
}

func TestPushValidationError(t *testing.T) {
	h, _ := buildPushAPI(t, 0, 0, "")
	rec := postJSON(t, h, "/api/v1/push", "", `{"job":"","series":[]}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"errorType":"bad_data"`) {
		t.Fatalf("code %d body %s", rec.Code, rec.Body.String())
	}
}

func TestPushCardinalityCapIs503(t *testing.T) {
	h, _ := buildPushAPI(t, 1, 0, "")
	body := `{"job":"app","series":[{"name":"a","value":1},{"name":"b","value":2}]}`
	rec := postJSON(t, h, "/api/v1/push", "", body)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), `"errorType":"internal"`) {
		t.Fatalf("code %d body %s", rec.Code, rec.Body.String())
	}
}

func TestPushBodyTooLargeIs413(t *testing.T) {
	h, _ := buildPushAPI(t, 0, 0, "")
	big := `{"job":"app","series":[{"name":"a","value":1}],"pad":"` + strings.Repeat("x", 2<<20) + `"}`
	rec := postJSON(t, h, "/api/v1/push", "", big)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code %d body %s", rec.Code, rec.Body.String())
	}
}

func TestPushAuth(t *testing.T) {
	h, _ := buildPushAPI(t, 0, 0, "s3cr3t")
	body := `{"job":"app","series":[{"name":"a","value":1}]}`
	// missing token
	if rec := postJSON(t, h, "/api/v1/push", "", body); rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing token: code %d", rec.Code)
	}
	// wrong token
	if rec := postJSON(t, h, "/api/v1/push", "nope", body); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: code %d", rec.Code)
	}
	// right token
	if rec := postJSON(t, h, "/api/v1/push", "s3cr3t", body); rec.Code != 200 {
		t.Fatalf("right token: code %d body %s", rec.Code, rec.Body.String())
	}
}

func TestPushAuthRequiresBearerScheme(t *testing.T) {
	h, _ := buildPushAPI(t, 0, 0, "s3cr3t")
	// A raw token without the "Bearer " scheme must be rejected, per the spec.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/push", strings.NewReader(`{"job":"app","series":[{"name":"a","value":1}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "s3cr3t")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("raw token without Bearer scheme: code %d, want 401", rec.Code)
	}
}

func TestPushSourcesEndpoint(t *testing.T) {
	h, _ := buildPushAPI(t, 0, 0, "")
	postJSON(t, h, "/api/v1/push", "", `{"job":"app","instance":"i1","series":[{"name":"a","value":1}]}`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/push/sources", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"job":"app"`) {
		t.Fatalf("sources: code %d body %s", rec.Code, rec.Body.String())
	}
}

func TestPushDisabledNotRegistered(t *testing.T) {
	db, _ := tsdb.Open(tsdb.Options{})
	h := api.New(api.Options{
		Engine:     promql.NewEngine(db),
		Storage:    db,
		Version:    "test",
		HeadSeries: db.HeadSeries,
		// Push/PushSources nil, PushConfig.Enabled false
	})
	rec := postJSON(t, h, "/api/v1/push", "", `{"job":"app","series":[{"name":"a","value":1}]}`)
	if rec.Code == 200 {
		t.Errorf("push should not be served when disabled")
	}
}
