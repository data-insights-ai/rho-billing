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

func TestPostgresPurchaseCommandRecoveryTransitionsAndPaymentRace(t *testing.T) {
	tests := []struct {
		name       string
		dispatched bool
		unknown    bool
		to         purchase.CommandState
	}{
		{name: "planned unknown", to: purchase.CommandUnknown},
		{name: "planned rejected", to: purchase.CommandRejected},
		{name: "planned reconciled", to: purchase.CommandReconciled},
		{name: "dispatched reconciled", dispatched: true, to: purchase.CommandReconciled},
		{name: "unknown unknown", unknown: true, to: purchase.CommandUnknown},
		{name: "unknown rejected", unknown: true, to: purchase.CommandRejected},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			account := billing.AccountID("command-recovery-" + test.name)
			store, db, intent := purchaseLifecycleFixture(t, account, "command-recovery-"+test.name)
			service := purchase.New(store.Purchases(), testTime)
			if test.dispatched {
				var err error
				intent, err = service.RecordCommand(t.Context(), purchase.CommandInput{Account: intent.Account, IntentID: intent.ID, Operation: "dispatch", ExpectedRevision: intent.Revision, State: purchase.CommandDispatched, ProviderReference: "provider-command", OccurredAt: testTime()})
				if err != nil {
					t.Fatal(err)
				}
			}
			if test.unknown {
				var err error
				intent, err = service.RecordCommand(t.Context(), purchase.CommandInput{Account: intent.Account, IntentID: intent.ID, Operation: "initial-unknown", ExpectedRevision: intent.Revision, State: purchase.CommandUnknown, EvidenceReference: "observation-initial", OccurredAt: testTime()})
				if err != nil {
					t.Fatal(err)
				}
			}
			providerReference := ""
			if test.to == purchase.CommandReconciled {
				providerReference = "provider-command"
			}
			in := purchase.CommandInput{Account: intent.Account, IntentID: intent.ID, Operation: "authoritative-recovery", ExpectedRevision: intent.Revision, State: test.to, ProviderReference: providerReference, EvidenceReference: "observation-authoritative", OccurredAt: testTime()}
			got, err := service.RecordCommand(t.Context(), in)
			if err != nil || got.Command != test.to {
				t.Fatalf("intent=%+v err=%v", got, err)
			}
			replay, err := purchase.New(New(db).Purchases(), testTime).RecordCommand(t.Context(), in)
			if err != nil || replay != got {
				t.Fatalf("restart replay=%+v err=%v want=%+v", replay, err, got)
			}
			changed := in
			changed.EvidenceReference = "observation-conflict"
			if out, err := service.RecordCommand(t.Context(), changed); !errors.Is(err, billing.ErrConflict) || out != (purchase.Intent{}) {
				t.Fatalf("changed replay=%+v err=%v", out, err)
			}
		})
	}

	store, _, intent := purchaseLifecycleFixture(t, "command-payment-race", "command-payment-race")
	service := purchase.New(store.Purchases(), testTime)
	fact := purchase.PaymentFact{Account: intent.Account, Scope: intent.Scope, EventID: "action-required", TransactionID: "provider-transaction", IntentID: intent.ID, Status: purchase.FactActionRequired, Currency: intent.Currency, OccurredAt: testTime()}
	if result, err := service.ApplyPayment(t.Context(), fact); err != nil || !result.Applied {
		t.Fatalf("payment=%+v err=%v", result, err)
	}
	current, err := service.Intent(t.Context(), intent.Account, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := service.RecordCommand(t.Context(), purchase.CommandInput{Account: current.Account, IntentID: current.ID, Operation: "recover-after-payment", ExpectedRevision: current.Revision, State: purchase.CommandReconciled, ProviderReference: "provider-command", EvidenceReference: "observation-payment-race", OccurredAt: testTime().Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Payment != purchase.PaymentActionRequired || recovered.Fulfillment != purchase.FulfillmentPending || recovered.LastPaymentEventID != fact.EventID || !recovered.LastPaymentAt.Equal(fact.OccurredAt) {
		t.Fatalf("recovery changed payment fields: %+v", recovered)
	}
}

func TestPostgresPurchaseCommandRecoveryRollbackAndDeferredCommit(t *testing.T) {
	store, db, intent := purchaseLifecycleFixture(t, "command-recovery-rollback", "command-recovery-rollback")
	ctx := t.Context()
	sentinel := errors.New("rollback command recovery")
	err := store.Atomic(ctx, intent.Account, func(scope integration.Session) error {
		_, err := purchase.New(scope.(*session).Purchases(), testTime).RecordCommand(ctx, purchase.CommandInput{Account: intent.Account, IntentID: intent.ID, Operation: "rollback-recovery", ExpectedRevision: intent.Revision, State: purchase.CommandUnknown, EvidenceReference: "observation-rollback", OccurredAt: testTime()})
		if err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("rollback error=%v", err)
	}
	assertCommandRecoveryUnchanged(t, store, db, intent)

	if _, err := db.ExecContext(ctx, `CREATE FUNCTION fail_purchase_command_recovery_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'command recovery commit failure'; END $$; CREATE CONSTRAINT TRIGGER fail_purchase_command_recovery_commit AFTER INSERT ON billing_purchase_commands DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION fail_purchase_command_recovery_commit()`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = db.ExecContext(ctx, `DROP TRIGGER IF EXISTS fail_purchase_command_recovery_commit ON billing_purchase_commands`)
		_, _ = db.ExecContext(ctx, `DROP FUNCTION IF EXISTS fail_purchase_command_recovery_commit()`)
	}()
	service := purchase.New(store.Purchases(), testTime)
	out, err := service.RecordCommand(ctx, purchase.CommandInput{Account: intent.Account, IntentID: intent.ID, Operation: "failed-commit-recovery", ExpectedRevision: intent.Revision, State: purchase.CommandRejected, EvidenceReference: "observation-failed-commit", OccurredAt: testTime()})
	if err == nil || out != (purchase.Intent{}) {
		t.Fatalf("failed commit out=%+v err=%v", out, err)
	}
	assertCommandRecoveryUnchanged(t, store, db, intent)
}

func assertCommandRecoveryUnchanged(t *testing.T, store *Store, db *sql.DB, intent purchase.Intent) {
	t.Helper()
	current, err := purchase.New(store.Purchases(), testTime).Intent(t.Context(), intent.Account, intent.ID)
	if err != nil || current.Command != purchase.CommandPlanned || current.Revision != intent.Revision || current.Payment != intent.Payment || current.Fulfillment != intent.Fulfillment {
		t.Fatalf("current=%+v err=%v", current, err)
	}
	var commands int
	if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM billing_purchase_commands WHERE account_id=$1`, intent.Account).Scan(&commands); err != nil {
		t.Fatal(err)
	}
	if commands != 0 {
		t.Fatalf("commands=%d, want zero", commands)
	}
}
