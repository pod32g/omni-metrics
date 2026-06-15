package tsdb

import (
	"sort"
	"sync"

	"github.com/pod32g/omni-metrics/internal/model"
)

// memSeries is one series held in the head block: its identity plus its samples
// in ascending timestamp order.
type memSeries struct {
	ref     uint64
	labels  model.Labels
	samples []model.Sample
}

// seriesResult is a snapshot of a series produced by a query: labels plus a copy
// of the samples within the queried range (copied so concurrent appends to the
// live series cannot race with a reader iterating the result).
type seriesResult struct {
	labels  model.Labels
	samples []model.Sample
}

// head is the in-memory store of recent samples with an inverted index for
// matcher resolution. All access is guarded by mu.
type head struct {
	mu        sync.RWMutex
	series    map[uint64]*memSeries
	hashes    map[uint64][]*memSeries                   // label hash -> series (collision chains)
	postings  map[string]map[string]map[uint64]struct{} // name -> value -> set of refs
	lastRef   uint64
	maxSeries int   // 0 = unlimited
	retention int64 // ms; informational — truncation is driven explicitly via truncate
}

func newHead(retention int64, maxSeries int) *head {
	return &head{
		series:    map[uint64]*memSeries{},
		hashes:    map[uint64][]*memSeries{},
		postings:  map[string]map[string]map[uint64]struct{}{},
		maxSeries: maxSeries,
		retention: retention,
	}
}

// getOrCreate returns the ref for l, creating the series if it does not exist.
// Uncapped — used by replay and tests.
func (h *head) getOrCreate(l model.Labels) (uint64, bool) {
	ref, created, _ := h.getOrCreateLimited(l, 0)
	return ref, created
}

// acquire is the capped creation path used by live appends. It returns
// ErrTooManySeries when creating a new series would exceed the configured cap.
func (h *head) acquire(l model.Labels) (uint64, bool, error) {
	ref, created, ok := h.getOrCreateLimited(l, h.maxSeries)
	if !ok {
		return 0, false, ErrTooManySeries
	}
	return ref, created, nil
}

// getOrCreateLimited finds or creates the series for l. limit==0 means unlimited;
// otherwise creation fails (ok=false) once the head already holds limit series.
func (h *head) getOrCreateLimited(l model.Labels, limit int) (ref uint64, created bool, ok bool) {
	hash := l.Hash()
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, ms := range h.hashes[hash] {
		if ms.labels.Equal(l) {
			return ms.ref, false, true
		}
	}
	if limit > 0 && len(h.series) >= limit {
		return 0, false, false
	}
	h.lastRef++
	ref = h.lastRef
	h.insertLocked(ref, l, hash)
	return ref, true, true
}

// addSeriesWithRef inserts a series with a specific ref (used by WAL replay). It
// is idempotent: replaying the same record twice does not duplicate the series.
func (h *head) addSeriesWithRef(ref uint64, l model.Labels) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, exists := h.series[ref]; exists {
		return
	}
	h.insertLocked(ref, l, l.Hash())
	if ref > h.lastRef {
		h.lastRef = ref
	}
}

// insertLocked adds a new series to the maps and inverted index. Caller holds mu.
func (h *head) insertLocked(ref uint64, l model.Labels, hash uint64) {
	ms := &memSeries{ref: ref, labels: l}
	h.series[ref] = ms
	h.hashes[hash] = append(h.hashes[hash], ms)
	for _, lbl := range l {
		vals := h.postings[lbl.Name]
		if vals == nil {
			vals = map[string]map[uint64]struct{}{}
			h.postings[lbl.Name] = vals
		}
		set := vals[lbl.Value]
		if set == nil {
			set = map[uint64]struct{}{}
			vals[lbl.Value] = set
		}
		set[ref] = struct{}{}
	}
}

// appendSample adds a sample to a series, preserving ascending-timestamp order.
// A missing ref (should not happen) is ignored.
func (h *head) appendSample(ref uint64, t int64, v float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ms := h.series[ref]
	if ms == nil {
		return
	}
	n := len(ms.samples)
	if n == 0 || t > ms.samples[n-1].T {
		ms.samples = append(ms.samples, model.Sample{T: t, V: v})
		return
	}
	if t == ms.samples[n-1].T {
		// Duplicate timestamp: keep the first value. This rejects the
		// one-value-per-timestamp violation and makes WAL sample replay
		// idempotent (replaying the same record twice is a no-op).
		return
	}
	// Out-of-order: insert at the sorted position (rare path).
	i := sort.Search(n, func(i int) bool { return ms.samples[i].T >= t })
	if i < n && ms.samples[i].T == t {
		return // duplicate timestamp in the out-of-order path
	}
	ms.samples = append(ms.samples, model.Sample{})
	copy(ms.samples[i+1:], ms.samples[i:])
	ms.samples[i] = model.Sample{T: t, V: v}
}

