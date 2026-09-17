package pg

import (
	"database/sql"
	"errors"
	"testing"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/purchase"
)

func purchaseAdjustmentFixture(t *testing.T, suffix string) (*Store, *sql.DB, purchase.Intent) {
	t.Helper()
	return purchaseLifecycleFixture(t, billing.AccountID("purchase-adjustment-"+suffix), suffix)
}

func adjustmentRecord(account billing.AccountID, intent purchase.Intent, suffix string) purchase.AdjustmentRecord {
	input := purchase.AdjustmentInput{
		Account: account, ID: "adjustment-" + suffix, IntentID: intent.ID,
		ProviderAdjustmentID: "provider-adjustment-" + suffix, TransactionID: "adjustment-tx-" + suffix,
		Scope: intent.Scope, Kind: purchase.AdjustmentRefund, Currency: intent.Currency,
		Lines:         []purchase.PaidLine{{LineID: "line-" + suffix, Gross: intent.Amount}},
		PolicyVersion: "v1", CreditPolicy: purchase.CreditRefundProportional,
		Actor: "operator", Reason: "customer refund", OccurredAt: testTime(),
	}
	return purchase.AdjustmentRecord{
		Input:     input,
		Result:    purchase.AdjustmentResult{Account: account, ID: input.ID, IntentID: intent.ID, Applied: true},
		CreatedAt: testTime(),
	}
}

func TestPostgresPurchaseAdjustmentImmutableReplayAndProviderScope(t *testing.T) {
	store, _, intent := purchaseAdjustmentFixture(t, "replay")
	ctx := t.Context()
	record := adjustmentRecord(intent.Account, intent, "replay")
	if err := store.Purchases().WithinAccount(ctx, intent.Account, func(tx purchase.Tx) error { return tx.InsertAdjustment(ctx, record) }); err != nil {
		t.Fatal(err)
	}
	var got purchase.AdjustmentRecord
	if err := store.Purchases().WithinAccount(ctx, intent.Account, func(tx purchase.Tx) error { var err error; got, err = tx.Adjustment(ctx, record.Input.ID); return err }); err != nil {
		t.Fatal(err)
	}
	if got.Fingerprint() != record.Fingerprint() || got.Input.ID != record.Input.ID {
		t.Fatalf("got=%+v", got)
	}
	replay := record
	replay.Input.Reason = "changed reason"
	if err := store.Purchases().WithinAccount(ctx, intent.Account, func(tx purchase.Tx) error { return tx.InsertAdjustment(ctx, replay) }); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("changed replay error=%v", err)
	}
	if err := store.Purchases().WithinAccount(ctx, intent.Account, func(tx purchase.Tx) error {
		_, err := tx.ProviderAdjustment(ctx, intent.Scope, record.Input.ProviderAdjustmentID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	foreign := intent
	foreign.Account = "other-adjustment-account"
	if err := store.CreateAccount(ctx, foreign.Account, string(foreign.Account)); err != nil {
		t.Fatal(err)
	}
	if err := store.Purchases().WithinAccount(ctx, foreign.Account, func(tx purchase.Tx) error {
		_, err := tx.ProviderAdjustment(ctx, billing.Scope{Provider: intent.Scope.Provider, Merchant: intent.Scope.Merchant, Environment: intent.Scope.Environment}, record.Input.ProviderAdjustmentID)
		return err
	}); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("foreign provider lookup error=%v", err)
	}
}

func TestPostgresPurchaseAdjustmentStateCASAndRollback(t *testing.T) {
	store, db, intent := purchaseAdjustmentFixture(t, "state")
	ctx := t.Context()
	record := adjustmentRecord(intent.Account, intent, "state")
	if err := store.Purchases().WithinAccount(ctx, intent.Account, func(tx purchase.Tx) error {
		if err := tx.InsertAdjustment(ctx, record); err != nil {
			return err
		}
		state := purchase.AdjustmentState{Account: intent.Account, IntentID: intent.ID, Revision: 1}
		if err := tx.SaveAdjustmentState(ctx, state, 0); err != nil {
			return err
		}
		state.Revision = 2
		return tx.SaveAdjustmentState(ctx, state, 1)
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Purchases().WithinAccount(ctx, intent.Account, func(tx purchase.Tx) error { _, err := tx.AdjustmentState(ctx, intent.ID); return err }); err != nil {
		t.Fatal(err)
	}
	var revision int64
	if err := db.QueryRowContext(ctx, `SELECT revision FROM billing_purchase_adjustment_states WHERE account_id=$1 AND intent_id=$2`, intent.Account, intent.ID).Scan(&revision); err != nil || revision != 2 {
		t.Fatalf("revision=%d err=%v", revision, err)
	}
}

func TestPostgresPurchaseReversalRejectsRelationalOriginalTamper(t *testing.T) {
	_, db, intent, service, input := paidAdjustmentFixture(t, "reversal-integrity")
	ctx := t.Context()
	input.Lines[0].Gross = 200
	if _, err := service.ApplyAdjustment(ctx, input); err != nil {
		t.Fatal(err)
	}
	rows, err := service.Fulfillments(ctx, intent.Account, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	var hostID, otherID string
	for _, row := range rows {
		if row.Effect.Host != nil {
			hostID = row.ID
		} else {
			otherID = row.ID
		}
	}
	if hostID == "" || otherID == "" {
		t.Fatal("mixed fixture did not produce host and non-host effects")
	}
	if _, err := db.ExecContext(ctx, `UPDATE billing_purchase_reversals SET original_effect_id=$3 WHERE account_id=$1 AND intent_id=$2`, intent.Account, intent.ID, otherID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Reversals(ctx, intent.Account, intent.ID); err == nil {
		t.Fatal("relational original effect tamper was accepted")
	}
}

func TestPostgresReversalAdjustmentMustBelongToSameIntent(t *testing.T) {
	store, db, intent, svc, in := paidAdjustmentFixture(t, "reversal-intent-fk")
	ctx := t.Context()
	in.Lines[0].Gross = 200
	if out, err := svc.ApplyAdjustment(ctx, in); err != nil || !out.Applied {
		t.Fatalf("refund: %+v %v", out, err)
	}
	otherInput := intent.IntentInput
	otherInput.ID = "other-intent"
	otherInput.Operation = "other-operation"
	other, err := svc.CreateIntent(ctx, otherInput)
	if err != nil {
		t.Fatal(err)
	}
	record := adjustmentRecord(intent.Account, other, "other-intent")
	record.Result.Applied = false
	record.Result.Rejection = "not_paid"
	if err := store.Purchases().WithinAccount(ctx, intent.Account, func(tx purchase.Tx) error { return tx.InsertAdjustment(ctx, record) }); err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE billing_purchase_reversals SET adjustment_id=$3 WHERE account_id=$1 AND intent_id=$2`, intent.Account, intent.ID, record.Input.ID); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err == nil {
		t.Fatal("cross-intent reversal reference committed")
	}
	revs, err := svc.Reversals(ctx, intent.Account, intent.ID)
	if err != nil || len(revs) != 1 || revs[0].AdjustmentID != in.ID {
		t.Fatalf("failed FK changed reversal: %+v %v", revs, err)
	}
}
