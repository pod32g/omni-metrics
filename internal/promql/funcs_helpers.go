package promql

import (
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/pod32g/omni-metrics/internal/model"
)

// bucket is one cumulative histogram bucket (le upper bound + cumulative count).
type bucket struct {
	upperBound float64
	count      float64
}

func parseLe(s string) (float64, error) {
	switch s {
	case "+Inf", "Inf", "inf", "+inf":
		return math.Inf(1), nil
	case "-Inf", "-inf":
		return math.Inf(-1), nil
	}
	return strconv.ParseFloat(s, 64)
}

// bucketQuantile computes the q-quantile from cumulative buckets (Prometheus'
// algorithm: linear interpolation within the bucket that crosses the rank).
func bucketQuantile(q float64, buckets []bucket) float64 {
	if math.IsNaN(q) {
		return math.NaN()
	}
	if q < 0 {
		return math.Inf(-1)
	}
	if q > 1 {
		return math.Inf(1)
	}
	sort.Slice(buckets, func(i, j int) bool { return buckets[i].upperBound < buckets[j].upperBound })
	if len(buckets) < 2 || !math.IsInf(buckets[len(buckets)-1].upperBound, 1) {
		return math.NaN()
	}
	total := buckets[len(buckets)-1].count
	if total == 0 {
		return math.NaN()
	}
	rank := q * total
	b := sort.Search(len(buckets)-1, func(i int) bool { return buckets[i].count >= rank })
	if b == len(buckets)-1 {
		return buckets[len(buckets)-2].upperBound
	}
	if b == 0 && buckets[0].upperBound <= 0 {
		return buckets[0].upperBound
	}
	bucketStart, countStart := 0.0, 0.0
	bucketEnd, countEnd := buckets[b].upperBound, buckets[b].count
	if b > 0 {
		bucketStart = buckets[b-1].upperBound
		countStart = buckets[b-1].count
	}
	if countEnd == countStart {
		return bucketStart
	}
	return bucketStart + (bucketEnd-bucketStart)*(rank-countStart)/(countEnd-countStart)
}

// quantile computes the q-quantile of a sample slice (Prometheus interpolation).
func quantile(q float64, vals []float64) float64 {
	if len(vals) == 0 {
		return math.NaN()
	}
	if q < 0 {
		return math.Inf(-1)
	}
	if q > 1 {
		return math.Inf(1)
	}
	s := append([]float64(nil), vals...)
	sort.Float64s(s)
	n := float64(len(s))
	rank := q * (n - 1)
	lower := int(math.Floor(rank))
	upper := int(math.Ceil(rank))
	weight := rank - float64(lower)
	return s[lower]*(1-weight) + s[upper]*weight
}

// linearRegression fits points by least squares, with x measured in seconds
// relative to interceptTime (ms). Returns slope (per second) and intercept.
func linearRegression(pts []Point, interceptTime int64) (slope, intercept float64) {
	var n, sumX, sumY, sumXY, sumX2 float64
	for _, p := range pts {
		x := float64(p.T-interceptTime) / 1000
		n++
		sumX += x
		sumY += p.V
		sumXY += x * p.V
		sumX2 += x * x
	}
	if n == 0 {
		return 0, math.NaN()
	}
	cov := sumXY - sumX*sumY/n
	varX := sumX2 - sumX*sumX/n
	if varX == 0 {
		return 0, sumY / n
	}
	slope = cov / varX
	intercept = sumY/n - slope*sumX/n
	return slope, intercept
}

func joinStrings(parts []string, sep string) string { return strings.Join(parts, sep) }

// setOrDeleteLabel returns l with name set to val, or removed when val is empty.
func setOrDeleteLabel(l model.Labels, name, val string) model.Labels {
	m := l.Map()
	if val == "" {
		delete(m, name)
	} else {
		m[name] = val
	}
	return model.FromMap(m)
}

func setLabel(l model.Labels, name, val string) model.Labels {
	m := l.Map()
	m[name] = val
	return model.FromMap(m)
}

func dropLabels(l model.Labels, names ...string) model.Labels {
	drop := make(map[string]bool, len(names))
	for _, n := range names {
		drop[n] = true
	}
	out := make(model.Labels, 0, len(l))
	for _, lbl := range l {
		if !drop[lbl.Name] {
			out = append(out, lbl)
		}
	}
	return out
}

func projectKeep(l model.Labels, names []string) model.Labels {
	keep := make(map[string]bool, len(names))
	for _, n := range names {
		keep[n] = true
	}
	out := make(model.Labels, 0, len(names))
	for _, lbl := range l {
		if keep[lbl.Name] {
			out = append(out, lbl)
		}
	}
	return out
}

// matchSignature produces the key two vector elements must share to be matched.
// includeName controls whether __name__ participates in the default (no
// on/ignoring) case — set operators default to including it, arithmetic does not.
func matchSignature(l model.Labels, m *VectorMatching, includeName bool) string {
	if m == nil {
		if includeName {
			return l.String()
		}
		return dropMetricName(l).String()
	}
	if m.On {
		return projectKeep(l, m.MatchingLabels).String()
	}
	return dropLabels(l, append([]string{model.MetricName}, m.MatchingLabels...)...).String()
}

// resultMetricOneToOne is the label set a one-to-one binary result carries.
func resultMetricOneToOne(lhs model.Labels, m *VectorMatching) model.Labels {
	if m != nil && m.On {
		return projectKeep(lhs, m.MatchingLabels)
	}
	return dropMetricName(lhs)
}
