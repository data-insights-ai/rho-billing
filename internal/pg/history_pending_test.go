package pg

import (
	"slices"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/integration"
)

func TestBoundCreditHistoryPreservesPendingRevocation(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	if err := store.CreateAccount(ctx, "pending-history", "pending-history"); err != nil {
		t.Fatal(err)
	}
	now := testTime()
	engine := credit.New(store, func() time.Time { return now })
	if _, err := engine.Grant(ctx, credit.GrantInput{Account: "pending-history", Operation: "grant", LotID: "lot", Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 10, Source: "purchase", SourceRef: "purchase", ValidFrom: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Reserve(ctx, credit.ReserveInput{Account: "pending-history", Operation: "reserve", ReservationID: "hold", Actor: "actor", Unit: "credits", Amount: 8, Deadline: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.RevokeAmount(ctx, credit.RevokeAmountInput{Account: "pending-history", Operation: "refund", LotID: "lot", Reason: "refund", Amount: 5}); err != nil {
		t.Fatal(err)
	}
	root, err := store.History(ctx, "pending-history", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var pending int64
	for _, entry := range root {
		pending += entry.PendingRevocation
	}
	if pending != 3 {
		t.Fatalf("pending=%d want3", pending)
	}
	err = store.Atomic(ctx, "pending-history", func(session integration.Session) error {
		bound, err := session.Credits().History(ctx, "pending-history", 0, 100)
		if err != nil {
			return err
		}
		if !slices.Equal(root, bound) {
			t.Fatalf("bound history differs from root: root=%+v bound=%+v", root, bound)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
