// Package state holds the pure alert state machine. It has no I/O and no
// timers: the caller supplies the current state, whether the condition is true
// this evaluation, the instant the condition first became true (activeAt), the
// evaluation time (now), and the rule's "for" duration. This makes every
// transition exhaustively testable in isolation.
package state

import (
	"time"

	"github.com/pod32g/omni-metrics/internal/alerts/models"
)

// Next computes the next state and whether it changed.
//
// Transitions:
//   - condition false: OK→OK (no-op), PENDING/FIRING→RESOLVED.
//   - condition true: OK→FIRING when for==0 else OK→PENDING; PENDING→FIRING once
//     now-activeAt >= for else PENDING; FIRING→FIRING.
//
// RESOLVED is not produced as a steady state — the caller drops the instance
// after recording the OK transition — so Next never receives StateResolved.
func Next(cur models.State, condTrue bool, activeAt, now time.Time, forD time.Duration) (models.State, bool) {
	if !condTrue {
		switch cur {
		case models.StatePending, models.StateFiring:
			return models.StateResolved, true
		default:
			return models.StateOK, false
		}
	}

	// Condition is true.
	switch cur {
	case models.StateFiring:
		return models.StateFiring, false
	case models.StatePending:
		if now.Sub(activeAt) >= forD {
			return models.StateFiring, true
		}
		return models.StatePending, false
	default: // StateOK (or resolved treated as fresh)
		if forD <= 0 {
			return models.StateFiring, true
		}
		return models.StatePending, true
	}
}
