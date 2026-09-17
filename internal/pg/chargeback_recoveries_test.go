package pg

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/purchase"
)

func TestPostgresChargebackRecoveriesReplayAndMigrationBackfill(t *testing.T) {
	store, db, intent, service, fact := chargebackRecoveryFixture(t, "recovery-projection")
	ctx := t.Context()
	lineID := "fulfillment-line-recovery-projection"
	fact.Recovery = &purchase.DisputeRecovery{ID: "recovery-projection-b", AdjustmentID: fact.DebitAdjustmentID, EvidenceReference: "recovery-evidence-b", Lines: []purchase.PaidLine{{LineID: lineID, Gross: 15}}}
	if out, err := service.ApplyDispute(ctx, fact); err != nil || !out.RecoveryApplied {
		t.Fatalf("first recovery=%+v err=%v", out, err)
	}
	fact.EventID = "recovery-projection-event-a"
	fact.Recovery = &purchase.DisputeRecovery{ID: "recovery-projection-a", AdjustmentID: fact.DebitAdjustmentID, EvidenceReference: "recovery-evidence-a", Lines: []purchase.PaidLine{{LineID: lineID, Gross: 10}}}
	if out, err := service.ApplyDispute(ctx, fact); err != nil || !out.RecoveryApplied {
		t.Fatalf("second recovery=%+v err=%v", out, err)
	}
	if out, err := service.ApplyDispute(ctx, fact); err != nil || !out.RecoveryApplied {
		t.Fatalf("replay=%+v err=%v", out, err)
	}
	if err := store.Purchases().WithinAccount(ctx, intent.Account, func(tx purchase.Tx) error {
		stored, err := tx.DisputeRecovery(ctx, intent.Scope, fact.Recovery.ID)
		if err != nil {
			return err
		}
		return tx.InsertDisputeRecovery(ctx, stored)
	}); err != nil {
		t.Fatalf("repository recovery replay: %v", err)
	}
	assertChargebackRecoveries(t, store, intent, []purchase.PaidLine{{LineID: lineID, Gross: 25}})

	var rawBefore, digestBefore string
	if err := db.QueryRowContext(ctx, `SELECT recovery::text,record_digest FROM billing_purchase_dispute_recoveries WHERE account_id=$1 AND recovery_id=$2`, intent.Account, fact.Recovery.ID).Scan(&rawBefore, &digestBefore); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		DROP TRIGGER billing_project_chargeback_recovery_after_insert ON billing_purchase_dispute_recoveries;
		DROP FUNCTION billing_project_chargeback_recovery();
		DROP FUNCTION billing_assert_chargeback_recovery_bounds(text,text);
		DROP FUNCTION billing_validate_chargeback_recovery_lines(jsonb);
		DROP TABLE billing_purchase_chargeback_recovery_totals`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, embeddedMigrationSection(t, "019_chargeback_recovery_totals.sql")); err != nil {
		t.Fatalf("backfill migration: %v", err)
	}
	assertChargebackRecoveries(t, store, intent, []purchase.PaidLine{{LineID: lineID, Gross: 25}})
	var rawAfter, digestAfter string
	if err := db.QueryRowContext(ctx, `SELECT recovery::text,record_digest FROM billing_purchase_dispute_recoveries WHERE account_id=$1 AND recovery_id=$2`, intent.Account, fact.Recovery.ID).Scan(&rawAfter, &digestAfter); err != nil {
		t.Fatal(err)
	}
	if rawAfter != rawBefore || digestAfter != digestBefore {
		t.Fatalf("migration rewrote recovery history: raw changed=%v digest changed=%v", rawAfter != rawBefore, digestAfter != digestBefore)
	}
}

func TestPostgresChargebackRecoveryProjectionRejectsMalformedAndOverflowAtomically(t *testing.T) {
	store, db, intent, _, fact := chargebackRecoveryFixture(t, "recovery-projection-reject")
	ctx := t.Context()
	lineID := "fulfillment-line-recovery-projection-reject"
	insert := func(id, lines string) error {
		_, err := db.ExecContext(ctx, `INSERT INTO billing_purchase_dispute_recoveries(account_id,provider,merchant,environment,recovery_id,adjustment_id,dispute_id,intent_id,transaction_id,recovery,created_at,recovery_fingerprint,record_digest) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10::jsonb,$11,$12,$13)`, intent.Account, intent.Scope.Provider, intent.Scope.Merchant, intent.Scope.Environment, id, fact.DebitAdjustmentID, fact.DisputeID, intent.ID, fact.TransactionID, fmt.Sprintf(`{"ID":%q,"AdjustmentID":%q,"Lines":%s,"EvidenceReference":"evidence"}`, id, fact.DebitAdjustmentID, lines), testTime().Add(time.Minute), "fingerprint", "digest")
		return err
	}
	if err := insert("recovery-duplicate-lines", fmt.Sprintf(`[{"LineID":%q,"Gross":1,"Tax":0},{"LineID":%q,"Gross":1,"Tax":0}]`, lineID, lineID)); err == nil {
		t.Fatal("duplicate recovery lines inserted")
	}
	if err := insert("recovery-exceeds-chargeback", fmt.Sprintf(`[{"LineID":%q,"Gross":101,"Tax":0}]`, lineID)); err == nil {
		t.Fatal("recovery exceeding applied chargeback inserted")
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO billing_purchase_chargeback_recovery_totals(account_id,intent_id,line_id,gross,tax) VALUES($1,$2,$3,$4,0)`, intent.Account, intent.ID, lineID, int64(math.MaxInt64)); err != nil {
		t.Fatal(err)
	}
	err := insert("recovery-overflow", fmt.Sprintf(`[{"LineID":%q,"Gross":1,"Tax":0}]`, lineID))
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); !ok || pgErr.Code != "22003" {
		t.Fatalf("overflow error=%v, want SQLSTATE 22003", err)
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM billing_purchase_dispute_recoveries WHERE account_id=$1 AND recovery_id IN ('recovery-duplicate-lines','recovery-exceeds-chargeback','recovery-overflow')`, intent.Account).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed recovery inserts count=%d err=%v", count, err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM billing_purchase_chargeback_recovery_totals WHERE account_id=$1 AND intent_id=$2`, intent.Account, intent.ID); err != nil {
		t.Fatal(err)
	}
	for i := range 101 {
		if _, err := db.ExecContext(ctx, `INSERT INTO billing_purchase_chargeback_recovery_totals(account_id,intent_id,line_id,gross,tax) VALUES($1,$2,$3,1,0)`, intent.Account, intent.ID, fmt.Sprintf("line-%03d", i)); err != nil {
			t.Fatal(err)
		}
	}
	err = store.Purchases().WithinAccount(ctx, intent.Account, func(tx purchase.Tx) error {
		lines, err := tx.ChargebackRecoveries(ctx, intent.ID)
		if len(lines) != 0 {
			t.Fatalf("bounded read returned partial lines: %d", len(lines))
		}
		return err
	})
	if !errors.Is(err, billing.ErrOverflow) {
		t.Fatalf("bounded read error=%v, want overflow", err)
	}
}

