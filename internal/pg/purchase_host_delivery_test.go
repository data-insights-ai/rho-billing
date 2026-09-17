package pg

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/purchase"
)

func TestPostgresHostDeliveryRecoversAfterHostCommitBeforeAcknowledgment(t *testing.T) {
	store, db, base := purchaseLifecycleFixture(t, "host-delivery", "host-delivery")
	ctx := t.Context()
	service := purchase.New(store.Purchases(), testTime)
	offer, err := service.PublishOffer(ctx, purchase.Offer{Account: base.Account, Revision: purchase.Revision{ID: "host-offer", Version: 1}, Name: "Provision workspace", Effects: []purchase.Effect{{Key: "credits", Credit: &purchase.CreditBenefit{Unit: billing.Unit{Code: "ai", Scale: 1000}, Amount: 5000, Validity: time.Hour}}, {Key: "host", Host: &purchase.HostBenefit{Kind: "provision", Payload: json.RawMessage(`{"capacity":9007199254740993}`)}}}})
	if err != nil {
		t.Fatal(err)
	}
	price, err := service.PublishPrice(ctx, purchase.Price{Account: base.Account, Revision: purchase.Revision{ID: "host-price", Version: 1}, Offer: offer.Revision, Currency: "USD", UnitAmount: 100, TaxTreatment: purchase.TaxExclusive})
	if err != nil {
		t.Fatal(err)
	}
	quote, err := service.CreateQuote(ctx, purchase.QuoteInput{Account: base.Account, ID: "host-quote", ValidUntil: base.ExpiresAt, Lines: []purchase.QuoteLineInput{{ID: "host-line", Price: price.Revision, Quantity: 2}}})
	if err != nil {
		t.Fatal(err)
	}
	in := base.IntentInput
	in.ID = "host-intent"
	in.Operation = "host-create"
	in.QuoteID = quote.ID
	in.QuoteFingerprint = quote.Fingerprint()
	intent, err := service.CreateIntent(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	fact := purchase.PaymentFact{Account: in.Account, Scope: in.Scope, IntentID: in.ID, EventID: "paid", TransactionID: "host-tx", Status: purchase.FactCompleted, Currency: "USD", Gross: 200, Lines: []purchase.PaidLine{{LineID: "host-line", Gross: 200}}, CollectedAt: testTime(), OccurredAt: testTime()}
	if out, err := service.ApplyPayment(ctx, fact); err != nil || !out.Applied {
		t.Fatalf("payment=%+v %v", out, err)
	}
	balance, err := credit.New(store, testTime).Balance(ctx, in.Account, "ai", "")
	if err != nil || balance.Available != 10000 {
		t.Fatalf("credit effect=%+v %v", balance, err)
	}
	deliveries, err := service.Fulfillments(ctx, in.Account, intent.ID)
	if err != nil || len(deliveries) != 2 {
		t.Fatalf("deliveries=%+v %v", deliveries, err)
	}
	var delivery purchase.Fulfillment
	for _, v := range deliveries {
		if v.Effect.Host != nil {
			delivery = v
		}
	}
	if delivery.ID == "" || delivery.State != purchase.FulfillmentPending || delivery.Quantity != 2 {
		t.Fatalf("host delivery=%+v", delivery)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE host_effects (effect_id text PRIMARY KEY, fingerprint text NOT NULL, resource_id text NOT NULL, applied_at timestamptz NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	// The host commits its resource and deduplication identity together. This
	// fixture's resource row represents that host-owned application transaction.
	for range 2 {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO host_effects VALUES ($1,$2,$3,$4) ON CONFLICT DO NOTHING`, delivery.ID, delivery.Fingerprint(), "workspace-42", testTime())
		if err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		var fingerprint string
		if err = tx.QueryRowContext(ctx, `SELECT fingerprint FROM host_effects WHERE effect_id=$1`, delivery.ID).Scan(&fingerprint); err != nil || fingerprint != delivery.Fingerprint() {
			_ = tx.Rollback()
			t.Fatalf("host replay conflict: %v", err)
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
		// A worker may disappear here. Redelivery repeats the host transaction and
		// obtains the same resource rather than provisioning another one.
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM host_effects`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("host effects=%d %v", count, err)
	}
	ack := purchase.Acknowledgment{Account: in.Account, EffectID: delivery.ID, Fingerprint: delivery.Fingerprint(), HostReference: "workspace-42", AppliedAt: testTime()}
	// Losing the library commit must not lose the host's completed resource.
	if _, err := db.ExecContext(ctx, `CREATE FUNCTION fail_host_ack_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'ack commit failure'; END $$; CREATE CONSTRAINT TRIGGER fail_host_ack_commit AFTER UPDATE ON billing_purchase_intents DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION fail_host_ack_commit()`); err != nil {
		t.Fatal(err)
	}
	failed, err := service.Acknowledge(ctx, ack)
	if err == nil || failed.ID != "" {
		t.Fatalf("failed ack=%+v %v", failed, err)
	}
	if _, err := db.ExecContext(ctx, `DROP TRIGGER fail_host_ack_commit ON billing_purchase_intents`); err != nil {
		t.Fatal(err)
	}
	recovered := purchase.New(New(db).Purchases(), testTime)
	first, err := recovered.Acknowledge(ctx, ack)
	if err != nil || first.State != purchase.FulfillmentComplete {
		t.Fatalf("ack=%+v %v", first, err)
	}
	replay, err := recovered.Acknowledge(ctx, ack)
	if err != nil || replay.RecordFingerprint() != first.RecordFingerprint() {
		t.Fatalf("ack replay=%+v %v", replay, err)
	}
	ack.HostReference = "another-resource"
	if _, err := recovered.Acknowledge(ctx, ack); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("changed host result=%v", err)
	}
	current, err := recovered.Intent(ctx, in.Account, in.ID)
	if err != nil || current.Fulfillment != purchase.FulfillmentComplete {
		t.Fatalf("intent=%+v %v", current, err)
	}
	balance, err = credit.New(store, testTime).Balance(ctx, in.Account, "ai", "")
	if err != nil || balance.Available != 10000 {
		t.Fatalf("redelivery changed credits=%+v %v", balance, err)
	}
}

func TestPostgresDelayedFulfillmentDoesNotRenewExpiredPurchasedCredits(t *testing.T) {
	store, _, intent, _ := mixedPurchaseFixture(t, "expired-delivery", true, "expired-plan")
	clock := func() time.Time { return testTime().Add(2 * time.Hour) }
	service := purchase.New(store.Purchases(), clock)
	fact := purchase.PaymentFact{Account: intent.Account, Scope: intent.Scope, IntentID: intent.ID, EventID: "late-paid", TransactionID: "late-tx", Status: purchase.FactCompleted, Currency: "USD", Gross: 200, Lines: []purchase.PaidLine{{LineID: "fulfillment-line-expired-delivery", Gross: 200}}, CollectedAt: testTime(), OccurredAt: clock()}
	if result, err := service.ApplyPayment(t.Context(), fact); err != nil || !result.Applied {
		t.Fatalf("late payment=%+v %v", result, err)
	}
	balance, err := credit.New(store, clock).Balance(t.Context(), intent.Account, "ai", "")
	if err != nil || balance.Available != 0 || balance.Expired != 10000 {
		t.Fatalf("expired credit=%+v %v", balance, err)
	}
	if _, err := service.Fulfill(t.Context(), intent.Account, intent.ID); err != nil {
		t.Fatal(err)
	}
	after, err := credit.New(store, clock).Balance(t.Context(), intent.Account, "ai", "")
	if err != nil || after != balance {
		t.Fatalf("recovery refreshed expiry=%+v %v", after, err)
	}
}
