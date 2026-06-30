package alerts_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pod32g/omni-metrics/internal/alerts"
	"github.com/pod32g/omni-metrics/internal/alerts/models"
	"github.com/pod32g/omni-metrics/internal/alerts/notify"
)

func promServer(t *testing.T, fire bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fire {
			w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"instance":"a"},"value":[1,"1"]}]}}`))
		} else {
			w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
		}
	}))
}

func newService(t *testing.T, dsURL string) *alerts.Service {
	t.Helper()
	svc, err := alerts.NewService(alerts.Options{
		StorePath: ":memory:",
		Datasources: []models.Datasource{
			{ID: "local", Name: "local", Type: "prometheus", BaseURL: dsURL, AuthType: models.AuthNone, Enabled: true, Source: models.SourceBuiltin, TimeoutMS: 2000},
		},
		DefaultDatasource: "local",
		Now:               time.Now,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	t.Cleanup(func() { svc.Stop() })
	return svc
}

func req(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestServiceSeedsDatasources(t *testing.T) {
	srv := promServer(t, true)
	defer srv.Close()
	svc := newService(t, srv.URL)
	h := svc.Handler()

	w := req(t, h, "GET", "/api/v1/datasources", "")
	var env struct {
		Data []models.Datasource `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &env)
	if len(env.Data) != 1 || env.Data[0].Name != "local" || env.Data[0].Source != models.SourceBuiltin {
		t.Fatalf("seeded datasources = %+v", env.Data)
	}
}

func TestServiceEndToEndFiring(t *testing.T) {
	srv := promServer(t, true)
	defer srv.Close()
	svc := newService(t, srv.URL)
	h := svc.Handler()

	// Create a rule with for=0 (fires immediately) and no explicit datasource
	// (defaults to local).
	w := req(t, h, "POST", "/api/v1/alerts", `{"name":"always","promql":"vector(1)","evaluation_interval_seconds":15,"for_duration_seconds":0,"severity":"critical"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create rule = %d body=%s", w.Code, w.Body.String())
	}

	// Evaluate synchronously via the API.
	w = req(t, h, "POST", "/api/v1/alerts/evaluate", "")
	if w.Code != http.StatusOK {
		t.Fatalf("evaluate = %d body=%s", w.Code, w.Body.String())
	}

	var active struct {
		Data []models.Instance `json:"data"`
	}
	json.Unmarshal(req(t, h, "GET", "/api/v1/alerts/active", "").Body.Bytes(), &active)
	if len(active.Data) != 1 || active.Data[0].StateName != "firing" {
		t.Fatalf("active = %+v", active.Data)
	}

	var events struct {
		Data []models.HistoryEntry `json:"data"`
	}
	json.Unmarshal(req(t, h, "GET", "/api/v1/alerts/events", "").Body.Bytes(), &events)
	if len(events.Data) != 1 || events.Data[0].NewName != "firing" {
		t.Fatalf("events = %+v", events.Data)
	}

	// Metrics reflect the firing alert.
	var b strings.Builder
	svc.Collector()(&b)
	if !strings.Contains(b.String(), "omni_alerts_active 1") {
		t.Errorf("metrics missing active gauge:\n%s", b.String())
	}
	if !strings.Contains(b.String(), `omni_alert_evaluations_total{result="success"}`) {
		t.Errorf("metrics missing eval counter:\n%s", b.String())
	}
}

func TestServiceResolvesWhenConditionClears(t *testing.T) {
	var firing atomic.Bool
	firing.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if firing.Load() {
			w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"instance":"a"},"value":[1,"1"]}]}}`))
		} else {
			w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
		}
	}))
	defer srv.Close()
	svc := newService(t, srv.URL)
	h := svc.Handler()

	req(t, h, "POST", "/api/v1/alerts", `{"name":"x","promql":"vector(1)","evaluation_interval_seconds":15,"severity":"warning"}`)
	req(t, h, "POST", "/api/v1/alerts/evaluate", "")

	firing.Store(false) // condition clears
	req(t, h, "POST", "/api/v1/alerts/evaluate", "")

	var active struct {
		Data []models.Instance `json:"data"`
	}
	json.Unmarshal(req(t, h, "GET", "/api/v1/alerts/active", "").Body.Bytes(), &active)
	if len(active.Data) != 0 {
		t.Fatalf("alert not resolved: %+v", active.Data)
	}
}

