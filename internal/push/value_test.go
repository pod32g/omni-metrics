package push

import (
	"encoding/json"
	"math"
	"testing"
)

func TestValueUnmarshal(t *testing.T) {
	cases := []struct {
		in   string
		want float64
		nan  bool
		err  bool
	}{
		{in: `42`, want: 42},
		{in: `-1.5`, want: -1.5},
		{in: `0`, want: 0},
		{in: `"NaN"`, nan: true},
		{in: `"+Inf"`, want: math.Inf(1)},
		{in: `"Inf"`, want: math.Inf(1)},
		{in: `"-Inf"`, want: math.Inf(-1)},
		{in: `"banana"`, err: true},
		{in: `true`, err: true},
		{in: `{}`, err: true},
	}
	for _, tc := range cases {
		var v Value
		err := json.Unmarshal([]byte(tc.in), &v)
		if tc.err {
			if err == nil {
				t.Errorf("%s: expected error, got %v", tc.in, float64(v))
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: unexpected error %v", tc.in, err)
			continue
		}
		if tc.nan {
			if !math.IsNaN(float64(v)) {
				t.Errorf("%s: want NaN, got %v", tc.in, float64(v))
			}
			continue
		}
		if float64(v) != tc.want {
			t.Errorf("%s: got %v, want %v", tc.in, float64(v), tc.want)
		}
	}
}
