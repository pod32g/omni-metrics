package notify

import "testing"

func TestMapSeverity(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"critical", "critical"},
		{"error", "error"},
		{"warning", "warning"},
		{"info", "info"},
		{"debug", "debug"},
		{"CRITICAL", "critical"}, // case-insensitive
		{"Warning", "warning"},
		{"", "warning"},           // empty defaults to warning
		{"page", "warning"},       // unknown defaults to warning
		{"notice", "warning"},     // unknown defaults to warning
		{" critical ", "warning"}, // not trimmed -> unknown -> default
	}
	for _, tt := range tests {
		if got := MapSeverity(tt.in); got != tt.want {
			t.Errorf("MapSeverity(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestMeetsMin(t *testing.T) {
	tests := []struct {
		min  string
		sev  string
		want bool
	}{
		{"", "debug", true},    // no filter admits everything
		{"", "critical", true}, // no filter admits everything
		{"warning", "critical", true},
		{"warning", "error", true},
		{"warning", "warning", true},
		{"warning", "info", false},
		{"warning", "debug", false},
		{"critical", "critical", true},
		{"critical", "error", false},
		{"debug", "debug", true},
		{"info", "warning", true},
	}
	for _, tt := range tests {
		if got := meetsMin(tt.min, tt.sev); got != tt.want {
			t.Errorf("meetsMin(min=%q, sev=%q) = %v, want %v", tt.min, tt.sev, got, tt.want)
		}
	}
}

func TestIsSeverity(t *testing.T) {
	for _, s := range []string{"critical", "error", "warning", "info", "debug"} {
		if !IsSeverity(s) {
			t.Errorf("IsSeverity(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "page", "notice", "CRITICAL"} {
		if IsSeverity(s) {
			t.Errorf("IsSeverity(%q) = true, want false", s)
		}
	}
}
