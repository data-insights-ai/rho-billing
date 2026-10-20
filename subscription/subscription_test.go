package subscription

import (
	"errors"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func snap(status Status, at time.Time, event string) Subscription {
	return Subscription{ID: "sub_1", Customer: "ctm_1", Status: status, Items: []Item{{PriceID: "pri_1", Quantity: 10}},
		Revision: Revision{At: at, EventID: event}}
}

func TestTheNewestSnapshotWinsWhateverTheOrder(t *testing.T) {
	first := snap(Active, t0, "evt_1")
	first.Reference = "org-1"
	cur, changed, err := Apply(nil, first)
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	canceled := snap(Canceled, t0.Add(time.Hour), "evt_3")
	if cur, changed, _ = Apply(&cur, canceled); !changed || cur.Status != Canceled || cur.Reference != "org-1" {
		t.Errorf("newer snapshot: %+v %v", cur, changed)
	}
	late := snap(PastDue, t0.Add(30*time.Minute), "evt_2")
	if got, changed, _ := Apply(&cur, late); changed || got.Status != Canceled {
		t.Errorf("a late, older event changed the subscription: %+v", got)
	}
	if got, changed, _ := Apply(&cur, canceled); changed || got.Status != Canceled {
		t.Errorf("a replay changed it: %v", changed)
	}
	sameInstant := snap(Active, t0.Add(time.Hour), "evt_4")
	if got, changed, _ := Apply(&cur, sameInstant); !changed || got.Status != Active {
		t.Errorf("a later event at the same instant: %+v", got)
	}
}

func TestBrokenSnapshotsAreRefused(t *testing.T) {
	cases := map[string]Subscription{
		"no id":          {Status: Active, Revision: Revision{At: t0}},
		"no time":        {ID: "sub_1", Status: Active},
		"unknown status": {ID: "sub_1", Status: "deleted", Revision: Revision{At: t0}},
		"empty item":     {ID: "sub_1", Status: Active, Items: []Item{{PriceID: "pri_1"}}, Revision: Revision{At: t0}},
	}
	for name, s := range cases {
		if _, _, err := Apply(nil, s); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	cur := snap(Active, t0, "evt_1")
	other := snap(Active, t0.Add(time.Hour), "evt_2")
	other.ID = "sub_2"
	if _, _, err := Apply(&cur, other); !errors.Is(err, ErrInvalid) {
		t.Errorf("another subscription's snapshot: %v", err)
	}
}

func TestAccessFollowsStatus(t *testing.T) {
	for s, want := range map[Status]Access{Active: Full, Trialing: Full, PastDue: Grace, Paused: None, Canceled: None} {
		if got := AccessOf(s); got != want {
			t.Errorf("%s: %s, want %s", s, got, want)
		}
	}
}
