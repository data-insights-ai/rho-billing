package pg

import (
	"errors"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/catalog"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/purchase"
	"github.com/data-insights-ai/rho-billing/subscription"
	"github.com/data-insights-ai/rho-billing/usage"
)

func TestPostgresOneTransactionFundsCreditPlanAndUsageOnce(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("lib18-multi")
	if err := store.CreateAccount(ctx, account, string(account)); err != nil {
		t.Fatal(err)
	}
	period := settlementTestPeriod()
	record := settlementTestRecord(t, store, string(account), "lib18-usage", period.Start.Add(time.Hour), period.Start.Add(2*time.Hour), 7)
	if _, err := recordUsage(ctx, store, record); err != nil {
		t.Fatal(err)
	}
	batch, err := finishSettlementClose(ctx, store.Settlements(), store.now, usage.CloseInput{
		Account: account, Operation: "lib18-close", BatchID: "lib18-batch", Period: period, Currency: "USD", CreatedAt: period.Cutoff,
	})
	if err != nil {
		t.Fatal(err)
	}
	plan := assignmentPlan("lib18-plan")
	if err := store.PublishPlan(ctx, plan); err != nil {
		t.Fatal(err)
	}

	svc := purchase.New(store.Purchases(), testTime)
	creditOffer, err := svc.PublishOffer(ctx, purchase.Offer{
		Account: account, Revision: purchase.Revision{ID: "lib18-credit-offer", Version: 1}, Name: "Top-up",
		Effects: []purchase.Effect{{Key: "credits", Credit: &purchase.CreditBenefit{Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 25}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	planOffer, err := svc.PublishOffer(ctx, purchase.Offer{
		Account: account, Revision: purchase.Revision{ID: "lib18-plan-offer", Version: 1}, Name: "Plan term",
		Effects: []purchase.Effect{{Key: "plan", Plan: &purchase.PlanBenefit{PlanVersionID: plan.ID, Quantity: 1, Validity: 24 * time.Hour}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	usageOffer, err := svc.PublishOffer(ctx, purchase.Offer{
		Account: account, Revision: purchase.Revision{ID: "lib18-usage-offer", Version: 1}, Name: "Usage settlement",
		Effects: []purchase.Effect{{Key: "usage", Settlement: &purchase.SettlementBenefit{}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	creditPrice, err := svc.PublishPrice(ctx, purchase.Price{Account: account, Revision: purchase.Revision{ID: "lib18-credit-price", Version: 1}, Offer: creditOffer.Revision, Currency: "USD", UnitAmount: 500, TaxTreatment: purchase.TaxExclusive})
	if err != nil {
		t.Fatal(err)
	}
	planPrice, err := svc.PublishPrice(ctx, purchase.Price{Account: account, Revision: purchase.Revision{ID: "lib18-plan-price", Version: 1}, Offer: planOffer.Revision, Currency: "USD", UnitAmount: 1000, TaxTreatment: purchase.TaxExclusive})
	if err != nil {
		t.Fatal(err)
	}
	usagePrice, err := svc.PublishPrice(ctx, purchase.Price{Account: account, Revision: purchase.Revision{ID: "lib18-usage-price", Version: 1}, Offer: usageOffer.Revision, Currency: "USD", UnitAmount: batch.Total, TaxTreatment: purchase.TaxExclusive})
	if err != nil {
		t.Fatal(err)
	}
	quote, err := svc.CreateQuote(ctx, purchase.QuoteInput{
		Account: account, ID: "lib18-quote", ValidUntil: testTime().Add(time.Hour),
		Lines: []purchase.QuoteLineInput{
			{ID: "credit-line", Price: creditPrice.Revision, Quantity: 1},
			{ID: "plan-line", Price: planPrice.Revision, Quantity: 1},
			{ID: "usage-line", Price: usagePrice.Revision, Quantity: 1, SettlementBatchID: batch.ID},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantGross := int64(1500) + batch.Total
	if quote.Amount != wantGross || quote.Currency != "USD" {
		t.Fatalf("quote=%+v, want amount %d", quote, wantGross)
	}

	scope := billing.Scope{Provider: "paddle", Merchant: "merchant", Environment: "sandbox"}
	intent, err := svc.CreateIntent(ctx, purchase.IntentInput{
		Account: account, ID: "lib18-intent", Operation: "lib18-buy", QuoteID: quote.ID, QuoteFingerprint: quote.Fingerprint(),
		Scope: scope, Actor: "owner", Reason: "bundle checkout", ExpiresAt: quote.ValidUntil,
	})
	if err != nil {
		t.Fatal(err)
	}

	binding, err := svc.BindCollection(ctx, purchase.CollectionInput{
		Account: account, Scope: scope, TransactionID: "lib18-txn", IntentID: intent.ID,
		QuoteFingerprint: quote.Fingerprint(), CustomerID: "ctm_lib18",
		Lines: []purchase.CollectionLine{
			{
				ProviderLineID: "txnitm_bundle", ProviderPriceID: "pri_bundle", Quantity: 2,
				Allocations: []purchase.CollectionAllocation{
					{QuoteLineID: "credit-line", Quantity: 1},
					{QuoteLineID: "plan-line", Quantity: 1},
				},
			},
			{
				ProviderLineID: "txnitm_usage", ProviderPriceID: "pri_usage", Quantity: 1,
				Allocations: []purchase.CollectionAllocation{{QuoteLineID: "usage-line", Quantity: 1}},
			},
		},
		Actor: "owner", Reason: "verified checkout", EvidenceReference: "checkout-evidence",
	})
	if err != nil || len(binding.Lines) != 2 {
		t.Fatalf("binding=%+v err=%v", binding, err)
	}

	fact := purchase.PaymentFact{
		Account: account, Scope: scope, EventID: "lib18-paid", TransactionID: "lib18-txn", IntentID: intent.ID,
		Status: purchase.FactPaid, Currency: "USD", Gross: wantGross,
		Lines: []purchase.PaidLine{
			{LineID: "credit-line", Gross: 500},
			{LineID: "plan-line", Gross: 1000},
			{LineID: "usage-line", Gross: batch.Total},
		},
		OccurredAt: testTime(), CollectedAt: testTime(),
	}
	paid, err := svc.ApplyPayment(ctx, fact)
	if err != nil || !paid.Applied {
		t.Fatalf("payment=%+v err=%v", paid, err)
	}
	replay, err := svc.ApplyPayment(ctx, fact)
	if err != nil || replay != paid {
		t.Fatalf("payment replay=%+v err=%v", replay, err)
	}

	got, err := credit.New(store, testTime).Balance(ctx, account, "credits", "")
	if err != nil || got.Available != 25 {
		t.Fatalf("top-up balance=%+v err=%v", got, err)
	}
	fulfillments, err := svc.Fulfillments(ctx, account, intent.ID)
	if err != nil || len(fulfillments) != 3 {
		t.Fatalf("fulfillments=%+v err=%v", fulfillments, err)
	}
	var planEffect purchase.Fulfillment
	for _, row := range fulfillments {
		if row.State != purchase.FulfillmentComplete {
			t.Fatalf("incomplete effect %+v", row)
		}
		if row.Effect.Plan != nil {
			planEffect = row
		}
	}
	assigned, err := catalog.NewEntitlement(store.Entitlements(), testTime).Assignment(ctx, account, planEffect.ID)
	if err != nil || assigned.Plan.PlanVersionID != plan.ID || assigned.Plan.Source != catalog.SourcePurchase || assigned.Plan.Perpetual {
		t.Fatalf("plan assignment=%+v err=%v", assigned, err)
	}
	resolved, err := catalog.ResolveEntitlements(catalog.EntitlementInput{
		At:          testTime().Add(time.Hour),
		Assignments: []catalog.PlanAssignment{assigned.Plan},
		Versions:    []catalog.PlanVersion{plan},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := resolved.Get("feature"); !ok {
		t.Fatalf("plan benefit missing: %+v", resolved)
	}
	var links int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM billing_purchase_settlement_funding WHERE account_id=$1 AND batch_id=$2`, account, batch.ID).Scan(&links); err != nil || links != 1 {
		t.Fatalf("usage funding links=%d err=%v", links, err)
	}
	var subscriptions int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM billing_subscriptions WHERE account_id=$1`, account).Scan(&subscriptions); err != nil || subscriptions != 0 {
		t.Fatalf("purchase created provider subscriptions=%d err=%v", subscriptions, err)
	}
	if _, err := subscription.New(store.Subscriptions()).Subscription(ctx, account, billing.Reference{Scope: scope, ID: "lib18-txn"}); !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("plan benefit fabricated a provider subscription: %v", err)
	}

	if again, err := svc.Fulfill(ctx, account, intent.ID); err != nil || again.Fulfillment != purchase.FulfillmentComplete {
		t.Fatalf("fulfill replay=%+v err=%v", again, err)
	}
	if got, err := credit.New(store, testTime).Balance(ctx, account, "credits", ""); err != nil || got.Available != 25 {
		t.Fatalf("double fulfillment credit=%+v err=%v", got, err)
	}

	dup := purchase.IntentInput{
		Account: account, ID: "lib18-dup", Operation: "lib18-dup-buy", QuoteID: quote.ID, QuoteFingerprint: quote.Fingerprint(),
		Scope: scope, Actor: "owner", Reason: "second buy", ExpiresAt: quote.ValidUntil,
	}
	if _, err := svc.CreateIntent(ctx, dup); err != nil {
		t.Fatal(err)
	}
	dupFact := fact
	dupFact.IntentID = dup.ID
	dupFact.EventID = "lib18-dup-paid"
	dupFact.TransactionID = "lib18-dup-txn"
	if out, err := svc.ApplyPayment(ctx, dupFact); err == nil || out.Applied {
		t.Fatalf("duplicate usage settlement accepted=%+v err=%v", out, err)
	}
}