func TestServiceForwardsFiringToNotify(t *testing.T) {
	type recv struct {
		auth  string
		event map[string]any
	}
	received := make(chan recv, 4)
	notifySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ev map[string]any
		json.NewDecoder(r.Body).Decode(&ev)
		received <- recv{auth: r.Header.Get("Authorization"), event: ev}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer notifySrv.Close()

	prom := promServer(t, true)
	defer prom.Close()

	svc, err := alerts.NewService(alerts.Options{
		StorePath: ":memory:",
		Datasources: []models.Datasource{
			{ID: "local", Name: "local", Type: "prometheus", BaseURL: prom.URL, AuthType: models.AuthNone, Enabled: true, Source: models.SourceBuiltin, TimeoutMS: 2000},
		},
		DefaultDatasource: "local",
		Now:               time.Now,
		Notify:            notify.Config{Enabled: true, URL: notifySrv.URL, Token: "tok"},
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	svc.Start(context.Background())
	defer svc.Stop()
	h := svc.Handler()

	req(t, h, "POST", "/api/v1/alerts", `{"name":"always","promql":"vector(1)","evaluation_interval_seconds":15,"for_duration_seconds":0,"severity":"critical"}`)
	req(t, h, "POST", "/api/v1/alerts/evaluate", "")

	select {
	case got := <-received:
		if got.auth != "Bearer tok" {
			t.Errorf("auth = %q, want Bearer tok", got.auth)
		}
		ev := got.event
		if ev["status"] != "firing" {
			t.Errorf("status = %v, want firing", ev["status"])
		}
		if ev["source"] != "omni-metrics" {
			t.Errorf("source = %v, want omni-metrics", ev["source"])
		}
		if ev["severity"] != "critical" {
			t.Errorf("severity = %v, want critical", ev["severity"])
		}
		if ev["title"] != "always" {
			t.Errorf("title = %v, want always", ev["title"])
		}
	case <-time.After(3 * time.Second):
		t.Fatal("omni-notify received no event")
	}

	var b strings.Builder
	svc.Collector()(&b)
	if !strings.Contains(b.String(), "omni_alerts_notify_sent_total") {
		t.Errorf("collector missing notify metrics:\n%s", b.String())
	}
}

func TestServiceNotifyDisabledSendsNothing(t *testing.T) {
	var hits int32
	notifySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer notifySrv.Close()

	prom := promServer(t, true)
	defer prom.Close()

	// No Notify config -> disabled -> no dispatcher, no enqueues ever.
	svc := newService(t, prom.URL)
	svc.Start(context.Background())
	defer svc.Stop()
	h := svc.Handler()

	req(t, h, "POST", "/api/v1/alerts", `{"name":"x","promql":"vector(1)","for_duration_seconds":0,"severity":"critical"}`)
	req(t, h, "POST", "/api/v1/alerts/evaluate", "")

	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Errorf("omni-notify hit %d times with notify disabled, want 0", n)
	}
}

func TestServiceCreateRuleReconcilesScheduler(t *testing.T) {
	srv := promServer(t, true)
	defer srv.Close()
	svc := newService(t, srv.URL)
	svc.Start(context.Background())
	h := svc.Handler()
	// Creating an enabled rule should not error and should be schedulable.
	w := req(t, h, "POST", "/api/v1/alerts", `{"name":"x","promql":"vector(1)","evaluation_interval_seconds":15,"severity":"warning","enabled":true}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d", w.Code)
	}
}
