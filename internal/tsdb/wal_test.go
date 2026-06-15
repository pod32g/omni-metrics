package tsdb

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pod32g/omni-metrics/internal/model"
)

func TestWALRoundTrip(t *testing.T) {
	dir := t.TempDir()
	w, err := openWAL(dir)
	if err != nil {
		t.Fatal(err)
	}
	l := model.FromStrings(model.MetricName, "m", "a", "1")
	if err := w.logSeries(7, l); err != nil {
		t.Fatal(err)
	}
	if err := w.logSamples([]refSample{{ref: 7, t: 10, v: 1.5}, {ref: 7, t: 20, v: 2.5}}); err != nil {
		t.Fatal(err)
	}
	if err := w.sync(); err != nil {
		t.Fatal(err)
	}
	w.close()

	h := newHead(0, 0)
	if err := replayWAL(dir, h); err != nil {
		t.Fatal(err)
	}
	res := h.query(0, 100, matchers(t, model.MatchEqual, model.MetricName, "m"))
	if len(res) != 1 || len(res[0].samples) != 2 {
		t.Fatalf("replay = %v, want 1 series with 2 samples", res)
	}
	if !res[0].labels.Equal(l) {
		t.Errorf("labels = %v, want %v", res[0].labels, l)
	}
}

// A crash mid-write leaves a truncated trailing record. Replay must tolerate it,
// recovering all complete records and discarding the partial tail.
func TestWALTruncatedTailTolerated(t *testing.T) {
	dir := t.TempDir()
	w, _ := openWAL(dir)
	l := model.FromStrings(model.MetricName, "m")
	w.logSeries(1, l)
	w.logSamples([]refSample{{ref: 1, t: 1, v: 1}})
	w.sync()
	w.close()

	// Corrupt: append a few stray bytes that look like the start of a record but
	// are truncated (simulating a torn write).
	segs, _ := filepath.Glob(filepath.Join(dir, "*.wal"))
	f, _ := os.OpenFile(segs[len(segs)-1], os.O_APPEND|os.O_WRONLY, 0o644)
	f.Write([]byte{byte(recSamples), 0xff, 0xff, 0xff}) // type + partial length, no payload
	f.Close()

	h := newHead(0, 0)
	if err := replayWAL(dir, h); err != nil {
		t.Fatalf("replay should tolerate truncated tail, got %v", err)
	}
	res := h.query(0, 100, matchers(t, model.MatchEqual, model.MetricName, "m"))
	if len(res) != 1 || len(res[0].samples) != 1 {
		t.Fatalf("expected the one good sample to survive, got %v", res)
	}
}

// Corruption in a non-final segment must not leave later segments behind for a
// subsequent reopen to resurrect — recovery must be deterministic across reopens.
func TestWALCorruptionQuarantinesLaterSegments(t *testing.T) {
	dir := t.TempDir()
	w, _ := openWAL(dir)
	w.maxSegSize = 1 // force a roll on every record
	l := model.FromStrings(model.MetricName, "m")
	w.logSeries(1, l)
	w.logSamples([]refSample{{ref: 1, t: 10, v: 1}})
	w.logSamples([]refSample{{ref: 1, t: 20, v: 2}})
	w.sync()
	w.close()

	segs, _ := segmentFiles(dir)
	if len(segs) < 2 {
		t.Fatalf("expected multiple segments, got %d", len(segs))
	}
	// Corrupt the FIRST segment.
	data, _ := os.ReadFile(segs[0])
	data[len(data)-2] ^= 0xff
	os.WriteFile(segs[0], data, 0o644)

	h := newHead(0, 0)
	if err := replayWAL(dir, h); err == nil {
		t.Fatalf("expected a corruption error replaying segment 1")
	}
	after, _ := segmentFiles(dir)
	if len(after) != 1 {
		t.Errorf("later segments should be quarantined after corruption, got %d remaining", len(after))
	}
	// A second replay must yield the same (corruption-stopped) state, not resurrect data.
	h2 := newHead(0, 0)
	_ = replayWAL(dir, h2)
	if got := h2.query(0, 100, matchers(t, model.MatchEqual, model.MetricName, "m")); len(got) != 0 {
		t.Errorf("second replay resurrected post-corruption data: %v", got)
	}
}

func TestWALCorruptCRCStopsCleanly(t *testing.T) {
	dir := t.TempDir()
	w, _ := openWAL(dir)
	l := model.FromStrings(model.MetricName, "m")
	w.logSeries(1, l)
	w.logSamples([]refSample{{ref: 1, t: 1, v: 1}})
	w.sync()
	w.close()

	// Flip a byte in the payload region of the file to break a CRC.
	segs, _ := filepath.Glob(filepath.Join(dir, "*.wal"))
	data, _ := os.ReadFile(segs[0])
	if len(data) > 6 {
		data[len(data)-2] ^= 0xff // corrupt near the end (the samples record)
	}
	os.WriteFile(segs[0], data, 0o644)

	h := newHead(0, 0)
	// Replay returns an error describing corruption but must not panic; the series
	// record (intact) should still be recovered.
	_ = replayWAL(dir, h)
	if _, ok := h.hashes[l.Hash()]; !ok {
		t.Errorf("intact series record before the corruption should be recovered")
	}
}