// query returns all series matching matchers that have at least one sample in
// [mint, maxt], sorted by their label string for determinism.
func (h *head) query(mint, maxt int64, matchers []model.Matcher) []seriesResult {
	h.mu.RLock()
	defer h.mu.RUnlock()
	var out []seriesResult
	for _, ms := range h.candidatesLocked(matchers) {
		if !matchesAll(ms.labels, matchers) {
			continue
		}
		s := samplesInRange(ms.samples, mint, maxt)
		if len(s) == 0 {
			continue
		}
		cp := make([]model.Sample, len(s))
		copy(cp, s)
		out = append(out, seriesResult{labels: ms.labels, samples: cp})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].labels.String() < out[j].labels.String() })
	return out
}

// candidatesLocked narrows the series to scan using the most selective equality
// matcher's postings list, falling back to all series when no equality matcher
// (with a non-empty value) is usable. Caller holds at least RLock.
func (h *head) candidatesLocked(matchers []model.Matcher) []*memSeries {
	var best map[uint64]struct{}
	haveBest := false
	for _, m := range matchers {
		if m.Type == model.MatchEqual && m.Value != "" {
			refs := h.postings[m.Name][m.Value]
			if !haveBest || len(refs) < len(best) {
				best = refs
				haveBest = true
			}
		}
	}
	if haveBest {
		out := make([]*memSeries, 0, len(best))
		for ref := range best {
			if ms := h.series[ref]; ms != nil {
				out = append(out, ms)
			}
		}
		return out
	}
	out := make([]*memSeries, 0, len(h.series))
	for _, ms := range h.series {
		out = append(out, ms)
	}
	return out
}

// truncate drops all samples with timestamp < minT from every series and
// reclaims any series left with no samples — removing it from the series map,
// inverted index, and hash chain so it stops consuming the cardinality budget.
// Without this reclamation, label churn (e.g. rotating pod names) would fill the
// head with empty series and eventually reject all new series.
func (h *head) truncate(minT int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ref, ms := range h.series {
		i := sort.Search(len(ms.samples), func(i int) bool { return ms.samples[i].T >= minT })
		if i > 0 {
			kept := make([]model.Sample, len(ms.samples)-i)
			copy(kept, ms.samples[i:])
			ms.samples = kept
		}
		if len(ms.samples) == 0 {
			h.removeSeriesLocked(ref, ms)
		}
	}
}

// reclaimIfEmpty removes the series at ref iff it currently holds no samples —
// used by appender.Rollback to release cap slots taken by series the batch
// created but never committed. A series that has since gained samples (e.g. a
// concurrent committed batch) is left intact.
func (h *head) reclaimIfEmpty(ref uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if ms := h.series[ref]; ms != nil && len(ms.samples) == 0 {
		h.removeSeriesLocked(ref, ms)
	}
}

// removeSeriesLocked deletes a series from all of the head's structures. Caller
// holds mu.
func (h *head) removeSeriesLocked(ref uint64, ms *memSeries) {
	delete(h.series, ref)

	hash := ms.labels.Hash()
	chain := h.hashes[hash]
	for i, s := range chain {
		if s == ms {
			h.hashes[hash] = append(chain[:i], chain[i+1:]...)
			break
		}
	}
	if len(h.hashes[hash]) == 0 {
		delete(h.hashes, hash)
	}

	for _, lbl := range ms.labels {
		vals := h.postings[lbl.Name]
		if vals == nil {
			continue
		}
		if set := vals[lbl.Value]; set != nil {
			delete(set, ref)
			if len(set) == 0 {
				delete(vals, lbl.Value)
			}
		}
		if len(vals) == 0 {
			delete(h.postings, lbl.Name)
		}
	}
}

// labelNames returns the sorted distinct label names across series matching
// matchers (or all series when matchers is empty).
func (h *head) labelNames(matchers []model.Matcher) []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	set := map[string]struct{}{}
	for _, ms := range h.candidatesLocked(matchers) {
		if !matchesAll(ms.labels, matchers) {
			continue
		}
		for _, l := range ms.labels {
			set[l.Name] = struct{}{}
		}
	}
	return sortedKeys(set)
}

// labelValues returns the sorted distinct values of name across series matching
// matchers.
func (h *head) labelValues(name string, matchers []model.Matcher) []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	set := map[string]struct{}{}
	for _, ms := range h.candidatesLocked(matchers) {
		if !matchesAll(ms.labels, matchers) {
			continue
		}
		if v := ms.labels.Get(name); v != "" {
			set[v] = struct{}{}
		}
	}
	return sortedKeys(set)
}

func matchesAll(l model.Labels, matchers []model.Matcher) bool {
	for i := range matchers {
		if !matchers[i].Matches(l.Get(matchers[i].Name)) {
			return false
		}
	}
	return true
}

// samplesInRange returns the sub-slice of samples (ascending) with mint<=T<=maxt.
func samplesInRange(samples []model.Sample, mint, maxt int64) []model.Sample {
	lo := sort.Search(len(samples), func(i int) bool { return samples[i].T >= mint })
	hi := sort.Search(len(samples), func(i int) bool { return samples[i].T > maxt })
	return samples[lo:hi]
}

func sortedKeys(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
