package pg

import (
	"testing"
	"time"

	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/purchase"
)

func TestPostgresRecoveredChargebackEffectsAdvanceOnlyPastPriorPeak(t *testing.T) {
	store, _, intent, _, debit := paidAdjustmentFixture(t, "recovery-effect-peak")
	ctx := t.Context()
	now := testTime().Add(4 * time.Minute)
	service := purchase.New(store.Purchases(), func() time.Time { return now })
	debit.Kind = purchase.AdjustmentChargeback
	debit.ID = "chargeback-effect-first"
	debit.ProviderAdjustmentID = "provider-chargeback-effect-first"
	debit.OccurredAt = testTime().Add(time.Minute)
	first, err := service.ApplyAdjustment(ctx, debit)
	if err != nil || !first.Applied || len(first.Effects) != 1 || first.Effects[0].TargetDelta != 5000 {
		t.Fatalf("first chargeback=%+v err=%v", first, err)
	}
	assertRecoveryEffectBalance(t, store, intent, now, 5000)

	recovery := purchase.DisputeFact{
		Account: intent.Account, Scope: intent.Scope, EventID: "chargeback-effect-recovered",
		DisputeID: "chargeback-effect-case", IntentID: intent.ID, TransactionID: debit.TransactionID,
		Currency: debit.Currency, Amount: 100, Status: purchase.DisputeWon,
		OccurredAt: testTime().Add(2 * time.Minute), EvidenceReference: "chargeback-effect-recovery-evidence",
		DebitAdjustmentID: debit.ID,
		Recovery: &purchase.DisputeRecovery{
			ID: "chargeback-effect-recovery", AdjustmentID: debit.ID,
			EvidenceReference: "chargeback-effect-recovery-evidence", Lines: debit.Lines,
		},
	}
	if result, err := service.ApplyDispute(ctx, recovery); err != nil || !result.RecoveryApplied {
		t.Fatalf("recovery=%+v err=%v", result, err)
	}
	assertRecoveryEffectBalance(t, store, intent, now, 5000)

	secondDebit := debit
	secondDebit.ID = "chargeback-effect-second"
	secondDebit.ProviderAdjustmentID = "provider-chargeback-effect-second"
	secondDebit.OccurredAt = testTime().Add(3 * time.Minute)
	second, err := service.ApplyAdjustment(ctx, secondDebit)
	if err != nil || !second.Applied || len(second.Effects) != 0 {
		t.Fatalf("equal outstanding chargeback=%+v err=%v", second, err)
	}
	assertRecoveryEffectBalance(t, store, intent, now, 5000)

	thirdDebit := debit
	thirdDebit.ID = "chargeback-effect-third"
	thirdDebit.ProviderAdjustmentID = "provider-chargeback-effect-third"
	thirdDebit.Lines = []purchase.PaidLine{{LineID: debit.Lines[0].LineID, Gross: 50}}
	thirdDebit.OccurredAt = now
	third, err := service.ApplyAdjustment(ctx, thirdDebit)
	if err != nil || !third.Applied || len(third.Effects) != 1 || third.Effects[0].TargetDelta != 2500 {
		t.Fatalf("higher outstanding chargeback=%+v err=%v", third, err)
	}
	assertRecoveryEffectBalance(t, store, intent, now, 2500)
}

func assertRecoveryEffectBalance(t *testing.T, store *Store, intent purchase.Intent, now time.Time, available int64) {
	t.Helper()
	balance, err := credit.New(store, func() time.Time { return now }).Balance(t.Context(), intent.Account, "ai", "")
	if err != nil || balance.Available != available {
		t.Fatalf("balance=%+v err=%v, want available=%d", balance, err, available)
	}
}
