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
