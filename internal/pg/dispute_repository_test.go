package pg

import (
	"errors"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/purchase"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestPostgresDisputeRepositoryReplayAndScopedOwnership(t *testing.T) {
	store, _, intent, service, adjustment := paidAdjustmentFixture(t, "dispute-repository")
	ctx := t.Context()
	adjustment.Kind = purchase.AdjustmentChargeback
	adjustment.ID = "chargeback-repository"
	adjustment.ProviderAdjustmentID = "provider-chargeback-repository"
	if result, err := service.ApplyAdjustment(ctx, adjustment); err != nil || !result.Applied {
		t.Fatalf("chargeback=%+v err=%v", result, err)
	}
	fact := purchase.DisputeFact{Account: intent.Account, Scope: intent.Scope, EventID: "dispute-event-repository", DisputeID: "dispute-repository", IntentID: intent.ID, TransactionID: adjustment.TransactionID, Currency: "USD", Amount: 100, Status: purchase.DisputeWarning, OccurredAt: testTime(), EvidenceReference: "provider-evidence", DebitAdjustmentID: adjustment.ID}
	first, err := service.ApplyDispute(ctx, fact)
	if err != nil || !first.Applied {
		t.Fatalf("dispute=%+v err=%v", first, err)
	}
	replay, err := service.ApplyDispute(ctx, fact)
	if err != nil || replay != first {
		t.Fatalf("replay=%+v err=%v first=%+v", replay, err, first)
	}
	got, err := service.Dispute(ctx, intent.Account, intent.Scope, fact.DisputeID)
	if err != nil || got.ID != fact.DisputeID || got.DebitAdjustmentID != adjustment.ID {
		t.Fatalf("stored=%+v err=%v", got, err)
	}
	other := billing.AccountID("dispute-repository-other")
	if err := store.CreateAccount(ctx, other, "dispute-repository-other-subject"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Dispute(ctx, other, intent.Scope, fact.DisputeID); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("foreign read=%v", err)
	}
}

func TestPostgresDisputeRepositoryRejectsImmutableDebitReplacement(t *testing.T) {
	store, _, intent, service, debit := paidAdjustmentFixture(t, "dispute-repository-cas")
	ctx := t.Context()
	debit.Kind = purchase.AdjustmentChargeback
	debit.ID = "chargeback-repository-cas"
	debit.ProviderAdjustmentID = "provider-chargeback-repository-cas"
	if out, err := service.ApplyAdjustment(ctx, debit); err != nil || !out.Applied {
		t.Fatalf("chargeback=%+v err=%v", out, err)
	}
	fact := purchase.DisputeFact{Account: intent.Account, Scope: intent.Scope, EventID: "event-repository-cas", DisputeID: "dispute-repository-cas", IntentID: intent.ID, TransactionID: debit.TransactionID, Currency: "USD", Amount: 100, Status: purchase.DisputeWarning, OccurredAt: testTime(), EvidenceReference: "evidence-cas", DebitAdjustmentID: debit.ID}
	if out, err := service.ApplyDispute(ctx, fact); err != nil || !out.Applied {
		t.Fatalf("dispute=%+v err=%v", out, err)
	}
	err := store.Purchases().WithinAccount(ctx, intent.Account, func(tx purchase.Tx) error {
		got, err := tx.Dispute(ctx, intent.Scope, fact.DisputeID)
		if err != nil {
			return err
		}
		got.Revision++
		got.DebitAdjustmentID = "replacement-debit"
		return tx.SaveDispute(ctx, got, got.Revision-1)
	})
	if !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("debit replacement error=%v, want conflict", err)
	}
}

