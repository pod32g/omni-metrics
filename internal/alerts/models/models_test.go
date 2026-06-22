package models_test

import (
	"testing"

	"github.com/pod32g/omni-metrics/internal/alerts/models"
)

func TestStateString(t *testing.T) {
	cases := map[models.State]string{
		models.StateOK:       "ok",
		models.StatePending:  "pending",
		models.StateFiring:   "firing",
		models.StateResolved: "resolved",
	}
	for s, want := range cases {
		if got := s.String(); got != want {
			t.Errorf("State(%d).String() = %q, want %q", s, got, want)
		}
	}
}

func TestParseState(t *testing.T) {
	for _, name := range []string{"ok", "pending", "firing", "resolved"} {
		s, err := models.ParseState(name)
		if err != nil {
			t.Fatalf("ParseState(%q): %v", name, err)
		}
		if s.String() != name {
			t.Errorf("ParseState(%q) round-trip = %q", name, s.String())
		}
	}
	if _, err := models.ParseState("bogus"); err == nil {
		t.Error("ParseState(\"bogus\") = nil error, want error")
	}
}

func TestFingerprintOrderIndependent(t *testing.T) {
	a := models.Fingerprint(map[string]string{"a": "1", "b": "2"})
	b := models.Fingerprint(map[string]string{"b": "2", "a": "1"})
	if a != b {
		t.Fatalf("fingerprint not order-independent: %s vs %s", a, b)
	}
	if a == "" {
		t.Fatal("fingerprint empty")
	}
	if a == models.Fingerprint(map[string]string{"a": "1"}) {
		t.Error("distinct label sets collided")
	}
	// Empty label set is stable and non-panicking.
	if models.Fingerprint(nil) != models.Fingerprint(map[string]string{}) {
		t.Error("nil and empty map fingerprints differ")
	}
}

func TestResultKind(t *testing.T) {
	if (models.Result{Kind: models.KindEmpty}).Kind != models.KindEmpty {
		t.Error("empty kind")
	}
	r := models.Result{Kind: models.KindVector, Samples: []models.Sample{{Labels: map[string]string{"x": "y"}, Value: 1}}}
	if len(r.Samples) != 1 || r.Samples[0].Value != 1 {
		t.Error("vector samples not carried")
	}
}
