package pg

import (
	"errors"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/purchase"
)

func TestPostgresPurchaseIntentRejectsChangedAndExpiredQuoteCreation(t *testing.T) {
	store, _, intent := purchaseLifecycleFixture(t, "purchase-acceptance-quote", "quote")
	ctx := t.Context()
	service := purchase.New(store.Purchases(), testTime)

	changed := intent.IntentInput
	changed.ID = "changed-quote-intent"
	changed.Operation = "changed-quote-operation"
	changedFingerprint := []byte(intent.QuoteFingerprint)
	if changedFingerprint[0] == '0' {
		changedFingerprint[0] = '1'
	} else {
		changedFingerprint[0] = '0'
	}
	changed.QuoteFingerprint = string(changedFingerprint)
	if got, err := service.CreateIntent(ctx, changed); !errors.Is(err, billing.ErrConflict) || got != (purchase.Intent{}) {
		t.Fatalf("changed quote result=%+v err=%v, want conflict and zero result", got, err)
	}
	if _, err := service.Intent(ctx, intent.Account, changed.ID); !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("changed quote created intent: %v", err)
	}

	expired := intent.IntentInput
	expired.ID = "expired-quote-intent"
	expired.Operation = "expired-quote-operation"
	expired.ExpiresAt = testTime().Add(2 * time.Hour)
	service = purchase.New(store.Purchases(), func() time.Time { return testTime().Add(time.Hour) })
	if got, err := service.CreateIntent(ctx, expired); !errors.Is(err, billing.ErrExpired) || got != (purchase.Intent{}) {
		t.Fatalf("expired quote result=%+v err=%v, want expired and zero result", got, err)
	}
	if _, err := service.Intent(ctx, intent.Account, expired.ID); !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("expired quote created intent: %v", err)
	}
	if original, err := service.Intent(ctx, intent.Account, intent.ID); err != nil || original != intent {
		t.Fatalf("rejected creation changed original intent: %+v error=%v", original, err)
	}
	if err := store.Purchases().WithinAccount(ctx, intent.Account, func(tx purchase.Tx) error {
		quote, err := tx.Quote(ctx, intent.QuoteID)
		if err != nil {
			return err
		}
		if quote.Fingerprint() != intent.QuoteFingerprint {
			return errors.New("rejected creation changed approved quote")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresPurchaseActionRequiredAndUnknownSurviveStoreRestart(t *testing.T) {
	store, db, intent := purchaseLifecycleFixture(t, "purchase-acceptance-restart", "restart")
	ctx := t.Context()
	service := purchase.New(store.Purchases(), testTime)
	fact := purchase.PaymentFact{
		Account: intent.Account, Scope: intent.Scope, EventID: "action-required-event",
		TransactionID: "action-required-transaction", IntentID: intent.ID,
		Status: purchase.FactActionRequired, Currency: intent.Currency,
		OccurredAt: testTime().Add(time.Minute),
	}
	if got, err := service.ApplyPayment(ctx, fact); err != nil || !got.Applied || got.Rejection != "" {
		t.Fatalf("action-required result=%+v err=%v", got, err)
	}

	actionRequired, err := service.Intent(ctx, intent.Account, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	command, err := service.RecordCommand(ctx, purchase.CommandInput{
		Account: intent.Account, IntentID: intent.ID, Operation: "restart-dispatch",
		ExpectedRevision: actionRequired.Revision, State: purchase.CommandDispatched,
		ProviderReference: "restart-provider-command", OccurredAt: testTime().Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if command.Command != purchase.CommandDispatched {
		t.Fatalf("dispatched command=%+v", command)
	}
	if _, err := service.RecordCommand(ctx, purchase.CommandInput{
		Account: intent.Account, IntentID: intent.ID, Operation: "restart-unknown",
		ExpectedRevision: command.Revision, State: purchase.CommandUnknown,
		ProviderReference: "restart-unknown-provider", OccurredAt: testTime().Add(3 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	fresh := purchase.New(New(db).Purchases(), testTime)
	stored, err := fresh.Intent(ctx, intent.Account, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Command != purchase.CommandUnknown || stored.Payment != purchase.PaymentActionRequired || stored.Fulfillment != purchase.FulfillmentPending {
		t.Fatalf("restarted lifecycle=%+v, want unknown/action_required/pending", stored)
	}
	if replay, err := fresh.ApplyPayment(ctx, fact); err != nil || !replay.Applied {
		t.Fatalf("restarted action-required replay=%+v error=%v", replay, err)
	}
	if after, err := fresh.Intent(ctx, intent.Account, intent.ID); err != nil || after != stored {
		t.Fatalf("restarted replay changed projection: %+v error=%v", after, err)
	}
}
