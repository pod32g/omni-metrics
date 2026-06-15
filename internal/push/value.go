// Package push implements JSON push ingestion: a process with no HTTP server can
// POST samples that append into storage as time series (the inverse of scrape).
package push

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
)

// Value is a float64 that decodes from either a JSON number or one of the
// strings "NaN", "+Inf"/"Inf", or "-Inf" — JSON has no native non-finite floats.
type Value float64

func (v *Value) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return fmt.Errorf("value: %w", err)
		}
		switch s {
		case "NaN":
			*v = Value(math.NaN())
		case "+Inf", "Inf":
			*v = Value(math.Inf(1))
		case "-Inf":
			*v = Value(math.Inf(-1))
		default:
			return fmt.Errorf("value: invalid number string %q", s)
		}
		return nil
	}
	var f float64
	if err := json.Unmarshal(b, &f); err != nil {
		return fmt.Errorf("value: expected number or string, got %s", b)
	}
	*v = Value(f)
	return nil
}
