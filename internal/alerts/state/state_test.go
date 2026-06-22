package state_test

import (
	"testing"
	"time"

	"github.com/pod32g/omni-metrics/internal/alerts/models"
	"github.com/pod32g/omni-metrics/internal/alerts/state"
)

func TestNext(t *testing.T) {
	base := time.Unix(1000, 0)
	min := time.Minute
	cases := []struct {
		name     string
		cur      models.State
		condTrue bool
		activeAt time.Time
		now      time.Time
		forD     time.Duration
		want     models.State
		changed  bool
	}{
		{"ok stays ok when false", models.StateOK, false, base, base, min, models.StateOK, false},
		{"ok to firing immediately when for=0", models.StateOK, true, base, base, 0, models.StateFiring, true},
		{"ok to pending when for>0", models.StateOK, true, base, base, min, models.StatePending, true},
		{"pending stays pending before for elapses", models.StatePending, true, base, base.Add(30 * time.Second), min, models.StatePending, false},
		{"pending to firing once for elapses", models.StatePending, true, base, base.Add(min), min, models.StateFiring, true},
		{"pending to firing past for", models.StatePending, true, base, base.Add(2 * min), min, models.StateFiring, true},
		{"pending to resolved when false", models.StatePending, false, base, base.Add(10 * time.Second), min, models.StateResolved, true},
		{"firing stays firing when true", models.StateFiring, true, base, base.Add(5 * min), min, models.StateFiring, false},
		{"firing to resolved when false", models.StateFiring, false, base, base.Add(5 * min), min, models.StateResolved, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, changed := state.Next(c.cur, c.condTrue, c.activeAt, c.now, c.forD)
			if got != c.want || changed != c.changed {
				t.Errorf("Next() = (%v,%v), want (%v,%v)", got, changed, c.want, c.changed)
			}
		})
	}
}