func TestPostgresDisputeRepositoryRejectsJSONIdentityCorruption(t *testing.T) {
	store, db, intent, service, debit := paidAdjustmentFixture(t, "dispute-repository-integrity")
	ctx := t.Context()
	debit.Kind = purchase.AdjustmentChargeback
	debit.ID = "chargeback-repository-integrity"
	debit.ProviderAdjustmentID = "provider-chargeback-repository-integrity"
	if out, err := service.ApplyAdjustment(ctx, debit); err != nil || !out.Applied {
		t.Fatalf("chargeback=%+v err=%v", out, err)
	}
	fact := purchase.DisputeFact{Account: intent.Account, Scope: intent.Scope, EventID: "event-repository-integrity", DisputeID: "dispute-repository-integrity", IntentID: intent.ID, TransactionID: debit.TransactionID, Currency: "USD", Amount: 100, Status: purchase.DisputeWarning, OccurredAt: testTime(), EvidenceReference: "evidence-integrity", DebitAdjustmentID: debit.ID}
	if out, err := service.ApplyDispute(ctx, fact); err != nil || !out.Applied {
		t.Fatalf("dispute=%+v err=%v", out, err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE billing_purchase_dispute_events SET fact=jsonb_set(fact, '{EventID}', '"tampered-event"'::jsonb) WHERE account_id=$1 AND event_id=$2`, intent.Account, fact.EventID); err != nil {
		t.Fatal(err)
	}
	err := store.Purchases().WithinAccount(ctx, intent.Account, func(tx purchase.Tx) error {
		_, err := tx.DisputeEvent(ctx, fact.Scope, fact.EventID)
		return err
	})
	if !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("event JSON identity error=%v, want conflict", err)
	}

	// A recovery JSON ID must agree with the indexed identity as well; otherwise
	// a repaired row could silently change which provider recovery was applied.
	fact.EventID = "event-repository-recovery"
	fact.Status = purchase.DisputeLost
	fact.OccurredAt = testTime().Add(time.Minute)
	fact.Recovery = &purchase.DisputeRecovery{ID: "recovery-repository-integrity", AdjustmentID: debit.ID, Lines: []purchase.PaidLine{{LineID: debit.Lines[0].LineID, Gross: 25}}, EvidenceReference: "recovery-integrity"}
	service = purchase.New(store.Purchases(), func() time.Time { return testTime().Add(2 * time.Minute) })
	if out, err := service.ApplyDispute(ctx, fact); err != nil || !out.RecoveryApplied {
		t.Fatalf("recovery=%+v err=%v", out, err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE billing_purchase_dispute_recoveries SET recovery=jsonb_set(recovery, '{ID}', '"tampered-recovery"'::jsonb) WHERE account_id=$1 AND recovery_id=$2`, intent.Account, fact.Recovery.ID); err != nil {
		t.Fatal(err)
	}
	err = store.Purchases().WithinAccount(ctx, intent.Account, func(tx purchase.Tx) error {
		_, err := tx.DisputeRecovery(ctx, fact.Scope, fact.Recovery.ID)
		return err
	})
	if !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("recovery JSON identity error=%v, want conflict", err)
	}
}

func TestPostgresDisputeRepositoryRejectsRecoveryForWrongDebit(t *testing.T) {
	store, _, intent, service, debit := paidAdjustmentFixture(t, "dispute-repository-fk")
	ctx := t.Context()
	debit.Kind = purchase.AdjustmentChargeback
	debit.ID = "chargeback-repository-fk"
	debit.ProviderAdjustmentID = "provider-chargeback-repository-fk"
	if out, err := service.ApplyAdjustment(ctx, debit); err != nil || !out.Applied {
		t.Fatalf("chargeback=%+v err=%v", out, err)
	}
	otherDebit := debit
	otherDebit.ID = "chargeback-repository-fk-other"
	otherDebit.ProviderAdjustmentID = "provider-chargeback-repository-fk-other"
	if out, err := service.ApplyAdjustment(ctx, otherDebit); err != nil || !out.Applied {
		t.Fatalf("other chargeback=%+v err=%v", out, err)
	}
	fact := purchase.DisputeFact{Account: intent.Account, Scope: intent.Scope, EventID: "event-repository-fk", DisputeID: "dispute-repository-fk", IntentID: intent.ID, TransactionID: debit.TransactionID, Currency: "USD", Amount: 100, Status: purchase.DisputeLost, OccurredAt: testTime(), EvidenceReference: "evidence-fk", DebitAdjustmentID: debit.ID}
	if out, err := service.ApplyDispute(ctx, fact); err != nil || !out.Applied {
		t.Fatalf("dispute=%+v err=%v", out, err)
	}
	wrong := purchase.DisputeRecoveryRecord{Account: intent.Account, Scope: intent.Scope, DisputeID: fact.DisputeID, IntentID: fact.IntentID, TransactionID: fact.TransactionID, CreatedAt: testTime(), Recovery: purchase.DisputeRecovery{ID: "recovery-repository-fk", AdjustmentID: otherDebit.ID, Lines: []purchase.PaidLine{{LineID: debit.Lines[0].LineID, Gross: 1}}, EvidenceReference: "wrong-debit"}}
	err := store.Purchases().WithinAccount(ctx, intent.Account, func(tx purchase.Tx) error {
		return tx.InsertDisputeRecovery(ctx, wrong)
	})
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); !ok || pgErr.Code != "23503" {
		t.Fatalf("wrong debit error=%v, want composite foreign-key violation", err)
	}
}
