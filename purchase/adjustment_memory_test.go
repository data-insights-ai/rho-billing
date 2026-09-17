package purchase

import (
	"context"
	"errors"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
)

func TestMemoryAdjustmentPersistenceRollbackReplayAndCAS(t *testing.T) {
	account := billing.AccountID("adjustment-memory")
	repo := NewMemoryRepository(ReferenceAccount{Account: account})
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	record, state, reversal := adjustmentMemoryFixtures(account, now)
	wantErr := errors.New("rollback adjustment")
	if err := repo.WithinAccount(ctx, account, func(tx Tx) error {
		seedAdjustmentReferences(tx.(*memoryTx), reversal.Original)
		if err := tx.InsertAdjustment(ctx, record); err != nil {
			return err
		}
		if err := tx.SaveAdjustmentState(ctx, state, 0); err != nil {
			return err
		}
		if err := tx.InsertReversal(ctx, reversal); err != nil {
			return err
		}
		return wantErr
	}); !errors.Is(err, wantErr) {
		t.Fatalf("callback error=%v, want %v", err, wantErr)
	}
	if err := repo.WithinAccount(ctx, account, func(tx Tx) error {
		for _, read := range []func() error{
			func() error { _, err := tx.Adjustment(ctx, record.Input.ID); return err },
			func() error { _, err := tx.AdjustmentState(ctx, state.IntentID); return err },
			func() error { _, err := tx.Reversal(ctx, reversal.ID); return err },
		} {
			if err := read(); !errors.Is(err, billing.ErrNotFound) {
				return errors.New("rolled-back adjustment state remains")
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := repo.WithinAccount(ctx, account, func(tx Tx) error {
		seedAdjustmentReferences(tx.(*memoryTx), reversal.Original)
		if err := tx.InsertAdjustment(ctx, record); err != nil {
			return err
		}
		if err := tx.SaveAdjustmentState(ctx, state, 0); err != nil {
			return err
		}
		return tx.InsertReversal(ctx, reversal)
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.WithinAccount(ctx, account, func(tx Tx) error {
		if err := tx.InsertAdjustment(ctx, record); err != nil {
			return err
		}
		if err := tx.SaveAdjustmentState(ctx, state, 0); err != nil {
			return err
		}
		if err := tx.InsertReversal(ctx, reversal); err != nil {
			return err
		}
		rows, err := tx.Reversals(ctx, reversal.IntentID)
		if err != nil || len(rows) != 1 || rows[0].ID != reversal.ID {
			return errors.New("reversal intent listing missing row")
		}
		rows, err = tx.Reversals(ctx, reversal.AdjustmentID)
		if err != nil || len(rows) != 0 {
			return errors.New("reversal listing used adjustment scope")
		}
		got, err := tx.Adjustment(ctx, record.Input.ID)
		if err != nil {
			return err
		}
		got.Input.Lines[0].Gross++
		fresh, err := tx.Adjustment(ctx, record.Input.ID)
		if err != nil {
			return err
		}
		if fresh.Input.Lines[0].Gross != record.Input.Lines[0].Gross {
			return errors.New("adjustment read was not detached")
		}
		updated := state
		updated.Revision = 2
		updated.Lines[0].RefundedGross = 25
		if err := tx.SaveAdjustmentState(ctx, updated, 1); err != nil {
			return err
		}
		completed := reversal
		completed.State = FulfillmentComplete
		completed.HostReference = "host-reversal"
		completed.AppliedAt = now.Add(time.Minute)
		completed.AcknowledgedAt = now.Add(2 * time.Minute)
		return tx.SaveReversal(ctx, completed, reversal.Fingerprint())
	}); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryAdjustmentProviderScopeIsAccountGlobal(t *testing.T) {
	scope := billing.Scope{Provider: "example", Merchant: "merchant", Environment: "sandbox"}
	accountA := billing.AccountID("adjustment-owner-a")
	accountB := billing.AccountID("adjustment-owner-b")
	repo := NewMemoryRepository(ReferenceAccount{Account: accountA}, ReferenceAccount{Account: accountB})
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	record, _, reversal := adjustmentMemoryFixtures(accountA, now)
	record.Input.Scope = scope
	record.Input.ProviderAdjustmentID = "provider-adjustment-global"
	if err := repo.WithinAccount(t.Context(), accountA, func(tx Tx) error {
		seedAdjustmentReferences(tx.(*memoryTx), reversal.Original)
		return tx.InsertAdjustment(t.Context(), record)
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.WithinAccount(t.Context(), accountB, func(tx Tx) error {
		if _, err := tx.ProviderAdjustment(t.Context(), scope, record.Input.ProviderAdjustmentID); !errors.Is(err, billing.ErrConflict) {
			return errors.New("foreign provider adjustment was not hidden by conflict")
		}
		foreign := record
		foreign.Input.Account = accountB
		foreign.Input.ID = "adjustment-b"
		foreign.Result.Account = accountB
		foreign.Result.ID = foreign.Input.ID
		foreign.Input.IntentID = "intent-b"
		foreign.Result.IntentID = foreign.Input.IntentID
		intent := Intent{IntentInput: IntentInput{Account: accountB, ID: foreign.Input.IntentID}}
		tx.(*memoryTx).lifecycle.intents[lifecycleIntentKey{account: accountB, id: intent.ID}] = intent
		if err := tx.InsertAdjustment(t.Context(), foreign); !errors.Is(err, billing.ErrConflict) {
			return errors.New("foreign provider adjustment was overwritten")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func adjustmentMemoryFixtures(account billing.AccountID, now time.Time) (AdjustmentRecord, AdjustmentState, Reversal) {
	intentID := "intent-adjustment"
	original := Fulfillment{
		Account: account, ID: fulfillmentID(account, intentID, "line-1", "host-effect"), IntentID: intentID, LineID: "line-1",
		Effect: Effect{Key: "host-effect", Host: &HostBenefit{Kind: "host", Payload: []byte(`{"ok":true}`)}}, Quantity: 1,
		EffectiveAt: now, CreatedAt: now, State: FulfillmentPending,
	}
	in := AdjustmentInput{Account: account, ID: "adjustment-1", IntentID: intentID, ProviderAdjustmentID: "provider-adjustment-1", TransactionID: "transaction-1", Scope: billing.Scope{Provider: "example", Merchant: "merchant", Environment: "sandbox"}, Kind: AdjustmentRefund, Currency: "USD", Lines: []PaidLine{{LineID: "line-1", Gross: 100, Tax: 10}}, PolicyVersion: "policy-1", CreditPolicy: CreditRefundProportional, Actor: "actor-1", Reason: "customer refund", OccurredAt: now}
	record := AdjustmentRecord{Input: in, Result: AdjustmentResult{Account: account, ID: in.ID, IntentID: in.IntentID, Applied: true}, CreatedAt: now}
	state := AdjustmentState{Account: account, IntentID: intentID, Revision: 1, Lines: []LineAdjustmentTotal{{LineID: "line-1"}}}
	reversal := Reversal{Account: account, ID: reversalID(account, original.ID), AdjustmentID: in.ID, IntentID: intentID, Original: original, EffectiveAt: now, CreatedAt: now, State: FulfillmentPending}
	return record, state, reversal
}

func seedAdjustmentReferences(tx *memoryTx, original Fulfillment) {
	tx.lifecycle.intents[lifecycleIntentKey{account: tx.account, id: original.IntentID}] = Intent{IntentInput: IntentInput{Account: tx.account, ID: original.IntentID}}
	tx.state.fulfillments[original.ID] = cloneFulfillment(original)
}
