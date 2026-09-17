package pg

import (
	"errors"
	"fmt"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
)

func advanceRepairToAllocations(t *testing.T, store *Store, account billing.AccountID, id string, status CreditRepairStatus) CreditRepairStatus {
	t.Helper()
	for i := 0; i < 128 && status.Phase == "replay"; i++ {
		var err error
		status, err = store.AdvanceCreditRepair(t.Context(), account, id, status.Revision, 64)
		if err != nil {
			t.Fatal(err)
		}
	}
	if status.Phase != "allocations" {
		t.Fatalf("repair did not reach allocations: %+v", status)
	}
	return status
}

func repairTarget(t *testing.T, store *Store, account billing.AccountID) int64 {
	t.Helper()
	var target int64
	if err := store.db.QueryRowContext(t.Context(), `SELECT next_journal_sequence FROM billing_accounts WHERE account_id=$1`, account).Scan(&target); err != nil {
		t.Fatal(err)
	}
	return target
}

func TestPostgresCreditRepairRejectsMissingFirstAllocationPosition(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("repair-missing-first-position")
	if err := store.CreateAccount(ctx, account, string(account)); err != nil {
		t.Fatal(err)
	}
	now := testTime()
	engine := credit.New(store, func() time.Time { return now })
	if _, err := engine.Grant(ctx, credit.GrantInput{
		Account: account, Operation: "missing-first-grant", LotID: "missing-first-lot",
		Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 3, Source: "repair", SourceRef: "missing-first", ValidFrom: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Reserve(ctx, credit.ReserveInput{
		Account: account, Operation: "missing-first-reserve", ReservationID: "missing-first-reservation", Actor: "worker", Unit: "credits", Amount: 1, Deadline: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE billing_reservation_allocations SET position=1 WHERE account_id=$1 AND reservation_id=$2`, account, "missing-first-reservation"); err != nil {
		t.Fatal(err)
	}
	request := CreditRepairRequest{Account: account, ID: "repair-missing-first", Actor: "operator", Reason: "position test", ExpectedJournalSequence: repairTarget(t, store, account)}
	status, err := store.StartCreditRepair(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	status = advanceRepairToAllocations(t, store, account, request.ID, status)
	_, err = store.AdvanceCreditRepair(ctx, account, request.ID, status.Revision, 8)
	if !errors.Is(err, billing.ErrState) {
		t.Fatalf("missing first allocation position error=%v, want ErrState", err)
	}
}

func TestPostgresCreditRepairChecksAllocationAmountAfterExactPageBoundary(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("repair-last-amount")
	if err := store.CreateAccount(ctx, account, string(account)); err != nil {
		t.Fatal(err)
	}
	now := testTime()
	engine := credit.New(store, func() time.Time { return now })
	for i := 0; i < 2; i++ {
		if _, err := engine.Grant(ctx, credit.GrantInput{
			Account: account, Operation: billing.OperationID(fmt.Sprintf("last-amount-grant-%d", i)), LotID: fmt.Sprintf("last-amount-lot-%d", i),
			Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 2, Source: "repair", SourceRef: fmt.Sprintf("last-amount-%d", i), ValidFrom: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := engine.Reserve(ctx, credit.ReserveInput{
		Account: account, Operation: "last-amount-reserve", ReservationID: "last-amount-reservation", Actor: "worker", Unit: "credits", Amount: 3, Deadline: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	result, err := db.ExecContext(ctx, `UPDATE billing_reservation_allocations SET amount=amount+1 WHERE account_id=$1 AND reservation_id=$2 AND position=1`, account, "last-amount-reservation")
	if err != nil {
		t.Fatal(err)
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		t.Fatalf("mutated last allocation rows=%d err=%v", n, err)
	}
	request := CreditRepairRequest{Account: account, ID: "repair-last-amount", Actor: "operator", Reason: "amount test", ExpectedJournalSequence: repairTarget(t, store, account)}
	status, err := store.StartCreditRepair(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	status = advanceRepairToAllocations(t, store, account, request.ID, status)
	// Exactly two rows fill the page. The mismatch must be checked on the
	// following empty page rather than being accepted at the page boundary.
	status, err = store.AdvanceCreditRepair(ctx, account, request.ID, status.Revision, 2)
	if err != nil {
		t.Fatal(err)
	}
	if status.Phase != "allocations" {
		t.Fatalf("exact-boundary page changed phase: %+v", status)
	}
	_, err = store.AdvanceCreditRepair(ctx, account, request.ID, status.Revision, 2)
	if !errors.Is(err, billing.ErrState) {
		t.Fatalf("missing last allocation amount error=%v, want ErrState", err)
	}
}

func TestPostgresCreditRepairRejectsHeldReservationWithoutAllocations(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("repair-zero-allocation")
	if err := store.CreateAccount(ctx, account, string(account)); err != nil {
		t.Fatal(err)
	}
	now := testTime()
	if _, err := credit.New(store, func() time.Time { return now }).Grant(ctx, credit.GrantInput{
		Account: account, Operation: "zero-allocation-grant", LotID: "zero-allocation-lot", Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 2,
		Source: "repair", SourceRef: "zero-allocation", ValidFrom: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO billing_reservations(account_id,reservation_id,actor,unit,scope,created_at,deadline,state,authorized,consumed,evidence) VALUES($1,$2,'worker','credits','',$3,$4,'held',1,0,'{}'::jsonb)`, account, "zero-allocation-reservation", now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	request := CreditRepairRequest{Account: account, ID: "repair-zero-allocation", Actor: "operator", Reason: "allocation test", ExpectedJournalSequence: repairTarget(t, store, account)}
	status, err := store.StartCreditRepair(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	status = advanceRepairToAllocations(t, store, account, request.ID, status)
	_, err = store.AdvanceCreditRepair(ctx, account, request.ID, status.Revision, 8)
	if !errors.Is(err, billing.ErrState) {
		t.Fatalf("held reservation without allocation error=%v, want ErrState", err)
	}
}
