package pg

import (
	"errors"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
)

func TestPostgresCreditRepairResumesAfterStoreReconnect(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("repair-reconnect")
	if err := store.CreateAccount(ctx, account, string(account)); err != nil {
		t.Fatal(err)
	}
	now := testTime()
	engine := credit.New(store, func() time.Time { return now })
	if _, err := engine.Grant(ctx, credit.GrantInput{
		Account: account, Operation: "reconnect-grant", LotID: "reconnect-lot",
		Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 10, Source: "repair-test",
		SourceRef: "reconnect", ValidFrom: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.SetLimit(ctx, credit.LimitInput{
		Account: account, Operation: "reconnect-limit",
		Limit: credit.Limit{Actor: "worker", Unit: "credits", Period: billing.Period{Start: now.Add(-time.Hour), End: now.Add(time.Hour)}, Amount: 10},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Reserve(ctx, credit.ReserveInput{
		Account: account, Operation: "reconnect-reserve", ReservationID: "reconnect-hold",
		Actor: "worker", Unit: "credits", Amount: 6, Deadline: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	var target int64
	if err := db.QueryRowContext(ctx, `SELECT next_journal_sequence FROM billing_accounts WHERE account_id=$1`, account).Scan(&target); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE billing_lots SET available=9, held=0, consumed=1 WHERE account_id=$1 AND lot_id='reconnect-lot'`, account); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE billing_credit_account_balances SET available=9, held=0, consumed=1 WHERE account_id=$1 AND unit_code='credits'`, account); err != nil {
		t.Fatal(err)
	}

	req := CreditRepairRequest{Account: account, ID: "repair-reconnect", Actor: "operator", Reason: "process-death resume", ExpectedJournalSequence: target}
	started, err := store.StartCreditRepair(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	page, err := store.AdvanceCreditRepair(ctx, account, req.ID, started.Revision, 1)
	if err != nil {
		t.Fatal(err)
	}
	if page.Phase == "completed" {
		t.Fatalf("first page finished the repair: %+v", page)
	}

	recoveredStore := New(secondQueueDB(t, db))
	status, err := recoveredStore.CreditRepair(ctx, account, req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if status.Revision != page.Revision || status.Phase != page.Phase || status.JournalCursor != page.JournalCursor {
		t.Fatalf("recovered status=%+v, want page %+v", status, page)
	}
	for i := 0; i < 128 && status.Phase != "ready"; i++ {
		status, err = recoveredStore.AdvanceCreditRepair(ctx, account, req.ID, status.Revision, 1)
		if err != nil {
			t.Fatal(err)
		}
	}
	if status.Phase != "ready" {
		t.Fatalf("repair did not reach ready after reconnect: %+v", status)
	}
	status, err = recoveredStore.ApplyCreditRepair(ctx, account, req.ID, status.Revision)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 256 && status.Phase != "completed"; i++ {
		status, err = recoveredStore.AdvanceCreditRepair(ctx, account, req.ID, status.Revision, 1)
		if err != nil {
			t.Fatal(err)
		}
	}
	if status.Phase != "completed" {
		t.Fatalf("repair did not complete after reconnect: %+v", status)
	}

	recoveredEngine := credit.New(recoveredStore, func() time.Time { return now })
	report, err := recoveredEngine.VerifyLedger(ctx, account)
	if err != nil || report.Lots < 1 {
		t.Fatalf("ledger after reconnect repair=%+v err=%v", report, err)
	}
	got, err := recoveredEngine.Balance(ctx, account, "credits", "")
	if err != nil || got.Available != 4 || got.Held != 6 || got.Consumed != 0 {
		t.Fatalf("repaired balance=%+v err=%v", got, err)
	}
	if _, err := recoveredEngine.Reserve(ctx, credit.ReserveInput{
		Account: account, Operation: "reconnect-reserve-rest", ReservationID: "reconnect-hold-rest",
		Actor: "worker", Unit: "credits", Amount: 4, Deadline: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := recoveredEngine.Reserve(ctx, credit.ReserveInput{
		Account: account, Operation: "reconnect-over-limit", ReservationID: "reconnect-hold-over",
		Actor: "worker", Unit: "credits", Amount: 1, Deadline: now.Add(time.Hour),
	}); !errors.Is(err, billing.ErrLimit) {
		t.Fatalf("over-limit reserve after repaired holds err=%v, want limit", err)
	}
}
