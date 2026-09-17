// Package creditledger replays immutable credit quantity deltas.
package creditledger

import (
	"fmt"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/internal/checked"
)

// State is the conserved quantity state of one credit lot.
type State struct {
	Available, Held, Consumed, Expired, Revoked int64
	PendingRevocation                           int64
	Granted                                     int64
}

// Delta is one immutable journal quantity transition.
type Delta struct {
	Kind                                        string
	Available, Held, Consumed, Expired, Revoked int64
	PendingRevocation                           int64
}

// Apply applies one delta and enforces grant uniqueness, non-negative
// quantities, conservation, and pending-revocation bounds.
func Apply(state State, delta Delta) (State, error) {
	if state.Granted == 0 && delta.Kind != "grant" {
		return State{}, fmt.Errorf("%w: journal delta precedes grant", billing.ErrState)
	}
	if delta.Kind == "grant" {
		if state.Granted != 0 || delta.Available <= 0 {
			return State{}, fmt.Errorf("%w: invalid grant history", billing.ErrState)
		}
		state.Granted = delta.Available
	}
	fields := []*int64{&state.Available, &state.Held, &state.Consumed, &state.Expired, &state.Revoked, &state.PendingRevocation}
	deltas := []int64{delta.Available, delta.Held, delta.Consumed, delta.Expired, delta.Revoked, delta.PendingRevocation}
	for i, field := range fields {
		value, err := checked.Add(*field, deltas[i])
		if err != nil {
			return State{}, err
		}
		if value < 0 {
			return State{}, fmt.Errorf("%w: negative journal quantity", billing.ErrState)
		}
		*field = value
	}
	total := int64(0)
	for _, value := range []int64{state.Available, state.Held, state.Consumed, state.Expired, state.Revoked} {
		var err error
		total, err = checked.Add(total, value)
		if err != nil {
			return State{}, err
		}
	}
	if total != state.Granted || state.PendingRevocation > state.Held {
		return State{}, fmt.Errorf("%w: journal conservation failure", billing.ErrState)
	}
	return state, nil
}
