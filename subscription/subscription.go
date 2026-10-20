// Package subscription is a provider's subscription in provider-neutral
// terms, without storing anything: the host keeps the latest Subscription,
// offers every incoming snapshot to Apply and stores what it returns.
// Providers deliver events late, twice and out of order; Apply keeps the
// newest by revision, so replays and stale events change nothing.
package subscription

import (
	"errors"
	"time"
)

var ErrInvalid = errors.New("subscription: invalid snapshot")

// Status is the provider's view of the subscription.
type Status string

const (
	Trialing Status = "trialing"
	Active   Status = "active"
	// PastDue: a renewal payment failed and the provider is retrying.
	PastDue  Status = "past_due"
	Paused   Status = "paused"
	Canceled Status = "canceled"
)

// Item is one price on the subscription.
type Item struct {
	PriceID  string
	Quantity int
}

// Revision orders snapshots: the time the provider says the change happened,
// then its event id for changes at the same instant.
type Revision struct {
	At      time.Time
	EventID string
}

func (r Revision) after(o Revision) bool {
	if !r.At.Equal(o.At) {
		return r.At.After(o.At)
	}
	return r.EventID > o.EventID
}

// Subscription is the latest known state of one provider subscription.
type Subscription struct {
	ID       string
	Customer string
	Status   Status
	Items    []Item
	// Period is the current billing period; zero while there is none.
	PeriodStart, PeriodEnd time.Time
	// CancelAt is set when a cancellation is scheduled for a later time.
	CancelAt time.Time
	// Reference is what the host attached at checkout to find its own
	// account (for example an organization id).
	Reference string
	Revision  Revision
}

func (s Subscription) validate() error {
	switch {
	case s.ID == "" || s.Revision.At.IsZero():
		return ErrInvalid
	case s.Status != Trialing && s.Status != Active && s.Status != PastDue && s.Status != Paused && s.Status != Canceled:
		return ErrInvalid
	}
	for _, it := range s.Items {
		if it.PriceID == "" || it.Quantity < 1 {
			return ErrInvalid
		}
	}
	return nil
}

// Apply returns the subscription to keep and whether the snapshot changed
// it. current is nil for a subscription the host has not seen. A snapshot
// for another subscription or older than current changes nothing. A missing
// reference is taken from current, since providers do not repeat it on
// every event.
func Apply(current *Subscription, snapshot Subscription) (Subscription, bool, error) {
	if err := snapshot.validate(); err != nil {
		return Subscription{}, false, err
	}
	if current == nil {
		return snapshot, true, nil
	}
	if current.ID != snapshot.ID {
		return Subscription{}, false, ErrInvalid
	}
	if !snapshot.Revision.after(current.Revision) {
		return *current, false, nil
	}
	if snapshot.Reference == "" {
		snapshot.Reference = current.Reference
	}
	return snapshot, true, nil
}
