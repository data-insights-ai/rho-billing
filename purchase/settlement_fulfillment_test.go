package purchase

import (
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/usage"
)

func TestReferenceSettlementFulfillmentLinksBatchBeforeEffectReceipt(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	ctx := t.Context()
	batch := usage.BatchSummary{Account: "account", ID: "batch", Currency: "USD", Total: 100, State: usage.BatchReady, Revision: 1, LineCount: 1, CreatedAt: now, UpdatedAt: now}
	repo := NewMemoryRepository(ReferenceAccount{Account: "account", Settlements: []usage.BatchSummary{batch}})
	service := New(repo, func() time.Time { return now })
	offer, err := service.PublishOffer(ctx, Offer{Account: "account", Revision: Revision{ID: "offer", Version: 1}, Name: "Usage", Effects: []Effect{{Key: "settlement", Settlement: &SettlementBenefit{}}}})
	if err != nil {
		t.Fatal(err)
	}
	price, err := service.PublishPrice(ctx, Price{Account: "account", Revision: Revision{ID: "price", Version: 1}, Offer: offer.Revision, Currency: "USD", UnitAmount: 100, TaxTreatment: TaxExclusive})
	if err != nil {
		t.Fatal(err)
	}
	quote, err := service.CreateQuote(ctx, QuoteInput{Account: "account", ID: "quote", ValidUntil: now.Add(time.Hour), Lines: []QuoteLineInput{{ID: "line", Price: price.Revision, Quantity: 1, SettlementBatchID: batch.ID}}})
	if err != nil {
		t.Fatal(err)
	}
	scope := billing.Scope{Provider: "example", Merchant: "merchant", Environment: "sandbox"}
	intent, err := service.CreateIntent(ctx, IntentInput{Account: "account", ID: "intent", Operation: "create", QuoteID: quote.ID, QuoteFingerprint: quote.Fingerprint(), Scope: scope, Actor: "owner", Reason: "settle usage", ExpiresAt: quote.ValidUntil})
	if err != nil {
		t.Fatal(err)
	}
	fact := PaymentFact{Account: intent.Account, Scope: scope, IntentID: intent.ID, EventID: "paid", TransactionID: "tx", Status: FactPaid, Currency: "USD", Gross: 100, Lines: []PaidLine{{LineID: "line", Gross: 100}}, OccurredAt: now, CollectedAt: now}
	if out, err := service.ApplyPayment(ctx, fact); err != nil || !out.Applied {
		t.Fatalf("payment=%+v %v", out, err)
	}
	current, err := service.Intent(ctx, intent.Account, intent.ID)
	if err != nil || current.Fulfillment != FulfillmentComplete {
		t.Fatalf("completion=%+v %v", current, err)
	}
	if err := repo.WithinAccount(ctx, intent.Account, func(tx Tx) error {
		funding, err := tx.SettlementFunding(ctx, batch.ID)
		if err != nil {
			return err
		}
		if funding.BatchID != batch.ID || funding.IntentID != intent.ID || funding.Amount != 100 {
			t.Fatalf("link=%+v", funding)
		}
		rows, err := tx.Fulfillments(ctx, intent.ID)
		if err != nil {
			return err
		}
		if len(rows) != 1 || rows[0].ID != funding.EffectID {
			t.Fatalf("receipt=%+v", rows)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
