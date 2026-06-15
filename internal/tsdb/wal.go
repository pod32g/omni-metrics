package tsdb

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/pod32g/omni-metrics/internal/model"
)

// Record types in the WAL.
const (
	recSeries  byte = 1 // a new series: ref + labels
	recSamples byte = 2 // a batch of samples: ref + t + v, repeated
)

const defaultMaxSegSize = 64 << 20 // 64 MiB per segment before rolling

// refSample is a sample tagged with its series ref, as stored in the WAL and
// staged by an appender.
type refSample struct {
	ref uint64
	t   int64
	v   float64
}

// WAL is a segmented, append-only, CRC-checked write-ahead log. Records are
// length-prefixed; a torn trailing record from a crash is tolerated on replay.
type WAL struct {
	mu         sync.Mutex
	dir        string
	cur        *os.File
	curSize    int64
	seq        int
	maxSegSize int64
}

// openWAL opens (or creates) the log directory and the active segment for
// appending. It assumes any torn tail of the last segment has already been
// truncated by a preceding replayWAL.
func openWAL(dir string) (*WAL, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating wal dir: %w", err)
	}
	segs, err := segmentFiles(dir)
	if err != nil {
		return nil, err
	}
	w := &WAL{dir: dir, maxSegSize: defaultMaxSegSize}
	if len(segs) > 0 {
		w.seq = segSeq(segs[len(segs)-1])
	}
	if err := w.openSegment(); err != nil {
		return nil, err
	}
	return w, nil
}

// openSegment opens the current segment for appending, creating the first
// segment if none exist.
func (w *WAL) openSegment() error {
	if w.seq == 0 {
		w.seq = 1
	}
	path := filepath.Join(w.dir, fmt.Sprintf("%08d.wal", w.seq))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("opening segment: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	w.cur = f
	w.curSize = info.Size()
	// fsync the directory so the new segment's directory entry is durable — an
	// fsync of the file's contents alone does not guarantee the name is on disk.
	return syncDir(w.dir)
}

// syncDir fsyncs a directory so newly created entries within it survive a crash.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		// Some platforms/filesystems do not support directory fsync; ignore that
		// specific case rather than failing segment creation.
		return nil
	}
	return nil
}

func (w *WAL) logSeries(ref uint64, l model.Labels) error {
	return w.writeRecord(recSeries, encodeSeries(ref, l))
}

func (w *WAL) logSamples(samples []refSample) error {
	return w.writeRecord(recSamples, encodeSamples(samples))
}

func (w *WAL) writeRecord(typ byte, payload []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.curSize >= w.maxSegSize {
		if err := w.rollLocked(); err != nil {
			return err
		}
	}
	var hdr [5]byte
	hdr[0] = typ
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	crc := crc32.ChecksumIEEE(payload)
	var crcb [4]byte
	binary.BigEndian.PutUint32(crcb[:], crc)

	if _, err := w.cur.Write(hdr[:]); err != nil {
		return err
	}
	if _, err := w.cur.Write(payload); err != nil {
		return err
	}
	if _, err := w.cur.Write(crcb[:]); err != nil {
		return err
	}
	w.curSize += int64(len(hdr) + len(payload) + len(crcb))
	return nil
}

func (w *WAL) rollLocked() error {
	if err := w.cur.Sync(); err != nil {
		return err
	}
	if err := w.cur.Close(); err != nil {
		return err
	}
	w.seq++
	return w.openSegment()
}

// sync flushes the active segment to disk. Called by Appender.Commit so a
// committed batch survives a power loss.
func (w *WAL) sync() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.cur.Sync()
}

func (w *WAL) close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cur == nil {
		return nil
	}
	err := w.cur.Sync()
	if cerr := w.cur.Close(); err == nil {
		err = cerr
	}
	w.cur = nil
	return err
}

// replayWAL reads every segment in order and applies its records to h. It
// truncates any torn trailing record (clean crash) without error, and stops with
// an error on a CRC mismatch (real corruption) after recovering the records that
// preceded it.
func replayWAL(dir string, h *head) error {
	segs, err := segmentFiles(dir)
	if err != nil {
		return err
	}
	for i, seg := range segs {
		stopped, err := replaySegment(seg, h)
		if err != nil {
			// Corruption: quarantine every later segment so a subsequent reopen
			// cannot resurrect data past the point recovery stopped at, keeping
			// recovery deterministic across restarts.
			for _, later := range segs[i+1:] {
				_ = os.Remove(later)
			}
			return err
		}
		if stopped {
			break
		}
	}
	return nil
}

// replaySegment applies one segment's records. It returns stopped=true when it
// hit a torn tail (and truncated the file there); err!=nil signals corruption.
func replaySegment(path string, h *head) (stopped bool, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	off := 0
	valid := 0
	for {
		if off == len(data) {
			break // clean end of segment
		}
		if off+5 > len(data) {
			stopped = true // torn header
			break
		}
		typ := data[off]
		n := int(binary.BigEndian.Uint32(data[off+1 : off+5]))
		recEnd := off + 5 + n + 4
		if recEnd > len(data) {
			stopped = true // torn payload/crc
			break
		}
		payload := data[off+5 : off+5+n]
		gotCRC := binary.BigEndian.Uint32(data[off+5+n : recEnd])
		if crc32.ChecksumIEEE(payload) != gotCRC {
			// Real corruption: truncate here and report it.
			_ = os.Truncate(path, int64(valid))
			return true, fmt.Errorf("wal %s: crc mismatch at offset %d", filepath.Base(path), off)
		}
		if aerr := applyRecord(typ, payload, h); aerr != nil {
			_ = os.Truncate(path, int64(valid))
			return true, fmt.Errorf("wal %s: %w", filepath.Base(path), aerr)
		}
		off = recEnd
		valid = recEnd
	}
	if stopped && valid != len(data) {
		_ = os.Truncate(path, int64(valid))
	}
	return stopped, nil
}

