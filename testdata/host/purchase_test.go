package host_test

import (
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/postgres"
	"github.com/data-insights-ai/rho-billing/purchase"
)

func TestHostCreatesMoneyOnlyPurchaseQuote(t *testing.T) {
	ctx := t.Context()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	account := billing.AccountID("host-purchase")
	service := purchase.New(purchase.NewMemoryRepository(purchase.ReferenceAccount{Account: account}), func() time.Time { return now })
	offer := purchase.Offer{
		Account:  account,
		Revision: purchase.Revision{ID: "offer-money", Version: 1},
		Name:     "Money only",
	}
	if _, err := service.PublishOffer(ctx, offer); err != nil {
		t.Fatal(err)
	}
	price := purchase.Price{
		Account:      account,
		Revision:     purchase.Revision{ID: "price-money", Version: 1},
		Offer:        offer.Revision,
		Currency:     "USD",
		UnitAmount:   1250,
		TaxTreatment: purchase.TaxExclusive,
	}
	if _, err := service.PublishPrice(ctx, price); err != nil {
		t.Fatal(err)
	}
	quote, err := service.CreateQuote(ctx, purchase.QuoteInput{
		Account:    account,
		ID:         "quote-money",
		ValidUntil: now.Add(time.Hour),
		Lines: []purchase.QuoteLineInput{{
			ID:       "line-money",
			Price:    price.Revision,
			Quantity: 2,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if quote.Amount != 2500 || quote.Currency != "USD" || len(quote.Lines) != 1 {
		t.Fatalf("quote=%+v", quote)
	}
	if len(quote.Lines[0].Offer.Effects) != 0 {
		t.Fatalf("money-only offer unexpectedly has effects: %+v", quote.Lines[0].Offer.Effects)
	}
	scope := billing.Scope{Provider: "stripe", Merchant: "host-merchant", Environment: "test"}
	intent, err := service.CreateIntent(ctx, purchase.IntentInput{
		Account: account, ID: "intent-money", Operation: "operation-money",
		QuoteID: quote.ID, QuoteFingerprint: quote.Fingerprint(), Scope: scope,
		Actor: "host-actor", Reason: "customer checkout", ExpiresAt: now.Add(30 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if intent.Command != purchase.CommandPlanned || intent.Payment != purchase.PaymentPending {
		t.Fatalf("new intent=%+v", intent)
	}
	result, err := service.ApplyPayment(ctx, purchase.PaymentFact{
		Account: account, Scope: scope, EventID: "event-money", TransactionID: "transaction-money",
		IntentID: intent.ID, Status: purchase.FactPaid, Currency: "USD", Gross: 2500,
		Lines: []purchase.PaidLine{{LineID: "line-money", Gross: 2500}}, OccurredAt: now.Add(time.Minute), CollectedAt: now.Add(time.Minute),
	})
	if err != nil || !result.Applied {
		t.Fatalf("payment result=%+v error=%v", result, err)
	}
	stored, err := service.Intent(ctx, account, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Command != purchase.CommandPlanned || stored.Payment != purchase.PaymentPaid || stored.Fulfillment != purchase.FulfillmentComplete {
		t.Fatalf("paid money-only intent=%+v", stored)
	}
	now = now.Add(10 * time.Minute)
	debit, err := service.ApplyAdjustment(ctx, purchase.AdjustmentInput{
		Account: account, ID: "host-chargeback", IntentID: intent.ID,
		ProviderAdjustmentID: "provider-chargeback", TransactionID: "transaction-money",
		Scope: scope, Kind: purchase.AdjustmentChargeback, Currency: "USD",
		Lines:         []purchase.PaidLine{{LineID: "line-money", Gross: 2500}},
		PolicyVersion: "host-policy-1", CreditPolicy: purchase.CreditRefundProportional,
		Actor: "host-worker", Reason: "verified chargeback", OccurredAt: now,
	})
	if err != nil || !debit.Applied {
		t.Fatalf("chargeback=%+v error=%v", debit, err)
	}
	fact := purchase.DisputeFact{
		Account: account, Scope: scope, EventID: "case-lost", DisputeID: "host-case",
		IntentID: intent.ID, TransactionID: "transaction-money", Currency: "USD",
		Amount: 2500, Status: purchase.DisputeLost, OccurredAt: now,
		EvidenceReference: "verified-case", DebitAdjustmentID: debit.ID,
	}
	if result, err := service.ApplyDispute(ctx, fact); err != nil || !result.StatusApplied {
		t.Fatalf("lost case=%+v error=%v", result, err)
	}
	now = now.Add(time.Minute)
	fact.EventID, fact.Status, fact.OccurredAt = "case-won", purchase.DisputeWon, now
	if result, err := service.ApplyDispute(ctx, fact); err != nil || !result.StatusApplied || result.RecoveryApplied {
		t.Fatalf("won case=%+v error=%v", result, err)
	}
	caseValue, err := service.Dispute(ctx, account, scope, fact.DisputeID)
	if err != nil || caseValue.Status != purchase.DisputeWon || len(caseValue.Recovered) != 0 {
		t.Fatalf("status alone recovered funds: %+v error=%v", caseValue, err)
	}
	fact.EventID = "case-recovery"
	fact.Recovery = &purchase.DisputeRecovery{
		ID: "provider-recovery", AdjustmentID: debit.ID, EvidenceReference: "verified-reversal",
		Lines: []purchase.PaidLine{{LineID: "line-money", Gross: 2500}},
	}
	if result, err := service.ApplyDispute(ctx, fact); err != nil || !result.RecoveryApplied {
		t.Fatalf("recovery=%+v error=%v", result, err)
	}
	caseValue, err = service.Dispute(ctx, account, scope, fact.DisputeID)
	if err != nil || len(caseValue.Recovered) != 1 || caseValue.Recovered[0].Gross != 2500 {
		t.Fatalf("recovered case=%+v error=%v", caseValue, err)
	}
}

func TestHostCanConstructDurablePurchaseAndEntitlementRepositories(t *testing.T) {
	store := postgres.New(nil)
	if store.Purchases() == nil {
		t.Fatal("missing durable purchase repository")
	}
	if store.Entitlements() == nil {
		t.Fatal("missing durable entitlement repository")
	}
}
