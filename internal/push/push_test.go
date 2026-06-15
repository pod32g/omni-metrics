package push

import (
	"testing"

	"github.com/pod32g/omni-metrics/internal/model"
)

func TestRequestValidate(t *testing.T) {
	v := func(f float64) *Value { x := Value(f); return &x }
	cases := []struct {
		name string
		req  Request
		ok   bool
	}{
		{name: "ok value", req: Request{Job: "j", Series: []SeriesInput{{Name: "m", Value: v(1)}}}, ok: true},
		{name: "ok samples", req: Request{Job: "j", Series: []SeriesInput{{Name: "m", Samples: []SamplePoint{{Value: 1}}}}}, ok: true},
		{name: "empty job", req: Request{Series: []SeriesInput{{Name: "m", Value: v(1)}}}},
		{name: "whitespace-only job", req: Request{Job: "  \t", Series: []SeriesInput{{Name: "m", Value: v(1)}}}},
		{name: "no series", req: Request{Job: "j"}},
		{name: "empty name", req: Request{Job: "j", Series: []SeriesInput{{Value: v(1)}}}},
		{name: "bad metric name", req: Request{Job: "j", Series: []SeriesInput{{Name: "1bad", Value: v(1)}}}},
		{name: "neither value nor samples", req: Request{Job: "j", Series: []SeriesInput{{Name: "m"}}}},
		{name: "both value and samples", req: Request{Job: "j", Series: []SeriesInput{{Name: "m", Value: v(1), Samples: []SamplePoint{{Value: 1}}}}}},
		{name: "empty samples", req: Request{Job: "j", Series: []SeriesInput{{Name: "m", Samples: []SamplePoint{}}}}},
		{name: "bad label name", req: Request{Job: "j", Series: []SeriesInput{{Name: "m", Labels: map[string]string{"a-b": "1"}, Value: v(1)}}}},
		{name: "reserved __ label", req: Request{Job: "j", Series: []SeriesInput{{Name: "m", Labels: map[string]string{"__x": "1"}, Value: v(1)}}}},
		{name: "client job label allowed (overridden)", req: Request{Job: "j", Series: []SeriesInput{{Name: "m", Labels: map[string]string{"job": "evil"}, Value: v(1)}}}, ok: true},
	}
	for _, tc := range cases {
		err := tc.req.validate()
		if tc.ok && err != nil {
			t.Errorf("%s: unexpected error %v", tc.name, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("%s: expected validation error", tc.name)
		}
	}
}

func TestBuildLabelsOverridesReserved(t *testing.T) {
	s := SeriesInput{Name: "http_requests_total", Labels: map[string]string{
		"method":   "GET",
		"job":      "evil", // must be overridden
		"instance": "evil", // must be overridden
	}}
	got := buildLabels(s, "realjob", "realinst")
	want := model.FromStrings(
		model.MetricName, "http_requests_total",
		"method", "GET",
		"job", "realjob",
		"instance", "realinst",
	)
	if !got.Equal(want) {
		t.Errorf("buildLabels = %s, want %s", got.String(), want.String())
	}
}
