package api_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func postForm(t *testing.T, h http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestQueryAcceptsPOST guards Grafana's default behaviour: it POSTs form-encoded
// queries. They must return the JSON envelope, not the SPA HTML.
func TestQueryAcceptsPOST(t *testing.T) {
	h := buildAPI(t)
	rec := postForm(t, h, "/api/v1/query", "query=m&time=3")
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("POST query content-type = %q, want json (body=%s)", ct, rec.Body.String()[:min(80, len(rec.Body.String()))])
	}
	if !strings.Contains(rec.Body.String(), `"status":"success"`) {
		t.Errorf("POST query body = %s", rec.Body.String())
	}

	rr := postForm(t, h, "/api/v1/query_range", "query=m&start=1&end=3&step=1")
	if !strings.Contains(rr.Body.String(), `"resultType":"matrix"`) {
		t.Errorf("POST query_range body = %s", rr.Body.String())
	}
}

func TestUnknownAPIPathIsJSON404(t *testing.T) {
	h := buildAPI(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/does_not_exist", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown api path code = %d, want 404", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("unknown api path content-type = %q, want json", ct)
	}
	if !strings.Contains(rec.Body.String(), `"status":"error"`) {
		t.Errorf("unknown api path body = %s", rec.Body.String())
	}
}

func TestBuildInfo(t *testing.T) {
	h := buildAPI(t)
	code, body := getJSON(t, h, "/api/v1/status/buildinfo")
	if code != 200 || body["status"] != "success" {
		t.Fatalf("buildinfo status %d %v", code, body)
	}
	data := body["data"].(map[string]any)
	if data["version"] == nil || data["version"] == "" {
		t.Errorf("buildinfo missing version: %v", data)
	}
}

func TestMetadata(t *testing.T) {
	h := buildAPI(t)
	code, body := getJSON(t, h, "/api/v1/metadata")
	if code != 200 || body["status"] != "success" {
		t.Fatalf("metadata status %d %v", code, body)
	}
	if _, ok := body["data"].(map[string]any); !ok {
		t.Errorf("metadata data is not an object: %v", body["data"])
	}
}

// TestLabelValuesMatch checks the match[] filter Grafana's label_values() uses.
// buildAPI seeds m{job=a}, m{job=a,inst=2}, m{job=b}; filtering to inst="2" must
// leave only job="a".
func TestLabelValuesMatch(t *testing.T) {
	h := buildAPI(t)
	_, all := getJSON(t, h, "/api/v1/label/job/values")
	if got := len(all["data"].([]any)); got != 2 {
		t.Fatalf("unfiltered job values = %d, want 2", got)
	}
	_, filtered := getJSON(t, h, `/api/v1/label/job/values?match[]=m{inst="2"}`)
	vals := filtered["data"].([]any)
	if len(vals) != 1 || vals[0] != "a" {
		t.Errorf("match-filtered job values = %v, want [a]", vals)
	}
}

func TestSPAStillServedForNonAPI(t *testing.T) {
	h := buildAPI(t)
	for _, p := range []string{"/", "/graph", "/some/spa/route"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ui") {
			t.Errorf("%s: SPA not served (code %d, body %q)", p, rec.Code, rec.Body.String())
		}
	}
}

// TestWrongMethodOnOperationalEndpoints: a non-GET to /metrics or the health
// probes must return 405, not the SPA HTML.
func TestWrongMethodOnOperationalEndpoints(t *testing.T) {
	h := buildAPI(t)
	for _, p := range []string{"/metrics", "/-/healthy", "/-/ready"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, p, nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s = %d, want 405", p, rec.Code)
		}
		if rec.Header().Get("Allow") != "GET" {
			t.Errorf("POST %s Allow = %q, want GET", p, rec.Header().Get("Allow"))
		}
	}
	// GET still works.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/-/ready", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("GET /-/ready = %d, want 200", rec.Code)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
