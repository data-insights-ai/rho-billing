package pg

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/integration"
	"github.com/data-insights-ai/rho-billing/purchase"
)

func purchaseLifecycleFixture(t *testing.T, account billing.AccountID, suffix string) (*Store, *sql.DB, purchase.Intent) {
	t.Helper()
	store, db := testStore(t)
	ctx := t.Context()
	if err := store.CreateAccount(ctx, account, string(account)); err != nil {
		t.Fatal(err)
	}
	commercial := purchase.New(store.Purchases(), testTime)
	offer := purchase.Offer{Account: account, Revision: purchase.Revision{ID: "lifecycle-offer-" + suffix, Version: 1}, Name: "Money-only", PublishedAt: testTime()}
	if _, err := commercial.PublishOffer(ctx, offer); err != nil {
		t.Fatal(err)
	}
	price := purchase.Price{Account: account, Revision: purchase.Revision{ID: "lifecycle-price-" + suffix, Version: 1}, Offer: offer.Revision, Currency: "USD", UnitAmount: 1000, TaxTreatment: purchase.TaxExclusive, PublishedAt: testTime()}
	if _, err := commercial.PublishPrice(ctx, price); err != nil {
		t.Fatal(err)
	}
	quote, err := commercial.CreateQuote(ctx, purchase.QuoteInput{Account: account, ID: "lifecycle-quote-" + suffix, ValidUntil: testTime().Add(time.Hour), Lines: []purchase.QuoteLineInput{{ID: "line-" + suffix, Price: price.Revision, Quantity: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	scope := billing.Scope{Provider: "test", Merchant: "merchant", Environment: "sandbox"}
	intent, err := commercial.CreateIntent(ctx, purchase.IntentInput{Account: account, ID: "lifecycle-intent-" + suffix, Operation: "lifecycle-operation-" + suffix, QuoteID: quote.ID, QuoteFingerprint: quote.Fingerprint(), Scope: scope, Actor: "operator", Reason: "lifecycle test", ExpiresAt: testTime().Add(30 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	return store, db, intent
}

func TestPostgresPurchaseLifecycleMoneyOnlyPaidFundingAndReplay(t *testing.T) {
	store, _, intent := purchaseLifecycleFixture(t, "purchase-lifecycle-money", "money")
	ctx := t.Context()
	service := purchase.New(store.Purchases(), testTime)
	fact := purchase.PaymentFact{Account: intent.Account, Scope: intent.Scope, EventID: "money-event", TransactionID: "money-transaction", IntentID: intent.ID, Status: purchase.FactPaid, Currency: intent.Currency, Gross: intent.Amount, Lines: []purchase.PaidLine{{LineID: "line-money", Gross: intent.Amount}}, OccurredAt: testTime(), CollectedAt: testTime(), Payload: []byte(`{"source":"test"}`)}
	first, err := service.ApplyPayment(ctx, fact)
	if err != nil || !first.Applied {
		t.Fatalf("paid fact result=%+v err=%v", first, err)
	}
	replay, err := service.ApplyPayment(ctx, fact)
	if err != nil || replay != first {
		t.Fatalf("paid replay=%+v err=%v first=%+v", replay, err, first)
	}
	changed := fact
	changed.Payload = []byte(`{"source":"changed"}`)
	if _, err := service.ApplyPayment(ctx, changed); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("changed payment replay error=%v", err)
	}
	funding, err := service.Funding(ctx, intent.Account, intent.Scope, fact.TransactionID)
	if err != nil || funding.IntentID != intent.ID || len(funding.Lines) != 1 {
		t.Fatalf("funding=%+v err=%v", funding, err)
	}
	updated, err := service.Intent(ctx, intent.Account, intent.ID)
	if err != nil || updated.Payment != purchase.PaymentPaid || updated.Fulfillment != purchase.FulfillmentComplete {
		t.Fatalf("updated intent=%+v err=%v", updated, err)
	}
}

func TestPostgresPurchaseLifecycleUnknownCommandSurvivesRead(t *testing.T) {
	store, _, intent := purchaseLifecycleFixture(t, "purchase-lifecycle-command", "command")
	ctx := t.Context()
	service := purchase.New(store.Purchases(), testTime)
	accepted, err := service.RecordCommand(ctx, purchase.CommandInput{Account: intent.Account, IntentID: intent.ID, Operation: intent.Operation, ExpectedRevision: intent.Revision, State: purchase.CommandDispatched, ProviderReference: "dispatch-ref", OccurredAt: testTime()})
	if err != nil {
		t.Fatal(err)
	}
	unknown, err := service.RecordCommand(ctx, purchase.CommandInput{Account: intent.Account, IntentID: intent.ID, Operation: intent.Operation + "-unknown", ExpectedRevision: accepted.Revision, State: purchase.CommandUnknown, ProviderReference: "unknown-ref", OccurredAt: testTime().Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := service.Intent(ctx, intent.Account, intent.ID)
	if err != nil || stored.Command != purchase.CommandUnknown || unknown.Command != purchase.CommandUnknown {
		t.Fatalf("stored=%+v unknown=%+v err=%v", stored, unknown, err)
	}
}

func TestPostgresPurchaseLifecycleHostRollbackAndFailedCommitZero(t *testing.T) {
	store, db, intent := purchaseLifecycleFixture(t, "purchase-lifecycle-rollback", "rollback")
	ctx := t.Context()
	sentinel := errors.New("host rollback")
	err := store.Atomic(ctx, intent.Account, func(v integration.Session) error {
		_, err := purchase.New(v.(*session).Purchases(), testTime).RecordCommand(ctx, purchase.CommandInput{Account: intent.Account, IntentID: intent.ID, Operation: intent.Operation + "-rollback", ExpectedRevision: intent.Revision, State: purchase.CommandDispatched, ProviderReference: "rollback-ref", OccurredAt: testTime()})
		if err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("host rollback error=%v", err)
	}
	unchanged, err := purchase.New(store.Purchases(), testTime).Intent(ctx, intent.Account, intent.ID)
	if err != nil || unchanged.Revision != intent.Revision {
		t.Fatalf("rollback changed intent=%+v err=%v", unchanged, err)
	}
	if _, err := db.ExecContext(ctx, `CREATE FUNCTION fail_purchase_intent_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'purchase intent commit failure'; END $$; CREATE CONSTRAINT TRIGGER fail_purchase_intent_commit AFTER INSERT ON billing_purchase_intents DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION fail_purchase_intent_commit()`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = db.ExecContext(ctx, `DROP TRIGGER IF EXISTS fail_purchase_intent_commit ON billing_purchase_intents`)
		_, _ = db.ExecContext(ctx, `DROP FUNCTION IF EXISTS fail_purchase_intent_commit()`)
	}()
	newIntent, err := purchase.New(store.Purchases(), testTime).CreateIntent(ctx, purchase.IntentInput{Account: intent.Account, ID: "failed-intent", Operation: "failed-operation", QuoteID: intent.QuoteID, QuoteFingerprint: intent.QuoteFingerprint, Scope: intent.Scope, Actor: "operator", Reason: "failed commit", ExpiresAt: intent.ExpiresAt})
	if err == nil || newIntent != (purchase.Intent{}) {
		t.Fatalf("failed commit intent=%+v err=%v", newIntent, err)
	}
}
