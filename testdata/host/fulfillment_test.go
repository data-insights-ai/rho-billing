package host_test

import (
	"errors"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/purchase"
)

func TestHostFulfillsAndAcknowledgesHostEffect(t *testing.T) {
	ctx := t.Context()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	account := billing.AccountID("host-fulfillment")
	service := purchase.New(purchase.NewMemoryRepository(purchase.ReferenceAccount{Account: account}), func() time.Time { return now })
	offer := purchase.Offer{
		Account: account, Revision: purchase.Revision{ID: "offer-host", Version: 1}, Name: "Hosted feature",
		Effects: []purchase.Effect{{Key: "host", Host: &purchase.HostBenefit{Kind: "provision", Payload: []byte(`{"tier":"pro"}`)}}},
	}
	if _, err := service.PublishOffer(ctx, offer); err != nil {
		t.Fatal(err)
	}
	price := purchase.Price{Account: account, Revision: purchase.Revision{ID: "price-host", Version: 1}, Offer: offer.Revision, Currency: "USD", UnitAmount: 100, TaxTreatment: purchase.TaxExclusive}
	if _, err := service.PublishPrice(ctx, price); err != nil {
		t.Fatal(err)
	}
	quote, err := service.CreateQuote(ctx, purchase.QuoteInput{Account: account, ID: "quote-host", ValidUntil: now.Add(time.Hour), Lines: []purchase.QuoteLineInput{{ID: "line-host", Price: price.Revision, Quantity: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	scope := billing.Scope{Provider: "stripe", Merchant: "host-merchant", Environment: "test"}
	intent, err := service.CreateIntent(ctx, purchase.IntentInput{Account: account, ID: "intent-host", Operation: "operation-host", QuoteID: quote.ID, QuoteFingerprint: quote.Fingerprint(), Scope: scope, Actor: "host-actor", Reason: "host feature", ExpiresAt: now.Add(30 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.ApplyPayment(ctx, purchase.PaymentFact{Account: account, Scope: scope, EventID: "event-host", TransactionID: "transaction-host", IntentID: intent.ID, Status: purchase.FactPaid, Currency: "USD", Gross: 100, Lines: []purchase.PaidLine{{LineID: "line-host", Gross: 100}}, OccurredAt: now, CollectedAt: now})
	if err != nil || !result.Applied {
		t.Fatalf("payment result=%+v error=%v", result, err)
	}
	if _, err := service.Fulfill(ctx, account, intent.ID); err != nil {
		t.Fatal(err)
	}
	rows, err := service.Fulfillments(ctx, account, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].State != purchase.FulfillmentPending {
		t.Fatalf("pending fulfillments=%+v", rows)
	}
	ack := purchase.Acknowledgment{Account: account, EffectID: rows[0].ID, Fingerprint: rows[0].Fingerprint(), HostReference: "host-provision-1", AppliedAt: now}
	completed, err := service.Acknowledge(ctx, ack)
	if err != nil {
		t.Fatal(err)
	}
	if completed.State != purchase.FulfillmentComplete || completed.HostReference != ack.HostReference {
		t.Fatalf("completed fulfillment=%+v", completed)
	}
	replay, err := service.Acknowledge(ctx, ack)
	if err != nil {
		t.Fatal(err)
	}
	if replay.RecordFingerprint() != completed.RecordFingerprint() {
		t.Fatalf("ack replay changed record: first=%s replay=%s", completed.RecordFingerprint(), replay.RecordFingerprint())
	}
	stored, err := service.Intent(ctx, account, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Fulfillment != purchase.FulfillmentComplete {
		t.Fatalf("intent after host acknowledgment=%+v", stored)
	}
}

func TestHostObservesPublicHostReversal(t *testing.T) {
	ctx := t.Context()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	account := billing.AccountID("host-reversal")
	service := purchase.New(purchase.NewMemoryRepository(purchase.ReferenceAccount{Account: account}), func() time.Time { return now })
	offer := purchase.Offer{Account: account, Revision: purchase.Revision{ID: "offer-reversal", Version: 1}, Name: "Reversible host", Effects: []purchase.Effect{{Key: "host", Host: &purchase.HostBenefit{Kind: "provision", Payload: []byte(`{"tier":"pro"}`)}}}}
	if _, err := service.PublishOffer(ctx, offer); err != nil {
		t.Fatal(err)
	}
	price := purchase.Price{Account: account, Revision: purchase.Revision{ID: "price-reversal", Version: 1}, Offer: offer.Revision, Currency: "USD", UnitAmount: 100, TaxTreatment: purchase.TaxExclusive}
	if _, err := service.PublishPrice(ctx, price); err != nil {
		t.Fatal(err)
	}
	quote, err := service.CreateQuote(ctx, purchase.QuoteInput{Account: account, ID: "quote-reversal", ValidUntil: now.Add(time.Hour), Lines: []purchase.QuoteLineInput{{ID: "line-reversal", Price: price.Revision, Quantity: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	scope := billing.Scope{Provider: "stripe", Merchant: "host-reversal", Environment: "test"}
	intent, err := service.CreateIntent(ctx, purchase.IntentInput{Account: account, ID: "intent-reversal", Operation: "operation-reversal", QuoteID: quote.ID, QuoteFingerprint: quote.Fingerprint(), Scope: scope, Actor: "host", Reason: "reversal", ExpiresAt: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ApplyPayment(ctx, purchase.PaymentFact{Account: account, Scope: scope, EventID: "event-reversal", TransactionID: "tx-reversal", IntentID: intent.ID, Status: purchase.FactPaid, Currency: "USD", Gross: 100, Lines: []purchase.PaidLine{{LineID: "line-reversal", Gross: 100}}, OccurredAt: now, CollectedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Fulfill(ctx, account, intent.ID); err != nil {
		t.Fatal(err)
	}
	rows, err := service.Fulfillments(ctx, account, intent.ID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	in := purchase.AdjustmentInput{Account: account, ID: "adjustment-reversal", IntentID: intent.ID, ProviderAdjustmentID: "provider-reversal", TransactionID: "tx-reversal", Scope: scope, Kind: purchase.AdjustmentRefund, Currency: "USD", Lines: []purchase.PaidLine{{LineID: "line-reversal", Gross: 100}}, PolicyVersion: "host-v1", CreditPolicy: purchase.CreditRefundFullOnly, Actor: "host", Reason: "customer refund", OccurredAt: now}
	if result, err := service.ApplyAdjustment(ctx, in); err != nil || !result.Applied {
		t.Fatalf("adjustment=%+v err=%v", result, err)
	}
	rows, err = service.Fulfillments(ctx, account, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].State != purchase.FulfillmentCanceled {
		t.Fatalf("reversal state=%+v", rows[0])
	}
	ack := purchase.Acknowledgment{Account: account, EffectID: rows[0].ID, Fingerprint: rows[0].Fingerprint(), HostReference: "late-host-delivery", AppliedAt: now}
	if _, err := service.Acknowledge(ctx, ack); !errors.Is(err, billing.ErrState) {
		t.Fatalf("late acknowledgment err=%v", err)
	}
}
