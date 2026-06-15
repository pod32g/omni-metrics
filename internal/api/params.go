package api

import (
	"errors"
	"fmt"
	"strconv"
)

var (
	errMissingTime = errors.New("missing required time parameter")
	errMissingStep = errors.New("missing required step parameter")
)

// parseDurationMillis parses a compound duration ("15s", "1m30s", "2h") into
// milliseconds, used for the query_range step parameter.
func parseDurationMillis(s string) (int64, error) {
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	var total int64
	i := 0
	for i < len(s) {
		start := i
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if start == i || i >= len(s) {
			return 0, fmt.Errorf("malformed duration %q", s)
		}
		num, _ := strconv.ParseInt(s[start:i], 10, 64)
		var unitMs int64
		switch s[i] {
		case 's':
			unitMs = 1000
		case 'm':
			unitMs = 60 * 1000
		case 'h':
			unitMs = 60 * 60 * 1000
		case 'd':
			unitMs = 24 * 60 * 60 * 1000
		case 'w':
			unitMs = 7 * 24 * 60 * 60 * 1000
		default:
			return 0, fmt.Errorf("unknown duration unit %q", string(s[i]))
		}
		total += num * unitMs
		i++
	}
	return total, nil
}
