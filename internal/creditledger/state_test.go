package creditledger

import (
	"errors"
	"math"
	"testing"

	billing "github.com/data-insights-ai/rho-billing"
)

func TestApplyValidLifecycle(t *testing.T) {
	state, err := Apply(State{}, Delta{Kind: "grant", Available: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, delta := range []Delta{
		{Kind: "reserve", Available: -4, Held: 4},
		{Kind: "consume", Held: -2, Consumed: 2},
		{Kind: "release", Held: -2, Available: 2},
		{Kind: "expiry", Available: -6, Expired: 6},
	} {
		state, err = Apply(state, delta)
		if err != nil {
			t.Fatal(err)
		}
	}
	if state != (State{Available: 2, Consumed: 2, Expired: 6, Granted: 10}) {
		t.Fatalf("state=%+v", state)
	}
}

func TestApplyPendingRevocation(t *testing.T) {
	state, err := Apply(State{}, Delta{Kind: "grant", Available: 10})
	if err != nil {
		t.Fatal(err)
	}
	state, err = Apply(state, Delta{Kind: "reserve", Available: -10, Held: 10})
	if err != nil {
		t.Fatal(err)
	}
	state, err = Apply(state, Delta{Kind: "revoke", PendingRevocation: 3})
	if err != nil {
		t.Fatal(err)
	}
	if state.PendingRevocation != 3 || state.Held != 10 {
		t.Fatalf("state=%+v", state)
	}
}

func TestApplyRejectsInvalidHistoryAndOverflow(t *testing.T) {
	if _, err := Apply(State{}, Delta{Kind: "consume", Consumed: 1}); !errors.Is(err, billing.ErrState) {
		t.Fatalf("missing grant error=%v", err)
	}
	state, err := Apply(State{}, Delta{Kind: "grant", Available: math.MaxInt64})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(state, Delta{Kind: "broken", Available: 1}); !errors.Is(err, billing.ErrOverflow) {
		t.Fatalf("overflow error=%v", err)
	}
	if _, err := Apply(state, Delta{Kind: "broken", Available: -1, Held: 1, PendingRevocation: 2}); !errors.Is(err, billing.ErrState) {
		t.Fatalf("pending bound error=%v", err)
	}
}
