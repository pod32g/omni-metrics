package model

import (
	"sort"
	"strings"
)

// Label is a single name/value pair.
type Label struct {
	Name  string
	Value string
}

// Labels is a set of labels kept sorted by Name with no duplicate names. The
// sorted invariant makes Hash, Equal, and String deterministic. Always build a
// Labels through FromStrings or FromMap (or sort a slice yourself) — the methods
// assume the invariant holds.
type Labels []Label

// FromStrings builds Labels from alternating name, value arguments. A trailing
// name without a value is dropped. The result is sorted and de-duplicated (last
// value wins for a repeated name).
func FromStrings(ss ...string) Labels {
	ls := make(Labels, 0, len(ss)/2)
	for i := 0; i+1 < len(ss); i += 2 {
		ls = append(ls, Label{Name: ss[i], Value: ss[i+1]})
	}
	return normalize(ls)
}

// FromMap builds sorted Labels from a map.
func FromMap(m map[string]string) Labels {
	ls := make(Labels, 0, len(m))
	for k, v := range m {
		ls = append(ls, Label{Name: k, Value: v})
	}
	return normalize(ls)
}

// normalize sorts by name and removes duplicate names, keeping the last value.
func normalize(ls Labels) Labels {
	sort.SliceStable(ls, func(i, j int) bool { return ls[i].Name < ls[j].Name })
	out := ls[:0]
	for i, l := range ls {
		if i > 0 && l.Name == out[len(out)-1].Name {
			out[len(out)-1].Value = l.Value
			continue
		}
		out = append(out, l)
	}
	return out
}

// Get returns the value for name, or "" if absent.
func (ls Labels) Get(name string) string {
	// Binary search is possible (sorted), but linear is fine for small sets and
	// avoids surprises if the invariant is ever violated.
	for _, l := range ls {
		if l.Name == name {
			return l.Value
		}
	}
	return ""
}

// Has reports whether name is present.
func (ls Labels) Has(name string) bool {
	for _, l := range ls {
		if l.Name == name {
			return true
		}
	}
	return false
}

// Map returns the labels as a plain map.
func (ls Labels) Map() map[string]string {
	m := make(map[string]string, len(ls))
	for _, l := range ls {
		m[l.Name] = l.Value
	}
	return m
}

// Equal reports whether two label sets are identical. Both must satisfy the
// sorted invariant.
func (ls Labels) Equal(o Labels) bool {
	if len(ls) != len(o) {
		return false
	}
	for i := range ls {
		if ls[i] != o[i] {
			return false
		}
	}
	return true
}

const sep = '\xff' // separator byte that cannot appear in valid UTF-8 names/values

// Hash returns a stable 64-bit identity for the label set using FNV-1a over the
// sorted name/value pairs. Equal label sets hash equally regardless of insertion
// order (the slice is always sorted).
func (ls Labels) Hash() uint64 {
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	h := uint64(offset64)
	writeByte := func(b byte) {
		h ^= uint64(b)
		h *= prime64
	}
	writeString := func(s string) {
		for i := 0; i < len(s); i++ {
			writeByte(s[i])
		}
	}
	for _, l := range ls {
		writeString(l.Name)
		writeByte(sep)
		writeString(l.Value)
		writeByte(sep)
	}
	return h
}

// String renders the label set in PromQL notation. If a __name__ label is
// present it is rendered as the metric name before the brace group and omitted
// from inside the braces: name{k="v",...}. Otherwise: {k="v",...}. Values are
// escaped for backslash, double quote, and newline.
func (ls Labels) String() string {
	var b strings.Builder
	metric := ls.Get(MetricName)
	b.WriteString(metric)
	b.WriteByte('{')
	first := true
	for _, l := range ls {
		if l.Name == MetricName {
			continue
		}
		if !first {
			b.WriteByte(',')
		}
		first = false
		b.WriteString(l.Name)
		b.WriteString(`="`)
		b.WriteString(escapeValue(l.Value))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

func escapeValue(v string) string {
	if !strings.ContainsAny(v, "\\\"\n") {
		return v
	}
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		switch v[i] {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteByte(v[i])
		}
	}
	return b.String()
}
