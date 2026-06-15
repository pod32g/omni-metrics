package push

import (
	"fmt"
	"strings"

	"github.com/pod32g/omni-metrics/internal/model"
)

// Request is the JSON push body. The /api/v1/ route pins the schema version.
type Request struct {
	Job      string        `json:"job"`
	Instance string        `json:"instance"`
	Series   []SeriesInput `json:"series"`
}

// SeriesInput is one metric in a push: a name, optional extra labels, and either
// a single shorthand Value (stamped at request time) or explicit Samples.
type SeriesInput struct {
	Name    string            `json:"name"`
	Labels  map[string]string `json:"labels"`
	Value   *Value            `json:"value"`
	Samples []SamplePoint     `json:"samples"`
}

// SamplePoint is one explicit observation. TimestampMs == 0 means request time.
type SamplePoint struct {
	TimestampMs int64 `json:"timestamp_ms"`
	Value       Value `json:"value"`
}

// reservedLabels are set by the server and may not be supplied by a client; if
// present in Labels they are silently dropped (server values win).
var reservedLabels = map[string]struct{}{model.MetricName: {}, "job": {}, "instance": {}}

func (r *Request) validate() error {
	if r.Job == "" {
		return fmt.Errorf("job must not be empty")
	}
	if len(r.Series) == 0 {
		return fmt.Errorf("series must not be empty")
	}
	for i := range r.Series {
		if err := r.Series[i].validate(); err != nil {
			return fmt.Errorf("series[%d]: %w", i, err)
		}
	}
	return nil
}

func (s *SeriesInput) validate() error {
	if !isValidMetricName(s.Name) {
		return fmt.Errorf("invalid metric name %q", s.Name)
	}
	hasValue := s.Value != nil
	hasSamples := len(s.Samples) > 0
	switch {
	case hasValue && hasSamples:
		return fmt.Errorf("set exactly one of value or samples, not both")
	case !hasValue && !hasSamples:
		// An explicitly empty samples array also lands here.
		return fmt.Errorf("set exactly one of value or samples")
	}
	for name := range s.Labels {
		if _, reserved := reservedLabels[name]; reserved {
			continue // dropped/overridden by the server, not an error
		}
		if strings.HasPrefix(name, "__") {
			return fmt.Errorf("label name %q uses the reserved __ prefix", name)
		}
		if !isValidLabelName(name) {
			return fmt.Errorf("invalid label name %q", name)
		}
	}
	return nil
}

// buildLabels assembles the stored label set: __name__ from Name, the client's
// non-reserved labels, then the server-owned job and instance (which override any
// client-supplied values).
func buildLabels(s SeriesInput, job, instance string) model.Labels {
	m := make(map[string]string, len(s.Labels)+3)
	for k, v := range s.Labels {
		if _, reserved := reservedLabels[k]; reserved {
			continue
		}
		m[k] = v
	}
	m[model.MetricName] = s.Name
	m["job"] = job
	m["instance"] = instance
	return model.FromMap(m)
}

// samplePoints expands a series into concrete (timestamp, value) pairs, using
// nowMs for the shorthand Value and for any sample with TimestampMs == 0.
func (s *SeriesInput) samplePoints(nowMs int64) []SamplePoint {
	if s.Value != nil {
		return []SamplePoint{{TimestampMs: nowMs, Value: *s.Value}}
	}
	out := make([]SamplePoint, len(s.Samples))
	for i, p := range s.Samples {
		if p.TimestampMs == 0 {
			p.TimestampMs = nowMs
		}
		out[i] = p
	}
	return out
}

func isValidMetricName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := c == '_' || c == ':' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		if i > 0 {
			ok = ok || (c >= '0' && c <= '9')
		}
		if !ok {
			return false
		}
	}
	return true
}

func isValidLabelName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		if i > 0 {
			ok = ok || (c >= '0' && c <= '9')
		}
		if !ok {
			return false
		}
	}
	return true
}
