package pg

import (
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
)

func TestPostgresCreditRepairPreservesPendingRevocationAndHeldExposure(t *testing.T) {
	for _, expired := range []bool{false, true} {
		name := "valid"
		if expired {
			name = "expired"
		}
		t.Run(name, func(t *testing.T) {
			store, db := testStore(t)
			ctx := t.Context()
			account := billing.AccountID("repair-pending-revocation")
			if err := store.CreateAccount(ctx, account, string(account)); err != nil {
				t.Fatal(err)
			}
			now := testTime()
			engine := credit.New(store, func() time.Time { return now })
			if _, err := engine.Grant(ctx, credit.GrantInput{Account: account, Operation: "pending-grant", LotID: "pending-lot", Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 10, Source: "repair-test", SourceRef: "pending", ValidFrom: now, ExpiresAt: now.Add(time.Minute)}); err != nil {
				t.Fatal(err)
			}
			if _, err := engine.Reserve(ctx, credit.ReserveInput{Account: account, Operation: "pending-reserve", ReservationID: "pending-reservation", Actor: "worker", Unit: "credits", Amount: 8, Deadline: now.Add(time.Hour)}); err != nil {
				t.Fatal(err)
			}
			if _, err := engine.RevokeAmount(ctx, credit.RevokeAmountInput{Account: account, Operation: "pending-revoke", LotID: "pending-lot", Amount: 5, Reason: "refund"}); err != nil {
				t.Fatal(err)
			}
			if expired {
				now = now.Add(2 * time.Minute)
			}
			var target int64
			if err := db.QueryRowContext(ctx, `SELECT next_journal_sequence FROM billing_accounts WHERE account_id=$1`, account).Scan(&target); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, `UPDATE billing_lots SET initial=11,available=1,pending_revocation=1 WHERE account_id=$1 AND lot_id='pending-lot'`, account); err != nil {
				t.Fatal(err)
			}
			req := CreditRepairRequest{Account: account, ID: "repair-pending", Actor: "operator", Reason: "restore pending revocation", ExpectedJournalSequence: target}
			status, err := store.StartCreditRepair(ctx, req)
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 128 && status.Phase != "ready"; i++ {
				status, err = store.AdvanceCreditRepair(ctx, account, req.ID, status.Revision, 1)
				if err != nil {
					t.Fatal(err)
				}
			}
			if status.Phase != "ready" {
				t.Fatalf("repair did not reach ready: %+v", status)
			}
			status, err = store.ApplyCreditRepair(ctx, account, req.ID, status.Revision)
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 256 && status.Phase != "completed"; i++ {
				status, err = store.AdvanceCreditRepair(ctx, account, req.ID, status.Revision, 1)
				if err != nil {
					t.Fatal(err)
				}
			}
			if status.Phase != "completed" {
				t.Fatalf("repair did not complete: %+v", status)
			}
			var initial, available, held, revoked, pending int64
			if err := db.QueryRowContext(ctx, `SELECT initial,available,held,revoked,pending_revocation FROM billing_lots WHERE account_id=$1 AND lot_id='pending-lot'`, account).Scan(&initial, &available, &held, &revoked, &pending); err != nil {
				t.Fatal(err)
			}
			if initial != 10 || available != 0 || held != 8 || revoked != 2 || pending != 3 {
				t.Fatalf("repaired pending lot=(%d,%d,%d,%d,%d)", initial, available, held, revoked, pending)
			}
			var journalCount int
			if err := db.QueryRowContext(ctx, `SELECT count(*) FROM billing_journal WHERE account_id=$1`, account).Scan(&journalCount); err != nil || journalCount != 3 {
				t.Fatalf("journal count=%d err=%v", journalCount, err)
			}
			var accountAvailable, accountHeld, accountRevoked, scopeAvailable, scopeHeld, scopeRevoked int64
			if err := db.QueryRowContext(ctx, `SELECT available::bigint,held::bigint,revoked::bigint FROM billing_credit_account_balances WHERE account_id=$1 AND unit_code='credits'`, account).Scan(&accountAvailable, &accountHeld, &accountRevoked); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRowContext(ctx, `SELECT available::bigint,held::bigint,revoked::bigint FROM billing_credit_scope_balances WHERE account_id=$1 AND unit_code='credits' AND scope=''`, account).Scan(&scopeAvailable, &scopeHeld, &scopeRevoked); err != nil {
				t.Fatal(err)
			}
			if accountAvailable != 0 || accountHeld != 8 || accountRevoked != 2 || scopeAvailable != accountAvailable || scopeHeld != accountHeld || scopeRevoked != accountRevoked {
				t.Fatalf("raw totals account=(%d,%d,%d) scope=(%d,%d,%d)", accountAvailable, accountHeld, accountRevoked, scopeAvailable, scopeHeld, scopeRevoked)
			}
			if report, err := engine.VerifyLedger(ctx, account); err != nil || report.Entries != 3 {
				t.Fatalf("ledger report=%+v err=%v", report, err)
			}
			if _, err := engine.Release(ctx, credit.ReleaseInput{Account: account, Operation: "pending-release", ReservationID: "pending-reservation", Reason: "completed"}); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRowContext(ctx, `SELECT available,held,revoked,pending_revocation FROM billing_lots WHERE account_id=$1 AND lot_id='pending-lot'`, account).Scan(&available, &held, &revoked, &pending); err != nil {
				t.Fatal(err)
			}
			wantAvailable := int64(5)
			if expired {
				wantAvailable = 0
			}
			if available != wantAvailable || held != 0 || revoked != 5 || pending != 0 {
				t.Fatalf("released pending lot=(%d,%d,%d,%d)", available, held, revoked, pending)
			}
		})
	}
}
