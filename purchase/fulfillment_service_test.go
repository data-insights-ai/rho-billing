package purchase

import (
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
)

func TestFulfillCreditEffectIsAtomicAndReplayable(t *testing.T) {
	now := billing.CanonicalTime(time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC))
	repo := NewMemoryRepository(ReferenceAccount{Account: "acct"})
	s := New(repo, func() time.Time { return now })
	offer, err := s.PublishOffer(t.Context(), Offer{Account: "acct", Revision: Revision{ID: "credit-offer", Version: 1}, Name: "Credit", Effects: []Effect{{Key: "grant", Credit: &CreditBenefit{Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 5}}}})
	if err != nil {
		t.Fatal(err)
	}
	price, err := s.PublishPrice(t.Context(), Price{Account: "acct", Revision: Revision{ID: "credit-price", Version: 1}, Offer: offer.Revision, Currency: "USD", UnitAmount: 100, TaxTreatment: TaxExclusive})
	if err != nil {
		t.Fatal(err)
	}
	quote, err := s.CreateQuote(t.Context(), QuoteInput{Account: "acct", ID: "credit-quote", ValidUntil: now.Add(time.Hour), Lines: []QuoteLineInput{{ID: "line", Price: price.Revision, Quantity: 2}}})
	if err != nil {
		t.Fatal(err)
	}
	scope := billing.Scope{Provider: "test", Merchant: "merchant", Environment: "sandbox"}
	intent, err := s.CreateIntent(t.Context(), IntentInput{Account: "acct", ID: "credit-intent", Operation: "credit-create", QuoteID: quote.ID, QuoteFingerprint: quote.Fingerprint(), Scope: scope, Actor: "actor", Reason: "purchase", ExpiresAt: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	fact := PaymentFact{Account: "acct", Scope: scope, EventID: "credit-paid", TransactionID: "credit-tx", IntentID: intent.ID, Status: FactPaid, Currency: "USD", Gross: 200, Lines: []PaidLine{{LineID: "line", Gross: 200}}, OccurredAt: now.Add(time.Minute), CollectedAt: now.Add(time.Minute)}
	if result, err := s.ApplyPayment(t.Context(), fact); err != nil || !result.Applied {
		t.Fatalf("payment=%+v err=%v", result, err)
	}
	fulfilled, err := s.Fulfill(t.Context(), "acct", intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fulfilled.Fulfillment != FulfillmentComplete {
		t.Fatalf("fulfilled intent=%+v", fulfilled)
	}
	if _, err := s.Fulfill(t.Context(), "acct", intent.ID); err != nil {
		t.Fatal(err)
	}
	if err := repo.WithinAccount(t.Context(), "acct", func(tx Tx) error {
		balance, err := credit.New(tx.Credits(), func() time.Time { return now.Add(2 * time.Minute) }).Balance(t.Context(), "acct", "credits", "")
		if err != nil {
			return err
		}
		if balance.Available != 10 {
			t.Fatalf("credit balance=%+v", balance)
		}
		rows, err := tx.Fulfillments(t.Context(), intent.ID)
		if err != nil {
			return err
		}
		if len(rows) != 1 || rows[0].State != FulfillmentComplete {
			t.Fatalf("fulfillments=%+v", rows)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
