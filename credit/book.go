package credit

import (
	"slices"
	"time"
)

// Book is the working set of one account's lots for one operation. It is not
// safe for concurrent use; the host serializes operations per account (for
// example by locking the account row) and opens a Book per operation.
type Book struct {
	lots    map[string]*Lot
	changes []Change
	touched map[string]bool
	ended   map[string]bool
}

// Open checks the lots and starts a working set on copies of them.
func Open(lots []Lot) (*Book, error) {
	b := &Book{lots: make(map[string]*Lot, len(lots)), touched: map[string]bool{}, ended: map[string]bool{}}
	for _, l := range lots {
		if err := l.check(); err != nil {
			return nil, err
		}
		if _, dup := b.lots[l.ID]; dup || l.ID == "" {
			return nil, ErrInvalid
		}
		c := l
		b.lots[l.ID] = &c
	}
	return b, nil
}

// Changes are the movements made since Open, in order.
func (b *Book) Changes() []Change { return slices.Clone(b.changes) }

// Changed are the lots the movements touched, as they are now.
func (b *Book) Changed() []Lot {
	out := make([]Lot, 0, len(b.touched))
	for id := range b.touched {
		out = append(out, *b.lots[id])
	}
	slices.SortFunc(out, func(x, y Lot) int { return soonerFirst(x, y) })
	return out
}

// Lots are all lots as they are now, soonest-expiring first.
func (b *Book) Lots() []Lot {
	out := make([]Lot, 0, len(b.lots))
	for _, l := range b.lots {
		out = append(out, *l)
	}
	slices.SortFunc(out, func(x, y Lot) int { return soonerFirst(x, y) })
	return out
}

// Lot returns a lot as it is now.
func (b *Book) Lot(id string) (Lot, bool) {
	l, ok := b.lots[id]
	if !ok {
		return Lot{}, false
	}
	return *l, true
}

// Available is what a request in scope could spend at a time.
func (b *Book) Available(scope string, at time.Time) int64 {
	var sum int64
	for _, l := range b.lots {
		if l.spendableFor(scope, at) {
			sum += l.Available
		}
	}
	return sum
}

func (b *Book) move(l *Lot, c Change) {
	l.Available += c.Available
	l.Held += c.Held
	l.Consumed += c.Consumed
	l.Expired += c.Expired
	l.Revoked += c.Revoked
	c.LotID = l.ID
	b.changes = append(b.changes, c)
	b.touched[l.ID] = true
}

// spendable lists the lots a request in scope may use, soonest-expiring first.
func (b *Book) spendable(scope string, at time.Time) []*Lot {
	var out []*Lot
	for _, l := range b.lots {
		if l.spendableFor(scope, at) {
			out = append(out, l)
		}
	}
	slices.SortFunc(out, func(x, y *Lot) int { return soonerFirst(*x, *y) })
	return out
}

// Expire moves the available credit of lots past their expiry to expired;
// held credit lapses when its reservation ends.
func (b *Book) Expire(at time.Time) {
	ids := make([]string, 0, len(b.lots))
	for id, l := range b.lots {
		if l.expiredAt(at) && l.Available > 0 {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	for _, id := range ids {
		l := b.lots[id]
		b.move(l, Change{Kind: Lapse, Available: -l.Available, Expired: l.Available})
	}
}

// Revoke takes back what is left of a lot (a refund, a chargeback): its
// available credit at once, its held credit when the reservation ends.
// Consumed credit stays consumed.
func (b *Book) Revoke(id string, at time.Time) error {
	l, ok := b.lots[id]
	if !ok {
		return ErrUnknownLot
	}
	if !l.RevokedAt.IsZero() {
		return nil
	}
	l.RevokedAt = at
	b.touched[id] = true
	if l.Available > 0 {
		b.move(l, Change{Kind: Withdraw, Available: -l.Available, Revoked: l.Available})
	}
	return nil
}

// Withdraw takes back amount of a lot's credit (a refund of part or all of
// it) from what is available, and answers the part it could not take:
// credit already spent, or held by work still running. Taking back all
// that is left revokes the lot as Revoke does, so its held credit is
// revoked when its reservation ends.
func (b *Book) Withdraw(id string, amount int64, at time.Time) (short int64, err error) {
	l, ok := b.lots[id]
	switch {
	case !ok:
		return 0, ErrUnknownLot
	case amount <= 0:
		return 0, ErrInvalid
	case !l.RevokedAt.IsZero():
		return amount, nil
	}
	take := min(amount, l.Available)
	if amount >= l.Available+l.Held {
		return amount - take, b.Revoke(id, at)
	}
	if take > 0 {
		b.move(l, Change{Kind: Withdraw, Available: -take, Revoked: take})
	}
	return amount - take, nil
}

// Spend consumes up to amount from the spendable lots, soonest-expiring
// first, and returns what it could not cover; the host decides whether that
// is billed overage or a refusal (then it discards the Book).
func (b *Book) Spend(scope string, amount int64, at time.Time, reservation string) (uncovered int64, err error) {
	if amount < 0 {
		return 0, ErrInvalid
	}
	for _, l := range b.spendable(scope, at) {
		if amount == 0 {
			break
		}
		take := min(amount, l.Available)
		b.move(l, Change{Kind: Consume, Reservation: reservation, Available: -take, Consumed: take})
		amount -= take
	}
	return amount, nil
}
