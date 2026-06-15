package api

import (
	"fmt"
	"io"
	"sort"
	"sync"
	"time"
)

// SelfMetrics is the server's own instrumentation, exposed at /metrics in the
// Prometheus text format so omni-metrics can scrape itself.
type SelfMetrics struct {
	mu           sync.Mutex
	version      string
	headSeries   func() int
	start        time.Time
	httpReqs     map[string]int64
	queries      int64
	queryErrors  int64
	pushOK       int64
	pushErr      int64
	pushSamples  int64
	pushRejected int64
}

// NewSelfMetrics creates a collector. headSeries may be nil.
func NewSelfMetrics(version string, headSeries func() int) *SelfMetrics {
	return &SelfMetrics{
		version:    version,
		headSeries: headSeries,
		start:      time.Now(),
		httpReqs:   map[string]int64{},
	}
}

// IncHTTP counts a request to the named handler.
func (s *SelfMetrics) IncHTTP(handler string) {
	s.mu.Lock()
	s.httpReqs[handler]++
	s.mu.Unlock()
}

// IncQuery counts a PromQL query, flagging whether it errored.
func (s *SelfMetrics) IncQuery(isErr bool) {
	s.mu.Lock()
	s.queries++
	if isErr {
		s.queryErrors++
	}
	s.mu.Unlock()
}

// IncPushRequest counts a push request, flagging success or failure.
func (s *SelfMetrics) IncPushRequest(ok bool) {
	s.mu.Lock()
	if ok {
		s.pushOK++
	} else {
		s.pushErr++
	}
	s.mu.Unlock()
}

// AddPushSamples adds to the count of successfully appended pushed samples.
func (s *SelfMetrics) AddPushSamples(n int) {
	s.mu.Lock()
	s.pushSamples += int64(n)
	s.mu.Unlock()
}

// IncPushRejected counts a push rejected by the head cardinality cap.
func (s *SelfMetrics) IncPushRejected() {
	s.mu.Lock()
	s.pushRejected++
	s.mu.Unlock()
}

// WriteExposition writes the metrics in text exposition format.
func (s *SelfMetrics) WriteExposition(w io.Writer) {
	s.mu.Lock()
	defer s.mu.Unlock()

	fmt.Fprintf(w, "# HELP omni_build_info Build information.\n")
	fmt.Fprintf(w, "# TYPE omni_build_info gauge\n")
	fmt.Fprintf(w, "omni_build_info{version=%q} 1\n", s.version)

	fmt.Fprintf(w, "# HELP omni_http_requests_total Total HTTP API requests by handler.\n")
	fmt.Fprintf(w, "# TYPE omni_http_requests_total counter\n")
	handlers := make([]string, 0, len(s.httpReqs))
	for h := range s.httpReqs {
		handlers = append(handlers, h)
	}
	sort.Strings(handlers)
	for _, h := range handlers {
		fmt.Fprintf(w, "omni_http_requests_total{handler=%q} %d\n", h, s.httpReqs[h])
	}

	fmt.Fprintf(w, "# HELP omni_queries_total Total PromQL queries executed.\n")
	fmt.Fprintf(w, "# TYPE omni_queries_total counter\n")
	fmt.Fprintf(w, "omni_queries_total %d\n", s.queries)

	fmt.Fprintf(w, "# HELP omni_query_errors_total Total PromQL queries that failed.\n")
	fmt.Fprintf(w, "# TYPE omni_query_errors_total counter\n")
	fmt.Fprintf(w, "omni_query_errors_total %d\n", s.queryErrors)

	fmt.Fprintf(w, "# HELP omni_push_requests_total Total push requests by status.\n")
	fmt.Fprintf(w, "# TYPE omni_push_requests_total counter\n")
	fmt.Fprintf(w, "omni_push_requests_total{status=%q} %d\n", "success", s.pushOK)
	fmt.Fprintf(w, "omni_push_requests_total{status=%q} %d\n", "error", s.pushErr)

	fmt.Fprintf(w, "# HELP omni_push_samples_appended_total Total samples appended via push.\n")
	fmt.Fprintf(w, "# TYPE omni_push_samples_appended_total counter\n")
	fmt.Fprintf(w, "omni_push_samples_appended_total %d\n", s.pushSamples)

	fmt.Fprintf(w, "# HELP omni_push_series_rejected_total Push appends rejected by the head series cap.\n")
	fmt.Fprintf(w, "# TYPE omni_push_series_rejected_total counter\n")
	fmt.Fprintf(w, "omni_push_series_rejected_total %d\n", s.pushRejected)

	if s.headSeries != nil {
		fmt.Fprintf(w, "# HELP omni_head_series Number of series in the head block.\n")
		fmt.Fprintf(w, "# TYPE omni_head_series gauge\n")
		fmt.Fprintf(w, "omni_head_series %d\n", s.headSeries())
	}

	fmt.Fprintf(w, "# HELP omni_start_time_seconds Unix start time of the process.\n")
	fmt.Fprintf(w, "# TYPE omni_start_time_seconds gauge\n")
	fmt.Fprintf(w, "omni_start_time_seconds %d\n", s.start.Unix())
}
