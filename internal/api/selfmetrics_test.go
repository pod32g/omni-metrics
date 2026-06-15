package api

import (
	"strings"
	"testing"
)

func TestSelfMetricsPushExposition(t *testing.T) {
	s := NewSelfMetrics("test", nil)
	s.IncPushRequest(true)
	s.IncPushRequest(false)
	s.AddPushSamples(7)
	s.IncPushRejected()

	var b strings.Builder
	s.WriteExposition(&b)
	out := b.String()
	for _, want := range []string{
		`omni_push_requests_total{status="success"} 1`,
		`omni_push_requests_total{status="error"} 1`,
		`omni_push_samples_appended_total 7`,
		`omni_push_series_rejected_total 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("exposition missing %q\n---\n%s", want, out)
		}
	}
}
