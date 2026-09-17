package pg

import (
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/purchase"
)

func mixedPurchaseFixture(t *testing.T, suffix string, publishPlan bool, planID string) (*Store, *sql.DB, purchase.Intent, *purchase.Service) {
	t.Helper()
	store, db, base := purchaseLifecycleFixture(t, billing.AccountID("fulfillment-"+suffix), suffix)
	ctx := t.Context()
	if publishPlan {
		if err := store.PublishPlan(ctx, assignmentPlan(planID)); err != nil {
			t.Fatal(err)
		}
	}
	service := purchase.New(store.Purchases(), testTime)
	offer, err := service.PublishOffer(ctx, purchase.Offer{
		Account: base.Account, Revision: purchase.Revision{ID: "fulfillment-offer-" + suffix, Version: 1}, Name: "Mixed effects",
		Effects: []purchase.Effect{
			{Key: "credit", Credit: &purchase.CreditBenefit{Unit: billing.Unit{Code: "ai", Scale: 1000}, Amount: 5000, Validity: time.Hour}},
			{Key: "plan", Plan: &purchase.PlanBenefit{PlanVersionID: planID, Quantity: 1, Validity: 24 * time.Hour}},
			{Key: "host", Host: &purchase.HostBenefit{Kind: "workspace", Payload: json.RawMessage(`{"tier":"pro"}`)}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	price, err := service.PublishPrice(ctx, purchase.Price{Account: base.Account, Revision: purchase.Revision{ID: "fulfillment-price-" + suffix, Version: 1}, Offer: offer.Revision, Currency: "USD", UnitAmount: 100, TaxTreatment: purchase.TaxExclusive})
	if err != nil {
		t.Fatal(err)
	}
	quote, err := service.CreateQuote(ctx, purchase.QuoteInput{Account: base.Account, ID: "fulfillment-quote-" + suffix, ValidUntil: base.ExpiresAt, Lines: []purchase.QuoteLineInput{{ID: "fulfillment-line-" + suffix, Price: price.Revision, Quantity: 2}}})
	if err != nil {
		t.Fatal(err)
	}
	in := base.IntentInput
	in.ID, in.Operation, in.QuoteID, in.QuoteFingerprint = "fulfillment-intent-"+suffix, "fulfillment-operation-"+suffix, quote.ID, quote.Fingerprint()
	intent, err := service.CreateIntent(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	return store, db, intent, service
}

func TestPostgresPurchaseFulfillmentPersistsMixedEffects(t *testing.T) {
	store, _, intent, service := mixedPurchaseFixture(t, "mixed", true, "fulfillment-plan-mixed")
	ctx := t.Context()
	fact := purchase.PaymentFact{Account: intent.Account, Scope: intent.Scope, EventID: "fulfillment-event-mixed", TransactionID: "fulfillment-tx-mixed", IntentID: intent.ID, Status: purchase.FactPaid, Currency: "USD", Gross: 200, Lines: []purchase.PaidLine{{LineID: "fulfillment-line-mixed", Gross: 200}}, OccurredAt: testTime(), CollectedAt: testTime()}
	if result, err := service.ApplyPayment(ctx, fact); err != nil || !result.Applied {
		t.Fatalf("payment=%+v err=%v", result, err)
	}
	rows, err := service.Fulfillments(ctx, intent.Account, intent.ID)
	if err != nil || len(rows) != 3 {
		t.Fatalf("fulfillments=%+v err=%v", rows, err)
	}
	var pendingHost bool
	for _, row := range rows {
		if row.Effect.Host != nil {
			pendingHost = row.State == purchase.FulfillmentPending
		} else if row.State != purchase.FulfillmentComplete {
			t.Fatalf("non-host effect not complete: %+v", row)
		}
	}
	if !pendingHost {
		t.Fatal("host effect was not durably left pending")
	}
	balance, err := credit.New(store, testTime).Balance(ctx, intent.Account, "ai", "")
	if err != nil || balance.Available != 10000 {
		t.Fatalf("credit balance=%+v err=%v", balance, err)
	}
	fresh := purchase.New(New(store.db).Purchases(), testTime)
	persisted, err := fresh.Fulfillments(ctx, intent.Account, intent.ID)
	if err != nil || len(persisted) != 3 {
		t.Fatalf("fresh fulfillment read=%+v err=%v", persisted, err)
	}
}

func TestPostgresPurchaseFulfillmentInvalidPlanRollsBackCredit(t *testing.T) {
	store, _, intent, service := mixedPurchaseFixture(t, "invalid-plan", false, "missing-plan-fulfillment")
	ctx := t.Context()
	fact := purchase.PaymentFact{Account: intent.Account, Scope: intent.Scope, EventID: "fulfillment-event-invalid", TransactionID: "fulfillment-tx-invalid", IntentID: intent.ID, Status: purchase.FactPaid, Currency: "USD", Gross: 200, Lines: []purchase.PaidLine{{LineID: "fulfillment-line-invalid-plan", Gross: 200}}, OccurredAt: testTime(), CollectedAt: testTime()}
	if _, err := service.ApplyPayment(ctx, fact); !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("invalid plan error=%v", err)
	}
	balance, err := credit.New(store, testTime).Balance(ctx, intent.Account, "ai", "")
	if err != nil || balance.Available != 0 {
		t.Fatalf("rolled back credit balance=%+v err=%v", balance, err)
	}
	stored, err := service.Intent(ctx, intent.Account, intent.ID)
	if err != nil || stored.Payment != purchase.PaymentPending || stored.Revision != 1 {
		t.Fatalf("rolled back intent=%+v err=%v", stored, err)
	}
}

func TestPostgresPurchaseFulfillmentPaymentCommitFailureRollsBackEffects(t *testing.T) {
	store, db, intent, service := mixedPurchaseFixture(t, "commit-failure", true, "fulfillment-plan-commit")
	ctx := t.Context()
	if _, err := db.ExecContext(ctx, `CREATE FUNCTION fail_fulfillment_payment_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'fulfillment payment commit failure'; END $$; CREATE CONSTRAINT TRIGGER fail_fulfillment_payment_commit AFTER INSERT ON billing_purchase_payment_events DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION fail_fulfillment_payment_commit()`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = db.ExecContext(ctx, `DROP TRIGGER IF EXISTS fail_fulfillment_payment_commit ON billing_purchase_payment_events`)
		_, _ = db.ExecContext(ctx, `DROP FUNCTION IF EXISTS fail_fulfillment_payment_commit()`)
	}()
	fact := purchase.PaymentFact{Account: intent.Account, Scope: intent.Scope, EventID: "fulfillment-event-commit", TransactionID: "fulfillment-tx-commit", IntentID: intent.ID, Status: purchase.FactPaid, Currency: "USD", Gross: 200, Lines: []purchase.PaidLine{{LineID: "fulfillment-line-commit-failure", Gross: 200}}, OccurredAt: testTime(), CollectedAt: testTime()}
	result, err := service.ApplyPayment(ctx, fact)
	if err == nil || result != (purchase.PaymentResult{}) {
		t.Fatalf("commit failure result=%+v err=%v", result, err)
	}
	balance, err := credit.New(store, testTime).Balance(ctx, intent.Account, "ai", "")
	if err != nil || balance.Available != 0 {
		t.Fatalf("failed payment left credit=%+v err=%v", balance, err)
	}
	rows, err := service.Fulfillments(ctx, intent.Account, intent.ID)
	if err != nil || len(rows) != 0 {
		t.Fatalf("failed payment left fulfillments=%+v err=%v", rows, err)
	}
}
