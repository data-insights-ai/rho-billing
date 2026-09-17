package pg

import (
	"errors"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/purchase"
)

func TestPostgresRecoveredChargebackAndOutOfOrderNewDebit(t *testing.T) {
	store, _, intent, _, debit := paidAdjustmentFixture(t, "recovered-exposure")
	now := testTime().Add(time.Minute)
	service := purchase.New(store.Purchases(), func() time.Time { return now })
	debit.Kind = purchase.AdjustmentChargeback
	debit.Lines[0].Gross = 200
	debit.OccurredAt = now
	first, err := service.ApplyAdjustment(t.Context(), debit)
	if err != nil || !first.Applied {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	newDebit := debit
	newDebit.ID = "later-debit"
	newDebit.ProviderAdjustmentID = "provider-later-debit"
	if result, err := service.ApplyAdjustment(t.Context(), newDebit); !errors.Is(err, billing.ErrConflict) || result.Applied {
		t.Fatalf("early new debit=%+v err=%v", result, err)
	}
	recovery := purchase.DisputeFact{Account: intent.Account, Scope: intent.Scope, EventID: "recovery-event", DisputeID: "case", IntentID: intent.ID, TransactionID: debit.TransactionID, Currency: "USD", Amount: 200, Status: purchase.DisputeWon, OccurredAt: now, DebitAdjustmentID: debit.ID, EvidenceReference: "evidence", Recovery: &purchase.DisputeRecovery{ID: "recovery", AdjustmentID: debit.ID, Lines: debit.Lines, EvidenceReference: "evidence"}}
	for range 2 {
		if result, err := service.ApplyDispute(t.Context(), recovery); err != nil || !result.Applied {
			t.Fatalf("recovery=%+v err=%v", result, err)
		}
	}
	result, err := service.ApplyAdjustment(t.Context(), newDebit)
	if err != nil || !result.Applied || len(result.Effects) != 0 {
		t.Fatalf("new debit=%+v err=%v", result, err)
	}
	if err := store.Purchases().WithinAccount(t.Context(), intent.Account, func(tx purchase.Tx) error {
		state, err := tx.AdjustmentState(t.Context(), intent.ID)
		if err != nil {
			return err
		}
		totals, err := tx.ChargebackRecoveries(t.Context(), intent.ID)
		if err != nil {
			return err
		}
		if len(state.Lines) != 1 || state.Lines[0].ChargebackGross != 400 || len(totals) != 1 || totals[0].Gross != 200 {
			t.Fatalf("state=%+v totals=%+v", state, totals)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	balance, err := credit.New(store, func() time.Time { return now }).Balance(t.Context(), intent.Account, "ai", "")
	if err != nil || balance.Available != 0 {
		t.Fatalf("benefits=%+v err=%v", balance, err)
	}
}

func TestPostgresRecoveryProjectionRollsBackAtDeferredCommit(t *testing.T) {
	store, db, intent, _, debit := paidAdjustmentFixture(t, "recovery-commit")
	now := testTime().Add(time.Minute)
	service := purchase.New(store.Purchases(), func() time.Time { return now })
	debit.Kind = purchase.AdjustmentChargeback
	debit.OccurredAt = now
	if result, err := service.ApplyAdjustment(t.Context(), debit); err != nil || !result.Applied {
		t.Fatalf("debit=%+v err=%v", result, err)
	}
	if _, err := db.ExecContext(t.Context(), `CREATE FUNCTION fail_recovery_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected recovery commit failure'; END $$; CREATE CONSTRAINT TRIGGER fail_recovery_commit AFTER INSERT ON billing_purchase_dispute_events DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION fail_recovery_commit()`); err != nil {
		t.Fatal(err)
	}
	fact := purchase.DisputeFact{Account: intent.Account, Scope: intent.Scope, EventID: "commit-recovery", DisputeID: "commit-case", IntentID: intent.ID, TransactionID: debit.TransactionID, Currency: "USD", Amount: 100, Status: purchase.DisputeWon, OccurredAt: now, DebitAdjustmentID: debit.ID, EvidenceReference: "evidence", Recovery: &purchase.DisputeRecovery{ID: "commit-recovery", AdjustmentID: debit.ID, Lines: debit.Lines, EvidenceReference: "evidence"}}
	if result, err := service.ApplyDispute(t.Context(), fact); err == nil || result.Applied {
		t.Fatalf("commit failure=%+v err=%v", result, err)
	}
	if err := store.Purchases().WithinAccount(t.Context(), intent.Account, func(tx purchase.Tx) error {
		totals, err := tx.ChargebackRecoveries(t.Context(), intent.ID)
		if len(totals) != 0 {
			t.Fatalf("partial projection survived: %+v", totals)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `DROP TRIGGER fail_recovery_commit ON billing_purchase_dispute_events; DROP FUNCTION fail_recovery_commit()`); err != nil {
		t.Fatal(err)
	}
	if result, err := service.ApplyDispute(t.Context(), fact); err != nil || !result.RecoveryApplied {
		t.Fatalf("retry=%+v err=%v", result, err)
	}
	if err := store.Purchases().WithinAccount(t.Context(), intent.Account, func(tx purchase.Tx) error {
		totals, err := tx.ChargebackRecoveries(t.Context(), intent.ID)
		if len(totals) != 1 || totals[0].Gross != 100 {
			t.Fatalf("retry projection=%+v", totals)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
