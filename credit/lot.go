// Package credit decides how prepaid credits are spent, without storing
// anything: the host loads an account's lots and reservations inside its own
// transaction, opens a Book on them, lets it decide, and stores the changed
// lots and the changes (its journal) in the same transaction.
//
// Amounts are integer subunits of one credit unit; the host fixes the unit.
// Every lot keeps Initial = Available + Held + Consumed + Expired + Revoked.
package credit

import (
	"errors"
	"time"
)

var (
	ErrInvalid      = errors.New("credit: invalid input")
	ErrInsufficient = errors.New("credit: not enough credit")
	ErrUnknownLot   = errors.New("credit: unknown lot")
	// ErrEnded: the reservation was settled or cancelled already.
	ErrEnded  = errors.New("credit: reservation already ended")
	ErrBroken = errors.New("credit: a lot's counters do not add up")
)

// Lot is one grant of credit: a purchase, a plan allowance, a promotion.
type Lot struct {
	ID string
	// Scope limits where the lot may be spent (for example one product); a
	// lot without scope may be spent anywhere.
	Scope     string
	GrantedAt time.Time
	ValidFrom time.Time
	// ExpiresAt is zero for credit that does not expire.
	ExpiresAt time.Time
	RevokedAt time.Time

	Initial, Available, Held, Consumed, Expired, Revoked int64
	// PendingRevoke is held credit a refund took back: it is revoked, not
	// made available, when its hold is released. At most Held.
	PendingRevoke int64
}

// NewLot is a lot with all of its amount available.
func NewLot(id, scope string, amount int64, grantedAt, validFrom, expiresAt time.Time) (Lot, error) {
	if id == "" || amount <= 0 || (!expiresAt.IsZero() && !expiresAt.After(validFrom)) {
		return Lot{}, ErrInvalid
	}
	return Lot{
		ID: id, Scope: scope, GrantedAt: grantedAt, ValidFrom: validFrom, ExpiresAt: expiresAt,
		Initial: amount, Available: amount,
	}, nil
}

func (l Lot) check() error {
	if l.Available < 0 || l.Held < 0 || l.Consumed < 0 || l.Expired < 0 || l.Revoked < 0 ||
		l.PendingRevoke < 0 || l.PendingRevoke > l.Held ||
		l.Available+l.Held+l.Consumed+l.Expired+l.Revoked != l.Initial {
		return ErrBroken
	}
	return nil
}

func (l Lot) expiredAt(at time.Time) bool { return !l.ExpiresAt.IsZero() && !at.Before(l.ExpiresAt) }

// spendableFor: a scoped request may use lots of its scope and unscoped
// lots; an unscoped request only unscoped lots.
func (l Lot) spendableFor(scope string, at time.Time) bool {
	return l.Available > 0 && !at.Before(l.ValidFrom) && !l.expiredAt(at) && l.RevokedAt.IsZero() &&
		(l.Scope == "" || l.Scope == scope)
}

// soonerFirst orders lots by expiry (non-expiring last), then grant time,
// then ID, so the credit that would be lost first is spent first.
func soonerFirst(a, b Lot) int {
	switch {
	case a.ExpiresAt.IsZero() != b.ExpiresAt.IsZero():
		if a.ExpiresAt.IsZero() {
			return 1
		}
		return -1
	case !a.ExpiresAt.Equal(b.ExpiresAt):
		return a.ExpiresAt.Compare(b.ExpiresAt)
	case !a.GrantedAt.Equal(b.GrantedAt):
		return a.GrantedAt.Compare(b.GrantedAt)
	}
	switch {
	case a.ID < b.ID:
		return -1
	case a.ID > b.ID:
		return 1
	}
	return 0
}
