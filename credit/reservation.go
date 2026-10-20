package credit

import "time"

// Allocation is the part of a reservation held on one lot.
type Allocation struct {
	LotID  string
	Amount int64
}

// Reservation holds credit for work that must not run without it. The host
// stores it with its own state (held, settled, released) and deadline.
type Reservation struct {
	ID          string
	Scope       string
	Amount      int64
	Allocations []Allocation
}

// Settlement is how a reservation ended.
type Settlement struct {
	// Consumed is the credit spent, from the hold and, beyond it, from free
	// lots.
	Consumed int64
	// Uncovered is usage beyond all spendable credit: billed overage, or a
	// refusal, as the host decides.
	Uncovered int64
}

// Reserve holds amount on the spendable lots, soonest-expiring first, or
// holds nothing and answers ErrInsufficient.
func (b *Book) Reserve(id, scope string, amount int64, at time.Time) (Reservation, error) {
	if id == "" || amount <= 0 {
		return Reservation{}, ErrInvalid
	}
	if b.Available(scope, at) < amount {
		return Reservation{}, ErrInsufficient
	}
	r := Reservation{ID: id, Scope: scope}
	return r, b.hold(&r, amount, at)
}

// Extend holds more for a reservation, or nothing and answers
// ErrInsufficient.
func (b *Book) Extend(r Reservation, additional int64, at time.Time) (Reservation, error) {
	if additional <= 0 {
		return r, ErrInvalid
	}
	if b.Available(r.Scope, at) < additional {
		return r, ErrInsufficient
	}
	r.Allocations = append([]Allocation(nil), r.Allocations...)
	return r, b.hold(&r, additional, at)
}

func (b *Book) hold(r *Reservation, amount int64, at time.Time) error {
	for _, l := range b.spendable(r.Scope, at) {
		if amount == 0 {
			break
		}
		take := min(amount, l.Available)
		b.move(l, Change{Kind: Hold, Reservation: r.ID, Available: -take, Held: take})
		r.Allocations = append(r.Allocations, Allocation{LotID: l.ID, Amount: take})
		r.Amount += take
		amount -= take
	}
	return nil
}

// Settle spends actual for the reservation: from its hold in the order it
// was taken, the rest of the hold is released, and usage beyond the hold is
// spent from free lots; what nothing covers is returned as uncovered.
func (b *Book) Settle(r Reservation, actual int64, at time.Time) (Settlement, error) {
	if actual < 0 {
		return Settlement{}, ErrInvalid
	}
	remaining := actual
	for _, a := range r.Allocations {
		l, ok := b.lots[a.LotID]
		if !ok || l.Held < a.Amount {
			return Settlement{}, ErrUnknownLot
		}
		consume := min(remaining, a.Amount)
		remaining -= consume
		if consume > 0 {
			b.move(l, Change{Kind: Consume, Reservation: r.ID, Held: -consume, Consumed: consume})
		}
		b.free(l, a.Amount-consume, r.ID, at)
	}
	uncovered, err := b.Spend(r.Scope, remaining, at, r.ID)
	return Settlement{Consumed: actual - uncovered, Uncovered: uncovered}, err
}

// Cancel releases a whole reservation, for work that did not run or a
// reservation past its deadline.
func (b *Book) Cancel(r Reservation, at time.Time) error {
	for _, a := range r.Allocations {
		l, ok := b.lots[a.LotID]
		if !ok || l.Held < a.Amount {
			return ErrUnknownLot
		}
		b.free(l, a.Amount, r.ID, at)
	}
	return nil
}

// free ends a hold: back to available, or lapsed or withdrawn when the lot
// expired or was revoked while it was held.
func (b *Book) free(l *Lot, amount int64, reservation string, at time.Time) {
	if amount <= 0 {
		return
	}
	c := Change{Reservation: reservation, Held: -amount}
	switch {
	case !l.RevokedAt.IsZero():
		c.Kind, c.Revoked = Withdraw, amount
	case l.expiredAt(at):
		c.Kind, c.Expired = Lapse, amount
	default:
		c.Kind, c.Available = Release, amount
	}
	b.move(l, c)
}