func chargebackRecoveryFixture(t *testing.T, suffix string) (*Store, *sql.DB, purchase.Intent, *purchase.Service, purchase.DisputeFact) {
	t.Helper()
	store, db, intent, service, debit := paidAdjustmentFixture(t, suffix)
	debit.Kind = purchase.AdjustmentChargeback
	debit.ID = "chargeback-" + suffix
	debit.ProviderAdjustmentID = "provider-chargeback-" + suffix
	if out, err := service.ApplyAdjustment(t.Context(), debit); err != nil || !out.Applied {
		t.Fatalf("chargeback=%+v err=%v", out, err)
	}
	now := testTime().Add(time.Minute)
	service = purchase.New(store.Purchases(), func() time.Time { return now })
	fact := purchase.DisputeFact{Account: intent.Account, Scope: intent.Scope, EventID: "dispute-event-" + suffix, DisputeID: "dispute-" + suffix, IntentID: intent.ID, TransactionID: debit.TransactionID, Currency: debit.Currency, Amount: debit.Lines[0].Gross, Status: purchase.DisputeLost, OccurredAt: now, EvidenceReference: "dispute-evidence-" + suffix, DebitAdjustmentID: debit.ID}
	if out, err := service.ApplyDispute(t.Context(), fact); err != nil || !out.Applied {
		t.Fatalf("dispute=%+v err=%v", out, err)
	}
	fact.EventID = "recovery-event-" + suffix
	fact.OccurredAt = now.Add(time.Minute)
	service = purchase.New(store.Purchases(), func() time.Time { return now.Add(2 * time.Minute) })
	return store, db, intent, service, fact
}

func assertChargebackRecoveries(t *testing.T, store *Store, intent purchase.Intent, want []purchase.PaidLine) {
	t.Helper()
	err := store.Purchases().WithinAccount(t.Context(), intent.Account, func(tx purchase.Tx) error {
		got, err := tx.ChargebackRecoveries(t.Context(), intent.ID)
		if err == nil && !slices.Equal(got, want) {
			t.Fatalf("recoveries=%+v, want %+v", got, want)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}
