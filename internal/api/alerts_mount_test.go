package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAlertHandlerMounted(t *testing.T) {
	ah := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ALERTS:" + r.URL.Path))
	})
	a := New(Options{
		Version:      "test",
		AlertHandler: ah,
		ExtraCollectors: []func(io.Writer){
			func(w io.Writer) { _, _ = io.WriteString(w, "omni_alert_rules_total 0\n") },
		},
	})

	for _, path := range []string{"/api/v1/alerts", "/api/v1/alerts/active", "/api/v1/datasources", "/api/v1/datasources/abc"} {
		w := httptest.NewRecorder()
		a.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != http.StatusOK || !strings.HasPrefix(w.Body.String(), "ALERTS:") {
			t.Errorf("%s routed to %d %q, want alert handler", path, w.Code, w.Body.String())
		}
	}

	// /metrics includes the extra collector output.
	w := httptest.NewRecorder()
	a.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(w.Body.String(), "omni_alert_rules_total 0") {
		t.Errorf("/metrics missing extra collector output:\n%s", w.Body.String())
	}
	// Still includes core self-metrics.
	if !strings.Contains(w.Body.String(), "omni_build_info") {
		t.Errorf("/metrics missing core self metrics")
	}
}

func TestNoAlertHandlerKeepsAPINotFound(t *testing.T) {
	a := New(Options{Version: "test"})
	w := httptest.NewRecorder()
	a.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/alerts", nil))
	// Without an alert handler, it falls to the JSON API-not-found.
	if w.Code != http.StatusNotFound {
		t.Errorf("code = %d, want 404 when alerting disabled", w.Code)
	}
}
