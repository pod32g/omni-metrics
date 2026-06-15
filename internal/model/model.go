// Package model defines the shared vocabulary of omni-metrics: labels that
// identify a time series, samples, metric types, and label matchers. It has no
// dependencies on other internal packages and is imported by all of them.
package model

// MetricName is the reserved label that holds a metric's name, e.g.
// {__name__="http_requests_total"}.
const MetricName = "__name__"

// Sample is a single observation: a value at a millisecond timestamp.
type Sample struct {
	T int64   // milliseconds since the Unix epoch
	V float64 // observed value
}

// MetricType classifies a metric family as advertised by the exposition format.
type MetricType uint8

const (
	Untyped MetricType = iota
	Counter
	Gauge
	Histogram
	Summary
)

// String returns the lowercase exposition-format name of the type.
func (t MetricType) String() string {
	switch t {
	case Counter:
		return "counter"
	case Gauge:
		return "gauge"
	case Histogram:
		return "histogram"
	case Summary:
		return "summary"
	default:
		return "untyped"
	}
}

// ParseMetricType maps an exposition "# TYPE" token to a MetricType. Unknown
// tokens map to Untyped rather than erroring, matching scraper leniency.
func ParseMetricType(s string) MetricType {
	switch s {
	case "counter":
		return Counter
	case "gauge":
		return Gauge
	case "histogram":
		return Histogram
	case "summary":
		return Summary
	default:
		return Untyped
	}
}
