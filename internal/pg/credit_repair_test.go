package pg

import (
	"errors"
	"fmt"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
)

func TestPostgresCreditRepairReplaysBoundedPagesAndPublishesOnlyAfterApproval(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("credit-repair-pages")
	if err := store.CreateAccount(ctx, account, string(account)); err != nil {
		t.Fatal(err)
	}
	now := testTime()
	engine := credit.New(store, func() time.Time { return now })
	for i := 0; i < 5; i++ {
		if _, err := engine.Grant(ctx, credit.GrantInput{
			Account: account, Operation: billing.OperationID(fmt.Sprintf("repair-grant-%d", i)),
			LotID: fmt.Sprintf("repair-lot-%d", i), Unit: billing.Unit{Code: "credits", Scale: 1},
			Amount: 10, Source: "repair-test", SourceRef: fmt.Sprintf("source-%d", i),
			ValidFrom: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	var target int64
	if err := db.QueryRowContext(ctx, `SELECT next_journal_sequence FROM billing_accounts WHERE account_id=$1`, account).Scan(&target); err != nil {
		t.Fatal(err)
	}
	// Keep the row valid while making both the lot projection and aggregate
	// projection disagree with the immutable grant journal.
	if _, err := db.ExecContext(ctx, `
		UPDATE billing_lots SET available=7, consumed=3
		WHERE account_id=$1 AND lot_id='repair-lot-2'`, account); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		UPDATE billing_credit_account_balances
		SET available=777, consumed=0
		WHERE account_id=$1 AND unit_code='credits'`, account); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		UPDATE billing_credit_scope_balances
		SET available=777, consumed=0
		WHERE account_id=$1 AND unit_code='credits' AND scope=''`, account); err != nil {
		t.Fatal(err)
	}

	request := CreditRepairRequest{
		Account: account, ID: "repair-pages", Actor: "operator-1", Reason: "restore projection",
		ExpectedJournalSequence: target,
	}
	started, err := store.StartCreditRepair(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if started.Phase != "replay" || started.Revision != 0 || started.TargetSequence != target {
		t.Fatalf("start status=%+v", started)
	}
	// Start is request-idempotent, but the identity/evidence is immutable.
	replayed, err := store.StartCreditRepair(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Revision != started.Revision || replayed.TargetSequence != started.TargetSequence || replayed.Phase != started.Phase {
		t.Fatalf("replayed start changed status: first=%+v replay=%+v", started, replayed)
	}
	if _, _, _, err := store.CreditRepairEvidencePage(ctx, account, request.ID, "", 2); !errors.Is(err, billing.ErrState) {
		t.Fatalf("evidence exposed before verification: %v", err)
	}
	changed := request
	changed.Reason = "different evidence"
	if _, err := store.StartCreditRepair(ctx, changed); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("changed repair request error=%v, want conflict", err)
	}

	page, err := store.AdvanceCreditRepair(ctx, account, request.ID, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if page.Revision != 1 || page.JournalCursor != 2 {
		t.Fatalf("first bounded page=%+v", page)
	}
	if _, err := store.AdvanceCreditRepair(ctx, account, request.ID, 0, 2); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("stale advancement error=%v, want conflict", err)
	}
	if _, err := store.ApplyCreditRepair(ctx, account, request.ID, page.Revision); !errors.Is(err, billing.ErrState) {
		t.Fatalf("early apply error=%v, want state error", err)
	}

	status := page
	for i := 0; i < 64 && status.Phase != "ready"; i++ {
		status, err = store.AdvanceCreditRepair(ctx, account, request.ID, status.Revision, 2)
		if err != nil {
			t.Fatal(err)
		}
	}
	if status.Phase != "ready" {
		t.Fatalf("repair did not reach ready: %+v", status)
	}
	if status.ReplayLots != 5 || status.VerifiedLots != 5 {
		t.Fatalf("ready counters=%+v", status)
	}
	evidence, next, more, err := store.CreditRepairEvidencePage(ctx, account, request.ID, "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence) != 2 || next == "" || !more {
		t.Fatalf("first evidence page len=%d next=%q more=%v", len(evidence), next, more)
	}
	rest, next, more, err := store.CreditRepairEvidencePage(ctx, account, request.ID, next, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 2 || next == "" || !more {
		t.Fatalf("second evidence page len=%d next=%q more=%v", len(rest), next, more)
	}
	status, err = store.ApplyCreditRepair(ctx, account, request.ID, status.Revision)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 128 && status.Phase != "completed"; i++ {
		status, err = store.AdvanceCreditRepair(ctx, account, request.ID, status.Revision, 2)
		if err != nil {
			t.Fatal(err)
		}
	}
	if status.Phase != "completed" {
		t.Fatalf("repair did not complete: %+v", status)
	}

	var available, consumed int64
	if err := db.QueryRowContext(ctx, `
		SELECT available,consumed FROM billing_lots
		WHERE account_id=$1 AND lot_id='repair-lot-2'`, account).Scan(&available, &consumed); err != nil {
		t.Fatal(err)
	}
	if available != 10 || consumed != 0 {
		t.Fatalf("repaired lot=(available %d, consumed %d)", available, consumed)
	}
	var aggregateAvailable, aggregateConsumed int64
	if err := db.QueryRowContext(ctx, `
		SELECT available::bigint,consumed::bigint FROM billing_credit_account_balances
		WHERE account_id=$1 AND unit_code='credits'`, account).Scan(&aggregateAvailable, &aggregateConsumed); err != nil {
		t.Fatal(err)
	}
	if aggregateAvailable != 50 || aggregateConsumed != 0 {
		t.Fatalf("repaired aggregate=(available %d, consumed %d)", aggregateAvailable, aggregateConsumed)
	}
	var journalCount, operationCount int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM billing_journal WHERE account_id=$1`, account).Scan(&journalCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM billing_operations WHERE account_id=$1`, account).Scan(&operationCount); err != nil {
		t.Fatal(err)
	}
	if journalCount != 5 || operationCount != 5 {
		t.Fatalf("repair created financial effects: journal=%d operations=%d", journalCount, operationCount)
	}
	if report, err := credit.New(store, func() time.Time { return now }).VerifyLedger(ctx, account); err != nil || report.Lots != 5 || report.Entries != 5 {
		t.Fatalf("final ledger report=%+v err=%v", report, err)
	}
	if got, err := credit.New(store, func() time.Time { return now }).Balance(ctx, account, "credits", ""); err != nil || got.Available != 50 || got.Consumed != 0 {
		t.Fatalf("final balance=%+v err=%v", got, err)
	}
}

func TestPostgresCreditRepairFenceAndStatusRecovery(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("credit-repair-fence")
	if err := store.CreateAccount(ctx, account, string(account)); err != nil {
		t.Fatal(err)
	}
	now := testTime()
	engine := credit.New(store, func() time.Time { return now })
	if _, err := engine.Grant(ctx, credit.GrantInput{
		Account: account, Operation: "fence-grant", LotID: "fence-lot",
		Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 10,
		Source: "repair-test", SourceRef: "fence", ValidFrom: now, ExpiresAt: now.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Reserve(ctx, credit.ReserveInput{
		Account: account, Operation: "fence-reserve-seed", ReservationID: "fence-reservation-seed", Actor: "operator", Unit: "credits", Amount: 5, Deadline: now.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	var target int64
	if err := store.db.QueryRowContext(ctx, `SELECT next_journal_sequence FROM billing_accounts WHERE account_id=$1`, account).Scan(&target); err != nil {
		t.Fatal(err)
	}
	request := CreditRepairRequest{Account: account, ID: "repair-fence", Actor: "operator-2", Reason: "freeze writes", ExpectedJournalSequence: target}
	if _, err := store.StartCreditRepair(ctx, request); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Grant(ctx, credit.GrantInput{
		Account: account, Operation: "fenced-grant", LotID: "fenced-lot", Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 1,
		Source: "repair-test", SourceRef: "fenced", ValidFrom: now,
	}); !errors.Is(err, credit.ErrRepairInProgress) && !errors.Is(err, billing.ErrState) {
		t.Fatalf("grant while frozen error=%v, want repair fence or unavailable projection", err)
	}
	if _, err := engine.Reserve(ctx, credit.ReserveInput{
		Account: account, Operation: "fenced-reserve", ReservationID: "fenced-reservation", Actor: "operator", Unit: "credits", Amount: 1, Deadline: now.Add(time.Hour),
	}); !errors.Is(err, credit.ErrRepairInProgress) && !errors.Is(err, billing.ErrState) {
		t.Fatalf("reserve while frozen error=%v, want repair fence or unavailable projection", err)
	}
	if _, err := credit.New(store, func() time.Time { return now.Add(2 * time.Hour) }).Sweep(ctx, account); !errors.Is(err, credit.ErrRepairInProgress) {
		t.Fatalf("sweep while frozen error=%v, want ErrRepairInProgress", err)
	}
	if err := store.WithinAccount(ctx, account, func(tx credit.Tx) error {
		return tx.PutLot(testLot("fenced-direct-lot", 1))
	}); !errors.Is(err, credit.ErrRepairInProgress) {
		t.Fatalf("direct lot write while frozen error=%v, want ErrRepairInProgress", err)
	}
	status, err := store.CreditRepair(ctx, account, request.ID)
	if err != nil {
		t.Fatal(err)
	}
	if status.ID != request.ID || status.Actor != request.Actor || status.Reason != request.Reason || status.Revision != 0 {
		t.Fatalf("recovered status=%+v", status)
	}
}

func TestPostgresCreditRepairMissingJournalFailsClosed(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("credit-repair-missing-journal")
	if err := store.CreateAccount(ctx, account, string(account)); err != nil {
		t.Fatal(err)
	}
	now := testTime()
	if _, err := credit.New(store, func() time.Time { return now }).Grant(ctx, credit.GrantInput{
		Account: account, Operation: "missing-journal-grant", LotID: "missing-journal-lot",
		Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 3, Source: "repair-test", SourceRef: "missing", ValidFrom: now,
	}); err != nil {
		t.Fatal(err)
	}
	var target int64
	if err := db.QueryRowContext(ctx, `SELECT next_journal_sequence FROM billing_accounts WHERE account_id=$1`, account).Scan(&target); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM billing_journal WHERE account_id=$1 AND sequence=1`, account); err != nil {
		t.Fatal(err)
	}
	request := CreditRepairRequest{Account: account, ID: "repair-missing-journal", Actor: "operator-3", Reason: "restore missing evidence", ExpectedJournalSequence: target}
	if _, err := store.StartCreditRepair(ctx, request); !errors.Is(err, billing.ErrState) {
		t.Fatalf("start with deleted latest journal error=%v, want state", err)
	}
	if _, err := store.CreditRepair(ctx, account, request.ID); !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("deleted-latest start created repair: %v", err)
	}

	// A middle gap leaves the latest journal sequence intact, so start is
	// allowed but replay must fail closed when it sees the missing sequence.
	account = billing.AccountID("credit-repair-middle-gap")
	if err := store.CreateAccount(ctx, account, string(account)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := credit.New(store, func() time.Time { return now }).Grant(ctx, credit.GrantInput{
			Account: account, Operation: billing.OperationID(fmt.Sprintf("middle-gap-grant-%d", i)), LotID: fmt.Sprintf("middle-gap-lot-%d", i),
			Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 2, Source: "repair-test", SourceRef: fmt.Sprintf("middle-gap-%d", i), ValidFrom: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM billing_journal WHERE account_id=$1 AND sequence=1`, account); err != nil {
		t.Fatal(err)
	}
	middle := CreditRepairRequest{Account: account, ID: "repair-middle-gap", Actor: "operator-3", Reason: "restore middle evidence", ExpectedJournalSequence: 2}
	if _, err := store.StartCreditRepair(ctx, middle); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AdvanceCreditRepair(ctx, account, middle.ID, 0, 2); !errors.Is(err, billing.ErrState) {
		t.Fatalf("middle gap replay error=%v, want state", err)
	}
	status, err := store.CreditRepair(ctx, account, middle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if status.Phase == "completed" || status.Phase == "ready" {
		t.Fatalf("missing evidence became publishable: %+v", status)
	}
}

func TestPostgresCreditRepairRejectsStaleExpectedJournalSequence(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("credit-repair-stale-sequence")
	if err := store.CreateAccount(ctx, account, string(account)); err != nil {
		t.Fatal(err)
	}
	now := testTime()
	if _, err := credit.New(store, func() time.Time { return now }).Grant(ctx, credit.GrantInput{
		Account: account, Operation: "stale-sequence-grant", LotID: "stale-sequence-lot", Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 1,
		Source: "repair-test", SourceRef: "stale-sequence", ValidFrom: now,
	}); err != nil {
		t.Fatal(err)
	}
	request := CreditRepairRequest{Account: account, ID: "repair-stale-sequence", Actor: "operator-stale", Reason: "stale sequence", ExpectedJournalSequence: 0}
	if _, err := store.StartCreditRepair(ctx, request); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("stale expected sequence error=%v, want conflict", err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE billing_accounts SET next_journal_sequence=0 WHERE account_id=$1`, account); err != nil {
		t.Fatal(err)
	}
	if _, err := store.StartCreditRepair(ctx, request); !errors.Is(err, billing.ErrState) {
		t.Fatalf("corrupt stored sequence error=%v, want state", err)
	}
	if _, err := store.CreditRepair(ctx, account, request.ID); !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("stale sequence created repair: %v", err)
	}
}

func TestPostgresCreditRepairEmptyAccountCompletesWithoutFinancialEffects(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("credit-repair-empty")
	other := billing.AccountID("credit-repair-other")
	if err := store.CreateAccount(ctx, account, string(account)); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateAccount(ctx, other, string(other)); err != nil {
		t.Fatal(err)
	}
	request := CreditRepairRequest{Account: account, ID: "repair-empty", Actor: "operator-empty", Reason: "check empty account", ExpectedJournalSequence: 0}
	status, err := store.StartCreditRepair(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if status.Phase != "replay" {
		t.Fatalf("empty start=%+v", status)
	}
	if _, err := store.BuildCreditBalanceProjection(ctx, account, 2); !errors.Is(err, credit.ErrRepairInProgress) {
		t.Fatalf("projection rebuild bypassed repair fence: %v", err)
	}
	if _, err := credit.New(store, func() time.Time { return testTime() }).Grant(ctx, credit.GrantInput{
		Account: other, Operation: "other-grant", LotID: "other-lot", Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 4,
		Source: "repair-test", SourceRef: "other", ValidFrom: testTime(),
	}); err != nil {
		t.Fatalf("other tenant write was fenced: %v", err)
	}
	for i := 0; i < 16 && status.Phase != "ready"; i++ {
		status, err = store.AdvanceCreditRepair(ctx, account, request.ID, status.Revision, 2)
		if err != nil {
			t.Fatal(err)
		}
	}
	if status.Phase != "ready" || status.ReplayLots != 0 || status.VerifiedLots != 0 {
		t.Fatalf("empty account did not reach ready: %+v", status)
	}
	status, err = store.ApplyCreditRepair(ctx, account, request.ID, status.Revision)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 16 && status.Phase != "completed"; i++ {
		status, err = store.AdvanceCreditRepair(ctx, account, request.ID, status.Revision, 2)
		if err != nil {
			t.Fatal(err)
		}
	}
	if status.Phase != "completed" {
		t.Fatalf("empty account did not complete: %+v", status)
	}
	var lots, journal int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM billing_lots WHERE account_id=$1`, account).Scan(&lots); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM billing_journal WHERE account_id=$1`, account).Scan(&journal); err != nil {
		t.Fatal(err)
	}
	if lots != 0 || journal != 0 {
		t.Fatalf("empty repair created rows: lots=%d journal=%d", lots, journal)
	}
}

func TestPostgresCreditRepairReconstructsHeldAllocationsAcrossPages(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("credit-repair-allocations")
	if err := store.CreateAccount(ctx, account, string(account)); err != nil {
		t.Fatal(err)
	}
	now := testTime()
	engine := credit.New(store, func() time.Time { return now })
	for i := 0; i < 3; i++ {
		if _, err := engine.Grant(ctx, credit.GrantInput{
			Account: account, Operation: billing.OperationID(fmt.Sprintf("allocation-grant-%d", i)), LotID: fmt.Sprintf("allocation-lot-%d", i),
			Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 10, Source: "repair-test", SourceRef: fmt.Sprintf("allocation-%d", i), ValidFrom: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := engine.Reserve(ctx, credit.ReserveInput{Account: account, Operation: "allocation-reserve", ReservationID: "allocation-reservation", Actor: "worker", Unit: "credits", Amount: 25, Deadline: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Reserve(ctx, credit.ReserveInput{Account: account, Operation: "allocation-reserve-second", ReservationID: "allocation-reservation-second", Actor: "worker", Unit: "credits", Amount: 5, Deadline: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	// This remains conservation-valid but disagrees with the retained allocation
	// evidence: the first lot is allocated for ten units, while its projection
	// claims only eight are held.
	if _, err := db.ExecContext(ctx, `UPDATE billing_lots SET available=2,held=8 WHERE account_id=$1 AND lot_id='allocation-lot-0'`, account); err != nil {
		t.Fatal(err)
	}
	var target int64
	if err := db.QueryRowContext(ctx, `SELECT next_journal_sequence FROM billing_accounts WHERE account_id=$1`, account).Scan(&target); err != nil {
		t.Fatal(err)
	}
	request := CreditRepairRequest{Account: account, ID: "repair-allocations", Actor: "operator-allocation", Reason: "restore held projection", ExpectedJournalSequence: target}
	status, err := store.StartCreditRepair(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 64 && status.Phase != "ready"; i++ {
		status, err = store.AdvanceCreditRepair(ctx, account, request.ID, status.Revision, 1)
		if err != nil {
			t.Fatal(err)
		}
	}
	if status.Phase != "ready" || status.ReplayLots != 3 || status.VerifiedLots != 3 {
		t.Fatalf("held allocation repair status=%+v", status)
	}
	status, err = store.ApplyCreditRepair(ctx, account, request.ID, status.Revision)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 128 && status.Phase != "completed"; i++ {
		status, err = store.AdvanceCreditRepair(ctx, account, request.ID, status.Revision, 1)
		if err != nil {
			t.Fatal(err)
		}
	}
	if status.Phase != "completed" {
		t.Fatalf("held allocation repair did not complete: %+v", status)
	}
	var available, held int64
	if err := db.QueryRowContext(ctx, `SELECT available,held FROM billing_lots WHERE account_id=$1 AND lot_id='allocation-lot-0'`, account).Scan(&available, &held); err != nil {
		t.Fatal(err)
	}
	if available != 0 || held != 10 {
		t.Fatalf("held allocation not reconstructed: available=%d held=%d", available, held)
	}
}
