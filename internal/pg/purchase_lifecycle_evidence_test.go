package pg

import (
	"errors"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/purchase"
)

func TestPostgresPurchaseLifecyclePaymentCommitFailureRollsBackAllEvidence(t *testing.T) {
	store, db, intent := purchaseLifecycleFixture(t, "payment-commit", "payment-commit")
	ctx := t.Context()
	service := purchase.New(store.Purchases(), testTime)
	fact := purchase.PaymentFact{Account: intent.Account, Scope: intent.Scope, EventID: "event", TransactionID: "tx", IntentID: intent.ID, Status: purchase.FactCompleted, Currency: "USD", Gross: 1200, Tax: 200, Lines: []purchase.PaidLine{{LineID: "line-payment-commit", Gross: 1200, Tax: 200}}, CollectedAt: testTime(), OccurredAt: testTime().Add(2 * time.Hour)}
	if _, err := db.ExecContext(ctx, `CREATE FUNCTION fail_purchase_payment_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'payment commit failure'; END $$; CREATE CONSTRAINT TRIGGER fail_purchase_payment_commit AFTER INSERT ON billing_purchase_payment_events DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION fail_purchase_payment_commit()`); err != nil {
		t.Fatal(err)
	}
	out, err := service.ApplyPayment(ctx, fact)
	if err == nil || out != (purchase.PaymentResult{}) {
		t.Fatalf("commit failure result=%+v err=%v", out, err)
	}
	var events, funding int
	if err := db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM billing_purchase_payment_events WHERE account_id=$1),(SELECT count(*) FROM billing_purchase_funding WHERE account_id=$1)`, intent.Account).Scan(&events, &funding); err != nil {
		t.Fatal(err)
	}
	if events != 0 || funding != 0 {
		t.Fatalf("rollback left events=%d funding=%d", events, funding)
	}
	unchanged, err := service.Intent(ctx, intent.Account, intent.ID)
	if err != nil || unchanged.Payment != purchase.PaymentPending || unchanged.Revision != 1 {
		t.Fatalf("rollback projection=%+v %v", unchanged, err)
	}
	if _, err := db.ExecContext(ctx, `DROP TRIGGER fail_purchase_payment_commit ON billing_purchase_payment_events`); err != nil {
		t.Fatal(err)
	}
	out, err = service.ApplyPayment(ctx, fact)
	if err != nil || !out.Applied {
		t.Fatalf("late completed retry=%+v %v", out, err)
	}
	current, err := service.Intent(ctx, intent.Account, intent.ID)
	if err != nil || !current.PaidAt.Equal(fact.CollectedAt) {
		t.Fatalf("transfer time=%+v %v", current, err)
	}
	// A fresh Store simulates loss of the service instance; evidence remains exact.
	recovered, err := purchase.New(New(db).Purchases(), testTime).ApplyPayment(ctx, fact)
	if err != nil || recovered != out {
		t.Fatalf("restart replay=%+v %v", recovered, err)
	}
}

func TestPostgresPurchaseLifecycleAllocatesAllLinesAndAppliesCreditEffect(t *testing.T) {
	store, db, base := purchaseLifecycleFixture(t, "purchase-bundle", "bundle")
	ctx := t.Context()
	service := purchase.New(store.Purchases(), testTime)
	offer, err := service.PublishOffer(ctx, purchase.Offer{Account: base.Account, Revision: purchase.Revision{ID: "credits", Version: 1}, Name: "Credits", Effects: []purchase.Effect{{Key: "credits", Credit: &purchase.CreditBenefit{Unit: billing.Unit{Code: "ai", Scale: 1000}, Amount: 5000}}}})
	if err != nil {
		t.Fatal(err)
	}
	price, err := service.PublishPrice(ctx, purchase.Price{Account: base.Account, Revision: purchase.Revision{ID: "credit-price", Version: 1}, Offer: offer.Revision, Currency: "USD", UnitAmount: 100, TaxTreatment: purchase.TaxExclusive})
	if err != nil {
		t.Fatal(err)
	}
	quote, err := service.CreateQuote(ctx, purchase.QuoteInput{Account: base.Account, ID: "bundle-quote", ValidUntil: base.ExpiresAt, Lines: []purchase.QuoteLineInput{{ID: "software", Price: purchase.Revision{ID: "lifecycle-price-bundle", Version: 1}, Quantity: 1}, {ID: "credits", Price: price.Revision, Quantity: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	in := base.IntentInput
	in.ID = "bundle-intent"
	in.Operation = "bundle-create"
	in.QuoteID = quote.ID
	in.QuoteFingerprint = quote.Fingerprint()
	intent, err := service.CreateIntent(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	fact := purchase.PaymentFact{Account: intent.Account, Scope: intent.Scope, IntentID: intent.ID, TransactionID: "bundle-tx", EventID: "mismatch", Status: purchase.FactPaid, Currency: "USD", Gross: 1320, Tax: 220, CollectedAt: testTime(), OccurredAt: testTime(), Lines: []purchase.PaidLine{{LineID: "software", Gross: 1200, Tax: 201}, {LineID: "credits", Gross: 120, Tax: 19}}}
	// A line split that does not match the quote is applied and reported,
	// not refused: the provider is the authority on what it collected, and
	// the customer must not lose a purchase to our bookkeeping.
	bad, err := service.ApplyPayment(ctx, fact)
	if err != nil || !bad.Applied || bad.Discrepancy == "" {
		t.Fatalf("bad allocation=%+v %v", bad, err)
	}
	// A later event for the same transaction reporting different amounts
	// is still refused. That check is about this platform contradicting
	// itself, not about disagreeing with the provider, and it is the one
	// worth keeping: the transaction is already funded at one set of
	// figures and cannot also be funded at another.
	replay := fact
	replay.EventID = "replay"
	replay.Lines = []purchase.PaidLine{{LineID: "software", Gross: 1200, Tax: 200}, {LineID: "credits", Gross: 120, Tax: 20}}
	conflicting, err := service.ApplyPayment(ctx, replay)
	if err != nil || conflicting.Applied || conflicting.Rejection != purchase.RejectAllocation {
		t.Fatalf("conflicting replay=%+v %v", conflicting, err)
	}

	// Repeating the event that was applied is harmless.
	paid, err := service.ApplyPayment(ctx, fact)
	if err != nil || !paid.Applied {
		t.Fatalf("paid=%+v %v", paid, err)
	}
	current, err := service.Intent(ctx, intent.Account, intent.ID)
	if err != nil || current.Payment != purchase.PaymentPaid || current.Fulfillment != purchase.FulfillmentComplete {
		t.Fatalf("bundle state=%+v %v", current, err)
	}
	funding, err := service.Funding(ctx, intent.Account, intent.Scope, fact.TransactionID)
	if err != nil || len(funding.Lines) != 2 || funding.Gross != 1320 || funding.Tax != 220 {
		t.Fatalf("allocations=%+v %v", funding, err)
	}
	balance, err := credit.New(store, testTime).Balance(ctx, intent.Account, "ai", "")
	if err != nil || balance.Available != 5000 {
		t.Fatalf("credit effect=%+v %v", balance, err)
	}
	// Relational and JSON evidence must agree even when another valid intent exists.
	if _, err := db.ExecContext(ctx, `UPDATE billing_purchase_payment_events SET intent_id=$1 WHERE account_id=$2 AND event_id=$3`, base.ID, intent.Account, fact.EventID); err != nil {
		t.Fatal(err)
	}
	out, err := service.ApplyPayment(ctx, fact)
	if !errors.Is(err, billing.ErrConflict) || out != (purchase.PaymentResult{}) {
		t.Fatalf("tampered replay=%+v %v", out, err)
	}
}