func applyRecord(typ byte, payload []byte, h *head) error {
	switch typ {
	case recSeries:
		ref, l, err := decodeSeries(payload)
		if err != nil {
			return err
		}
		h.addSeriesWithRef(ref, l)
	case recSamples:
		samples, err := decodeSamples(payload)
		if err != nil {
			return err
		}
		for _, s := range samples {
			h.appendSample(s.ref, s.t, s.v)
		}
	default:
		return fmt.Errorf("unknown record type %d", typ)
	}
	return nil
}

// --- segment helpers ---

func segmentFiles(dir string) ([]string, error) {
	segs, err := filepath.Glob(filepath.Join(dir, "*.wal"))
	if err != nil {
		return nil, err
	}
	sort.Slice(segs, func(i, j int) bool { return segSeq(segs[i]) < segSeq(segs[j]) })
	return segs, nil
}

func segSeq(path string) int {
	var n int
	fmt.Sscanf(filepath.Base(path), "%08d.wal", &n)
	return n
}

// --- record encoding ---

func encodeSeries(ref uint64, l model.Labels) []byte {
	buf := make([]byte, 0, 16+len(l)*16)
	buf = appendUvarint(buf, ref)
	buf = appendUvarint(buf, uint64(len(l)))
	for _, lbl := range l {
		buf = appendString(buf, lbl.Name)
		buf = appendString(buf, lbl.Value)
	}
	return buf
}

func decodeSeries(b []byte) (uint64, model.Labels, error) {
	d := decoder{b: b}
	ref := d.uvarint()
	n := int(d.uvarint())
	if d.err != nil || n < 0 || n > len(b) {
		return 0, nil, fmt.Errorf("corrupt series record")
	}
	pairs := make([]string, 0, n*2)
	for i := 0; i < n; i++ {
		name := d.str()
		val := d.str()
		pairs = append(pairs, name, val)
	}
	if d.err != nil {
		return 0, nil, d.err
	}
	return ref, model.FromStrings(pairs...), nil
}

func encodeSamples(samples []refSample) []byte {
	buf := make([]byte, 0, 8+len(samples)*18)
	buf = appendUvarint(buf, uint64(len(samples)))
	for _, s := range samples {
		buf = appendUvarint(buf, s.ref)
		buf = appendVarint(buf, s.t)
		var vb [8]byte
		binary.LittleEndian.PutUint64(vb[:], math.Float64bits(s.v))
		buf = append(buf, vb[:]...)
	}
	return buf
}

func decodeSamples(b []byte) ([]refSample, error) {
	d := decoder{b: b}
	n := int(d.uvarint())
	if d.err != nil || n < 0 {
		return nil, fmt.Errorf("corrupt samples record")
	}
	out := make([]refSample, 0, n)
	for i := 0; i < n; i++ {
		ref := d.uvarint()
		t := d.varint()
		bits := d.fixed64()
		if d.err != nil {
			return nil, d.err
		}
		out = append(out, refSample{ref: ref, t: t, v: math.Float64frombits(bits)})
	}
	return out, nil
}

func appendUvarint(b []byte, v uint64) []byte {
	var tmp [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(tmp[:], v)
	return append(b, tmp[:n]...)
}

func appendVarint(b []byte, v int64) []byte {
	var tmp [binary.MaxVarintLen64]byte
	n := binary.PutVarint(tmp[:], v)
	return append(b, tmp[:n]...)
}

func appendString(b []byte, s string) []byte {
	b = appendUvarint(b, uint64(len(s)))
	return append(b, s...)
}

// decoder is a cursor over a record payload that records the first error.
type decoder struct {
	b   []byte
	i   int
	err error
}

func (d *decoder) uvarint() uint64 {
	if d.err != nil {
		return 0
	}
	v, n := binary.Uvarint(d.b[d.i:])
	if n <= 0 {
		d.err = fmt.Errorf("bad uvarint")
		return 0
	}
	d.i += n
	return v
}

func (d *decoder) varint() int64 {
	if d.err != nil {
		return 0
	}
	v, n := binary.Varint(d.b[d.i:])
	if n <= 0 {
		d.err = fmt.Errorf("bad varint")
		return 0
	}
	d.i += n
	return v
}

func (d *decoder) fixed64() uint64 {
	if d.err != nil {
		return 0
	}
	if d.i+8 > len(d.b) {
		d.err = fmt.Errorf("short fixed64")
		return 0
	}
	v := binary.LittleEndian.Uint64(d.b[d.i : d.i+8])
	d.i += 8
	return v
}

func (d *decoder) str() string {
	n := int(d.uvarint())
	if d.err != nil {
		return ""
	}
	if n < 0 || d.i+n > len(d.b) {
		d.err = fmt.Errorf("short string")
		return ""
	}
	s := string(d.b[d.i : d.i+n])
	d.i += n
	return s
}
