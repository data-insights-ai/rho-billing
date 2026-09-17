package pg

import (
	"errors"
	"sync"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/purchase"
)

func TestPostgresDisputeConcurrentPartialRecoveriesRemainBounded(t *testing.T) {
	store, db, intent, _, debit := paidAdjustmentFixture(t, "dispute-concurrent-recovery")
	ctx := t.Context()
	now := testTime().Add(time.Minute)
	debit.Kind = purchase.AdjustmentChargeback
	debit.OccurredAt = now
	first := purchase.New(store.Purchases(), func() time.Time { return now })
	if out, err := first.ApplyAdjustment(ctx, debit); err != nil || !out.Applied {
		t.Fatalf("debit: %+v %v", out, err)
	}
	second := purchase.New(secondStore(t, db).Purchases(), func() time.Time { return now })

	base := purchase.DisputeFact{
		Account: intent.Account, Scope: intent.Scope, DisputeID: "concurrent-dispute",
		IntentID: intent.ID, TransactionID: debit.TransactionID, Currency: "USD",
		Amount: 200, Status: purchase.DisputeWon, OccurredAt: now,
		DebitAdjustmentID: debit.ID, EvidenceReference: "dispute-evidence",
	}
	left := base
	left.EventID = "dispute-recovery-left"
	left.Recovery = &purchase.DisputeRecovery{
		ID: "recovery-left", AdjustmentID: debit.ID,
		Lines:             []purchase.PaidLine{{LineID: debit.Lines[0].LineID, Gross: 60}},
		EvidenceReference: "recovery-evidence-left",
	}
	right := base
	right.EventID = "dispute-recovery-right"
	right.Recovery = &purchase.DisputeRecovery{
		ID: "recovery-right", AdjustmentID: debit.ID,
		Lines:             []purchase.PaidLine{{LineID: debit.Lines[0].LineID, Gross: 60}},
		EvidenceReference: "recovery-evidence-right",
	}

	start := make(chan struct{})
	results := make(chan struct {
		result purchase.DisputeResult
		err    error
	}, 2)
	var wg sync.WaitGroup
	wg.Go(func() {
		<-start
		out, err := first.ApplyDispute(ctx, left)
		results <- struct {
			result purchase.DisputeResult
			err    error
		}{out, err}
	})
	wg.Go(func() {
		<-start
		out, err := second.ApplyDispute(ctx, right)
		results <- struct {
			result purchase.DisputeResult
			err    error
		}{out, err}
	})
	close(start)
	wg.Wait()
	close(results)

	var applied int
	var rejected int
	for got := range results {
		if got.err != nil {
			if !errors.Is(got.err, billing.ErrConflict) {
				t.Fatalf("unexpected concurrent recovery failure: %v", got.err)
			}
			if got.result != (purchase.DisputeResult{}) {
				t.Fatalf("rejected recovery returned a result: %+v (%v)", got.result, got.err)
			}
			rejected++
			continue
		}
		if !got.result.Applied || !got.result.RecoveryApplied {
			t.Fatalf("successful recovery result: %+v", got.result)
		}
		applied++
	}
	if applied != 1 || rejected != 1 {
		t.Fatalf("concurrent recoveries applied=%d rejected=%d, want one each", applied, rejected)
	}

	state, err := first.Dispute(ctx, intent.Account, intent.Scope, base.DisputeID)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Recovered) != 1 || state.Recovered[0].Gross != 60 {
		t.Fatalf("bounded recovery state=%+v, want one 60-unit recovery", state.Recovered)
	}
	var recoveryCount, eventCount int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM billing_purchase_dispute_recoveries WHERE account_id=$1`, intent.Account).Scan(&recoveryCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM billing_purchase_dispute_events WHERE account_id=$1`, intent.Account).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if recoveryCount != 1 || eventCount != 1 {
		t.Fatalf("failed recovery left history: recoveries=%d events=%d", recoveryCount, eventCount)
	}

	ledger := credit.New(store, func() time.Time { return now })
	if _, err := ledger.VerifyLedger(ctx, intent.Account); err != nil {
		if errors.Is(err, billing.ErrConflict) {
			t.Fatalf("concurrent recovery corrupted credit ledger: %v", err)
		}
		t.Fatalf("verify ledger: %v", err)
	}
}
