package notify

import "strings"

// severityRank orders the omni-notify severity levels. Higher is more severe.
// Unknown severities rank 0, below every known level.
var severityRank = map[string]int{
	"critical": 5,
	"error":    4,
	"warning":  3,
	"info":     2,
	"debug":    1,
}

// MapSeverity normalizes a rule's free-form severity to one omni-notify accepts
// (critical|error|warning|info|debug). It lowercases the input and falls back to
// "warning" for anything empty or unrecognized, so every forwarded event carries
// a valid severity.
func MapSeverity(s string) string {
	low := strings.ToLower(s)
	if _, ok := severityRank[low]; ok {
		return low
	}
	return "warning"
}

// IsSeverity reports whether s is exactly one of the known severity levels. Used
// to validate a configured min_severity (which must be empty or a known level).
func IsSeverity(s string) bool {
	_, ok := severityRank[s]
	return ok
}

// meetsMin reports whether a (canonical) severity sev is at least as severe as
// min. An empty min disables filtering and admits everything.
func meetsMin(min, sev string) bool {
	if min == "" {
		return true
	}
	return severityRank[sev] >= severityRank[min]
}
