package pg

import (
	"errors"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/integration"
	"github.com/data-insights-ai/rho-billing/purchase"
)

func TestPostgresDisputeStatusAndPartialRecoveriesAreIndependent(t *testing.T) {
	store, _, intent, _, debit := paidAdjustmentFixture(t, "dispute-recovery")
	ctx := t.Context()
	now := testTime().Add(time.Minute)
	svc := purchase.New(store.Purchases(), func() time.Time { return now })
	fact := purchase.DisputeFact{Account: intent.Account, Scope: intent.Scope, EventID: "warning", DisputeID: "case", IntentID: intent.ID, TransactionID: debit.TransactionID, Currency: "USD", Amount: 200, Status: purchase.DisputeWarning, OccurredAt: now, EvidenceReference: "provider-warning"}
	if out, err := svc.ApplyDispute(ctx, fact); err != nil || !out.Applied || !out.StatusApplied {
		t.Fatalf("warning: %+v %v", out, err)
	}
	bal, err := credit.New(store, func() time.Time { return now }).Balance(ctx, intent.Account, "ai", "")
	if err != nil || bal.Available != 10000 {
		t.Fatalf("status alone revoked credits: %+v %v", bal, err)
	}
	now = testTime().Add(2 * time.Minute)
	debit.Kind = purchase.AdjustmentChargeback
	debit.OccurredAt = now
	err = store.Atomic(ctx, intent.Account, func(session integration.Session) error {
		bound := purchase.New(session.Purchases(), func() time.Time { return now })
		if out, err := bound.ApplyAdjustment(ctx, debit); err != nil || !out.Applied {
			t.Fatalf("debit: %+v %v", out, err)
		}
		fact.EventID = "lost"
		fact.Status = purchase.DisputeLost
		fact.OccurredAt = now
		fact.DebitAdjustmentID = debit.ID
		out, err := bound.ApplyDispute(ctx, fact)
		if err == nil && !out.Applied {
			t.Fatalf("case notapplied: %+v", out)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	now = testTime().Add(4 * time.Minute)
	fact.EventID = "won"
	fact.Status = purchase.DisputeWon
	fact.OccurredAt = now
	if out, err := svc.ApplyDispute(ctx, fact); err != nil || !out.Applied {
		t.Fatalf("won: %+v %v", out, err)
	}
	state, err := svc.Dispute(ctx, intent.Account, intent.Scope, "case")
	if err != nil || len(state.Recovered) != 0 {
		t.Fatalf("won inferred money: %+v %v", state, err)
	}
	// Older case status must not regress Won, but the separately verified recovery
	// remains financial evidence and must not disappear merely because it is late.
	fact.EventID = "late-recovery"
	fact.Status = purchase.DisputeUnderReview
	fact.OccurredAt = testTime().Add(3 * time.Minute)
	fact.Recovery = &purchase.DisputeRecovery{ID: "recovery-1", AdjustmentID: debit.ID, Lines: []purchase.PaidLine{{LineID: debit.Lines[0].LineID, Gross: 25}}, EvidenceReference: "provider-recovery-1"}
	out, err := svc.ApplyDispute(ctx, fact)
	if err != nil || !out.Applied || out.StatusApplied || !out.RecoveryApplied {
		t.Fatalf("late recovery: %+v %v", out, err)
	}
	state, err = svc.Dispute(ctx, intent.Account, intent.Scope, "case")
	if err != nil || state.Status != purchase.DisputeWon || len(state.Recovered) != 1 || state.Recovered[0].Gross != 25 {
		t.Fatalf("late projection: %+v %v", state, err)
	}
	now = testTime().Add(5 * time.Minute)
	fact.EventID = "recovery-rest"
	fact.Status = purchase.DisputeWon
	fact.OccurredAt = now
	fact.Recovery = &purchase.DisputeRecovery{ID: "recovery-2", AdjustmentID: debit.ID, Lines: []purchase.PaidLine{{LineID: debit.Lines[0].LineID, Gross: 75}}, EvidenceReference: "provider-recovery-2"}
	if out, err := svc.ApplyDispute(ctx, fact); err != nil || !out.RecoveryApplied {
		t.Fatalf("second partial: %+v %v", out, err)
	}
	fact.EventID = "recovery-redelivery"
	if out, err := svc.ApplyDispute(ctx, fact); err != nil || !out.Applied || out.RecoveryApplied {
		t.Fatalf("repeated recovery reapplied: %+v %v", out, err)
	}
	before, err := svc.Dispute(ctx, intent.Account, intent.Scope, "case")
	if err != nil || len(before.Recovered) != 1 || before.Recovered[0].Gross != 100 {
		t.Fatalf("recovery totals: %+v %v", before, err)
	}
	fact.EventID = "over-recovery"
	fact.Recovery = &purchase.DisputeRecovery{ID: "recovery-3", AdjustmentID: debit.ID, Lines: []purchase.PaidLine{{LineID: debit.Lines[0].LineID, Gross: 1}}, EvidenceReference: "provider-recovery-3"}
	if out, err := svc.ApplyDispute(ctx, fact); err == nil || out.Account != "" {
		t.Fatalf("over-recovery accepted: %+v %v", out, err)
	}
	after, err := svc.Dispute(ctx, intent.Account, intent.Scope, "case")
	if err != nil || after.Fingerprint() != before.Fingerprint() {
		t.Fatalf("rejection changed case: %+v %v", after, err)
	}
	bal, err = credit.New(store, func() time.Time { return now }).Balance(ctx, intent.Account, "ai", "")
	if err != nil || bal.Available != 5000 || bal.Revoked != 5000 {
		t.Fatalf("money recovery restored credits: %+v %v", bal, err)
	}
}
func TestPostgresDisputeRecoveryCanBeFirstCaseObservation(t *testing.T) {
	store, _, intent, _, debit := paidAdjustmentFixture(t, "recovery-first")
	ctx := t.Context()
	now := testTime().Add(time.Minute)
	svc := purchase.New(store.Purchases(), func() time.Time { return now })
	debit.Kind = purchase.AdjustmentChargeback
	debit.OccurredAt = now
	fact := purchase.DisputeFact{Account: intent.Account, Scope: intent.Scope, EventID: "first-recovery", DisputeID: "case", IntentID: intent.ID, TransactionID: debit.TransactionID, Currency: "USD", Amount: 200, Status: purchase.DisputeWon, OccurredAt: now, EvidenceReference: "case-evidence", Recovery: &purchase.DisputeRecovery{ID: "first-recovery", AdjustmentID: debit.ID, Lines: debit.Lines, EvidenceReference: "provider-recovery"}}
	if out, err := svc.ApplyDispute(ctx, fact); !errors.Is(err, billing.ErrNotFound) || out.Account != "" {
		t.Fatalf("missing debit: %+v %v", out, err)
	}
	if out, err := svc.ApplyAdjustment(ctx, debit); err != nil || !out.Applied {
		t.Fatalf("debit: %+v %v", out, err)
	}
	if out, err := svc.ApplyDispute(ctx, fact); err != nil || !out.RecoveryApplied {
		t.Fatalf("first case recovery: %+v %v", out, err)
	}
	state, err := svc.Dispute(ctx, intent.Account, intent.Scope, "case")
	if err != nil || state.DebitAdjustmentID != debit.ID || len(state.Recovered) != 1 || state.Recovered[0].Gross != 100 {
		t.Fatalf("recovery first projection: %+v %v", state, err)
	}
}
func TestPostgresDisputeFailedCommitRollsBackCaseRecoveryAndBoundDebit(t *testing.T) {
	store, db, intent, _, debit := paidAdjustmentFixture(t, "dispute-commit")
	ctx := t.Context()
	now := testTime().Add(time.Minute)
	debit.Kind = purchase.AdjustmentChargeback
	debit.OccurredAt = now
	svc := purchase.New(store.Purchases(), func() time.Time { return now })
	fact := purchase.DisputeFact{Account: intent.Account, Scope: intent.Scope, EventID: "debit-case", DisputeID: "case", IntentID: intent.ID, TransactionID: debit.TransactionID, Currency: "USD", Amount: 200, Status: purchase.DisputeLost, OccurredAt: now, EvidenceReference: "case-evidence", DebitAdjustmentID: debit.ID}
	if _, err := db.ExecContext(ctx, `CREATE FUNCTION fail_dispute_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'dispute commit failure'; END $$; CREATE CONSTRAINT TRIGGER fail_dispute_commit AFTER INSERT ON billing_purchase_dispute_events DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION fail_dispute_commit()`); err != nil {
		t.Fatal(err)
	}
	err := store.Atomic(ctx, intent.Account, func(session integration.Session) error {
		bound := purchase.New(session.Purchases(), func() time.Time { return now })
		if out, err := bound.ApplyAdjustment(ctx, debit); err != nil || !out.Applied {
			t.Fatalf("bound debit: %+v %v", out, err)
		}
		_, err := bound.ApplyDispute(ctx, fact)
		return err
	})
	if err == nil {
		t.Fatal("failed host commit returned success")
	}
	if _, err := svc.Adjustment(ctx, intent.Account, debit.ID); !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("debit escaped rollback: %v", err)
	}
	balance, err := credit.New(store, func() time.Time { return now }).Balance(ctx, intent.Account, "ai", "")
	if err != nil || balance.Available != 10000 {
		t.Fatalf("credit effect escaped rollback: %+v %v", balance, err)
	}
	if _, err := svc.Dispute(ctx, intent.Account, intent.Scope, "case"); !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("case escaped rollback: %v", err)
	}
	// Keep the failure active while testing a root case call with already committed
	// debit evidence. Root success-shaped results must not escape its failed commit.
	if out, err := svc.ApplyAdjustment(ctx, debit); err != nil || !out.Applied {
		t.Fatalf("standalone debit: %+v %v", out, err)
	}
	fact.Recovery = &purchase.DisputeRecovery{ID: "recovery", AdjustmentID: debit.ID, Lines: debit.Lines, EvidenceReference: "recovery-evidence"}
	if out, err := svc.ApplyDispute(ctx, fact); err == nil || out.Account != "" {
		t.Fatalf("root commit leaked result: %+v %v", out, err)
	}
	if _, err := db.ExecContext(ctx, `DROP TRIGGER fail_dispute_commit ON billing_purchase_dispute_events; DROP FUNCTION fail_dispute_commit()`); err != nil {
		t.Fatal(err)
	}
	if out, err := svc.ApplyDispute(ctx, fact); err != nil || !out.Applied || !out.RecoveryApplied {
		t.Fatalf("recovery after lost commit: %+v %v", out, err)
	}
}
