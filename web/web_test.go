package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandlerServesIndexAndAssets(t *testing.T) {
	h := Handler()

	cases := []struct {
		path     string
		wantBody string
	}{
		{"/", "omni-metrics"},
		{"/some/spa/route", "omni-metrics"}, // SPA fallback to index.html
		{"/styles.css", "--accent"},
		{"/app.js", "renderChartInto"},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status %d", tc.path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), tc.wantBody) {
			t.Errorf("%s: body missing %q", tc.path, tc.wantBody)
		}
	}
}

func TestThemeTokensPresent(t *testing.T) {
	// Both themes must be defined for the dark/light requirement.
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/styles.css", nil))
	css := rec.Body.String()
	for _, tok := range []string{`[data-theme="dark"]`, `[data-theme="light"]`} {
		if !strings.Contains(css, tok) {
			t.Errorf("styles.css missing %s", tok)
		}
	}
}
