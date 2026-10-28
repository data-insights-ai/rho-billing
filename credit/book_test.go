package credit_test

import (
	"errors"
	"testing"
	"time"

	"github.com/data-insights-ai/rho-billing/credit"
)

var t0 = time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)

func lot(t *testing.T, id, scope string, amount int64, expiresInDays int) credit.Lot {
	t.Helper()
	var expires time.Time
	if expiresInDays > 0 {
		expires = t0.AddDate(0, 0, expiresInDays)
	}
	l, err := credit.NewLot(id, scope, amount, t0, t0, expires)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func open(t *testing.T, lots ...credit.Lot) *credit.Book {
	t.Helper()
	b, err := credit.Open(lots)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func get(t *testing.T, b *credit.Book, id string) credit.Lot {
	t.Helper()
	l, ok := b.Lot(id)
	if !ok {
		t.Fatalf("no lot %s", id)
	}
	return l
}

func TestCreditThatExpiresFirstIsSpentFirst(t *testing.T) {
	b := open(t, lot(t, "forever", "", 100, 0), lot(t, "month", "", 50, 30), lot(t, "week", "", 30, 7))
	if uncovered, _ := b.Spend("", 60, t0, ""); uncovered != 0 {
		t.Fatalf("uncovered %d", uncovered)
	}
	if get(t, b, "week").Consumed != 30 || get(t, b, "month").Consumed != 30 || get(t, b, "forever").Consumed != 0 {
		t.Errorf("order: week %+v month %+v forever %+v", get(t, b, "week"), get(t, b, "month"), get(t, b, "forever"))
	}
}

func TestScopedCreditStaysInItsScope(t *testing.T) {
	b := open(t, lot(t, "soc", "soc", 100, 0), lot(t, "any", "", 10, 0))
	if got := b.Available("", t0); got != 10 {
		t.Errorf("an unscoped request may spend only unscoped credit, got %d", got)
	}
	if got := b.Available("soc", t0); got != 110 {
		t.Errorf("a scoped request may spend its scope and unscoped credit, got %d", got)
	}
	if got := b.Available("crm", t0); got != 10 {
		t.Errorf("another scope: %d", got)
	}
	if lots := b.Lots(); len(lots) != 2 || lots[0].ID != "any" {
		t.Errorf("lots: %+v", lots)
	}
}

func TestAReservationHoldsAllOrNothing(t *testing.T) {
	b := open(t, lot(t, "a", "", 40, 0))
	if _, err := b.Reserve("r1", "", 50, t0); !errors.Is(err, credit.ErrInsufficient) {
		t.Fatalf("over-reserve: %v", err)
	}
	if len(b.Changes()) != 0 {
		t.Errorf("a refused reservation moved credit: %+v", b.Changes())
	}
	r, err := b.Reserve("r1", "", 30, t0)
	if err != nil || r.Amount != 30 || get(t, b, "a").Held != 30 || b.Available("", t0) != 10 {
		t.Fatalf("reserve: %+v %v", r, err)
	}
}

func TestSettlingSpendsTheHoldReleasesTheRestAndReportsOverage(t *testing.T) {
	b := open(t, lot(t, "a", "", 100, 0))
	r, _ := b.Reserve("r1", "", 40, t0)
	s, err := b.Settle(r, 25, t0)
	if err != nil || s.Consumed != 25 || s.Uncovered != 0 {
		t.Fatalf("settle under the hold: %+v %v", s, err)
	}
	if a := get(t, b, "a"); a.Consumed != 25 || a.Held != 0 || a.Available != 75 {
		t.Errorf("after settling: %+v", a)
	}

	b = open(t, lot(t, "a", "", 100, 0))
	r, _ = b.Reserve("r2", "", 40, t0)
	s, _ = b.Settle(r, 130, t0)
	if s.Consumed != 100 || s.Uncovered != 30 {
		t.Errorf("usage beyond the hold spends free credit and reports the rest: %+v", s)
	}
}

func TestEndingAHoldOnALotThatExpiredOrWasRevokedMeanwhile(t *testing.T) {
	b := open(t, lot(t, "week", "", 50, 7))
	r, _ := b.Reserve("r1", "", 50, t0)
	if err := b.Cancel(r, t0.AddDate(0, 0, 8)); err != nil {
		t.Fatal(err)
	}
	if w := get(t, b, "week"); w.Expired != 50 || w.Available != 0 {
		t.Errorf("held credit of an expired lot must lapse, not come back: %+v", w)
	}

	b = open(t, lot(t, "bought", "", 50, 0))
	r, _ = b.Reserve("r1", "", 50, t0)
	if err := b.Revoke("bought", t0); err != nil {
		t.Fatal(err)
	}
	s, _ := b.Settle(r, 20, t0)
	if bought := get(t, b, "bought"); s.Consumed != 20 || bought.Consumed != 20 || bought.Revoked != 30 || bought.Available != 0 {
		t.Errorf("work authorized before a refund is spent, the rest goes back to nobody: %+v", bought)
	}
}

func TestExpiryAndRevocation(t *testing.T) {
	b := open(t, lot(t, "week", "", 50, 7), lot(t, "bought", "", 20, 0))
	b.Expire(t0.AddDate(0, 0, 7))
	if w := get(t, b, "week"); w.Expired != 50 || w.Available != 0 {
		t.Errorf("expire: %+v", w)
	}
	if err := b.Revoke("bought", t0); err != nil || get(t, b, "bought").Revoked != 20 {
		t.Errorf("revoke: %+v %v", get(t, b, "bought"), err)
	}
	if err := b.Revoke("missing", t0); !errors.Is(err, credit.ErrUnknownLot) {
		t.Errorf("unknown lot: %v", err)
	}
	if b.Available("", t0) != 0 {
		t.Error("expired or revoked credit is still spendable")
	}
}

func TestExtendHoldsMore(t *testing.T) {
	b := open(t, lot(t, "a", "", 30, 0), lot(t, "b", "", 30, 10))
	r, _ := b.Reserve("r1", "", 20, t0)
	r, err := b.Extend(r, 25, t0)
	if err != nil || r.Amount != 45 || len(r.Allocations) != 3 {
		t.Fatalf("extend: %+v %v", r, err)
	}
	if _, err := b.Extend(r, 100, t0); !errors.Is(err, credit.ErrInsufficient) {
		t.Errorf("over-extend: %v", err)
	}
}

func TestOpenRefusesBrokenLots(t *testing.T) {
	broken := lot(t, "a", "", 10, 0)
	broken.Consumed = 5
	if _, err := credit.Open([]credit.Lot{broken}); !errors.Is(err, credit.ErrBroken) {
		t.Errorf("counters that do not add up: %v", err)
	}
	if _, err := credit.Open([]credit.Lot{lot(t, "a", "", 10, 0), lot(t, "a", "", 10, 0)}); !errors.Is(err, credit.ErrInvalid) {
		t.Errorf("duplicate ids: %v", err)
	}
	if _, err := credit.NewLot("x", "", 10, t0, t0, t0); !errors.Is(err, credit.ErrInvalid) {
		t.Errorf("a lot that expires when it starts: %v", err)
	}
}

func TestChangesReplayToTheChangedLots(t *testing.T) {
	start := []credit.Lot{lot(t, "a", "", 100, 5), lot(t, "b", "", 50, 0)}
	b := open(t, start...)
	r, _ := b.Reserve("r1", "", 120, t0)
	_, _ = b.Settle(r, 70, t0.AddDate(0, 0, 6))
	replayed := map[string]credit.Lot{"a": start[0], "b": start[1]}
	for _, c := range b.Changes() {
		l := replayed[c.LotID]
		l.Available += c.Available
		l.Held += c.Held
		l.Consumed += c.Consumed
		l.Expired += c.Expired
		l.Revoked += c.Revoked
		replayed[c.LotID] = l
	}
	for _, l := range b.Changed() {
		got := replayed[l.ID]
		if got.Available != l.Available || got.Held != l.Held || got.Consumed != l.Consumed || got.Expired != l.Expired {
			t.Errorf("journal and lot disagree for %s: %+v vs %+v", l.ID, got, l)
		}
	}
}

// A reservation settles only from its own hold: settling it twice does not
// spend another reservation's hold on the same lot.
func TestSettleTwiceDoesNotSpendAnotherReservationsHold(t *testing.T) {
	b := open(t, lot(t, "a", "", 10, 0))
	first, err := b.Reserve("r1", "", 5, t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Reserve("r2", "", 5, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Settle(first, 5, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Settle(first, 5, t0); !errors.Is(err, credit.ErrEnded) {
		t.Fatalf("a settled reservation settled again: %v", err)
	}
	if l := get(t, b, "a"); l.Held != 5 || l.Consumed != 5 {
		t.Fatalf("lot %+v; want r2's hold of 5 intact", l)
	}
}

// A reservation whose allocations the lots do not hold changes nothing.
func TestSettleOfAnUnheldReservationChangesNothing(t *testing.T) {
	b := open(t, lot(t, "a", "", 10, 0), lot(t, "b", "", 10, 0))
	r, err := b.Reserve("r1", "", 5, t0)
	if err != nil {
		t.Fatal(err)
	}
	r.Allocations = append(r.Allocations, credit.Allocation{LotID: "b", Amount: 5})
	before := len(b.Changes())
	if _, err := b.Settle(r, 10, t0); !errors.Is(err, credit.ErrUnknownLot) {
		t.Fatalf("settled a hold the lots do not have: %v", err)
	}
	if len(b.Changes()) != before {
		t.Fatalf("a refused settle moved credit: %+v", b.Changes()[before:])
	}
}

// A refund takes back what is left of the refunded credit and answers the
// part already spent or held.
func TestWithdrawTakesBackWhatIsLeft(t *testing.T) {
	b := open(t, lot(t, "a", "", 10, 0), lot(t, "b", "", 10, 0))
	if _, err := b.Spend("", 6, t0, ""); err != nil {
		t.Fatal(err)
	}
	spent := "a"
	if get(t, b, "a").Consumed == 0 {
		spent = "b"
	}
	short, err := b.Withdraw(spent, 10, t0)
	if err != nil || short != 6 {
		t.Fatalf("short %d, %v; want the 6 spent", short, err)
	}
	if l := get(t, b, spent); l.Available != 0 || l.Revoked != 4 || l.RevokedAt.IsZero() {
		t.Fatalf("lot %+v; want the 4 left revoked", l)
	}
	other := map[string]string{"a": "b", "b": "a"}[spent]
	if short, err := b.Withdraw(other, 3, t0); err != nil || short != 0 {
		t.Fatalf("partial refund short %d, %v", short, err)
	}
	if l := get(t, b, other); l.Available != 7 || l.Revoked != 3 || !l.RevokedAt.IsZero() {
		t.Fatalf("lot %+v; a partial refund takes 3 and leaves the rest usable", l)
	}
	if _, err := b.Withdraw(other, 0, t0); !errors.Is(err, credit.ErrInvalid) {
		t.Fatalf("withdrew nothing: %v", err)
	}
}

// Credit refunded while a reservation held it is revoked when the hold is
// released, not returned to the customer.
func TestARefundOfHeldCreditIsRevokedOnRelease(t *testing.T) {
	b := open(t, lot(t, "a", "", 10, 0))
	r, err := b.Reserve("r1", "", 6, t0)
	if err != nil {
		t.Fatal(err)
	}
	short, err := b.Withdraw("a", 8, t0)
	if err != nil || short != 0 {
		t.Fatalf("short %d, %v; the held credit is still there to take back", short, err)
	}
	if err := b.Cancel(r, t0); err != nil {
		t.Fatal(err)
	}
	if got := b.Available("", t0); got != 2 {
		t.Fatalf("%d spendable after the release; want the 2 not refunded", got)
	}
	if l := get(t, b, "a"); l.Revoked != 8 {
		t.Fatalf("lot %+v; want the 8 refunded revoked", l)
	}
}

// A hold that is settled instead consumes the credit; it is not revoked
// twice and the lot stays consistent.
func TestARefundOfHeldCreditThatIsSettledStaysConsumed(t *testing.T) {
	b := open(t, lot(t, "a", "", 10, 0))
	r, err := b.Reserve("r1", "", 6, t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Withdraw("a", 8, t0); err != nil {
		t.Fatal(err)
	}
	st, err := b.Settle(r, 6, t0)
	if err != nil || st.Refunded != 4 {
		t.Fatalf("settlement %+v, %v; want the 4 refunded credits spent reported", st, err)
	}
	l := get(t, b, "a")
	if l.Consumed != 6 || l.Revoked != 4 || l.Held != 0 || l.PendingRevoke != 0 {
		t.Fatalf("lot %+v", l)
	}
}

// A second refund while the first waits on a hold reports its shortfall
// against what is left after the first.
func TestASecondRefundCountsWhatTheFirstTook(t *testing.T) {
	b := open(t, lot(t, "a", "", 10, 0))
	if _, err := b.Reserve("r1", "", 6, t0); err != nil {
		t.Fatal(err)
	}
	if short, err := b.Withdraw("a", 8, t0); err != nil || short != 0 {
		t.Fatalf("first refund short %d, %v", short, err)
	}
	if short, err := b.Withdraw("a", 6, t0); err != nil || short != 4 {
		t.Fatalf("second refund short %d, %v; only 2 were left", short, err)
	}
}

// Credit of a revoked lot that a running reservation then spends is
// reported as refunded credit spent.
func TestSpendingHeldCreditOfARevokedLotIsReported(t *testing.T) {
	b := open(t, lot(t, "a", "", 10, 0))
	r, err := b.Reserve("r1", "", 6, t0)
	if err != nil {
		t.Fatal(err)
	}
	if short, err := b.Withdraw("a", 10, t0); err != nil || short != 0 {
		t.Fatalf("short %d, %v", short, err)
	}
	st, err := b.Settle(r, 5, t0)
	if err != nil || st.Refunded != 5 {
		t.Fatalf("settlement %+v, %v; want 5 refunded credits spent", st, err)
	}
	if l := get(t, b, "a"); l.Revoked != 5 || l.Consumed != 5 || l.PendingRevoke != 0 {
		t.Fatalf("lot %+v", l)
	}
}
