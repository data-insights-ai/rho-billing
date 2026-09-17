package pg

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
)

func sameCreditRepairStatus(a, b CreditRepairStatus) bool {
	return a.Account == b.Account && a.ID == b.ID && a.Actor == b.Actor && a.Reason == b.Reason && a.Phase == b.Phase &&
		a.Revision == b.Revision && a.TargetSequence == b.TargetSequence && a.JournalCursor == b.JournalCursor &&
		a.LotCursor == b.LotCursor && a.ReservationCursor == b.ReservationCursor && a.AllocationPosition == b.AllocationPosition &&
		a.ReplayLots == b.ReplayLots && a.VerifiedLots == b.VerifiedLots && a.ChangedLots == b.ChangedLots && a.AppliedLots == b.AppliedLots &&
		a.CreatedAt.Equal(b.CreatedAt) && a.UpdatedAt.Equal(b.UpdatedAt)
}

func creditRepairFailureFixture(t *testing.T, account billing.AccountID, repairID string) (*Store, *sql.DB, CreditRepairRequest, CreditRepairStatus) {
	t.Helper()
	store, db := testStore(t)
	ctx := t.Context()
	if err := store.CreateAccount(ctx, account, string(account)); err != nil {
		t.Fatal(err)
	}
	now := testTime()
	if _, err := credit.New(store, func() time.Time { return now }).Grant(ctx, credit.GrantInput{Account: account, Operation: billing.OperationID(repairID + "-grant"), LotID: repairID + "-lot", Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 2, Source: "repair-test", SourceRef: repairID, ValidFrom: now}); err != nil {
		t.Fatal(err)
	}
	var target int64
	if err := db.QueryRowContext(ctx, `SELECT next_journal_sequence FROM billing_accounts WHERE account_id=$1`, account).Scan(&target); err != nil {
		t.Fatal(err)
	}
	req := CreditRepairRequest{Account: account, ID: repairID, Actor: "operator", Reason: "repair test", ExpectedJournalSequence: target}
	started, err := store.StartCreditRepair(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	return store, db, req, started
}

func TestCreditRepairStartDeferredCommitReturnsZeroAndRetries(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("repair-start-failure")
	if err := store.CreateAccount(ctx, account, string(account)); err != nil {
		t.Fatal(err)
	}
	var target int64
	if err := db.QueryRowContext(ctx, `SELECT next_journal_sequence FROM billing_accounts WHERE account_id=$1`, account).Scan(&target); err != nil {
		t.Fatal(err)
	}
	req := CreditRepairRequest{Account: account, ID: "repair-start", Actor: "operator", Reason: "commit test", ExpectedJournalSequence: target}
	if _, err := db.ExecContext(ctx, `CREATE FUNCTION fail_repair_start() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'repair start commit failure'; END $$; CREATE CONSTRAINT TRIGGER fail_repair_start AFTER INSERT ON billing_credit_repair_jobs DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION fail_repair_start()`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = db.ExecContext(ctx, `DROP TRIGGER IF EXISTS fail_repair_start ON billing_credit_repair_jobs`)
		_, _ = db.ExecContext(ctx, `DROP FUNCTION IF EXISTS fail_repair_start()`)
	}()
	status, err := store.StartCreditRepair(ctx, req)
	if err == nil || status != (CreditRepairStatus{}) {
		t.Fatalf("failed start status=%+v err=%v", status, err)
	}
	var jobs int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM billing_credit_repair_jobs WHERE account_id=$1`, account).Scan(&jobs); err != nil || jobs != 0 {
		t.Fatalf("failed start persisted jobs=%d err=%v", jobs, err)
	}
	if _, err := db.ExecContext(ctx, `DROP TRIGGER fail_repair_start ON billing_credit_repair_jobs`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DROP FUNCTION fail_repair_start()`); err != nil {
		t.Fatal(err)
	}
	if status, err := store.StartCreditRepair(ctx, req); err != nil || status.ID != req.ID {
		t.Fatalf("retry start status=%+v err=%v", status, err)
	}
}

func TestCreditRepairAdvanceDeferredCommitReturnsZeroAndRetries(t *testing.T) {
	store, db, req, started := creditRepairFailureFixture(t, "repair-advance-failure", "repair-advance")
	ctx := t.Context()
	if _, err := db.ExecContext(ctx, `CREATE FUNCTION fail_repair_advance() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'repair advance commit failure'; END $$; CREATE CONSTRAINT TRIGGER fail_repair_advance AFTER UPDATE OF revision ON billing_credit_repair_jobs DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION fail_repair_advance()`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = db.ExecContext(ctx, `DROP TRIGGER IF EXISTS fail_repair_advance ON billing_credit_repair_jobs`)
		_, _ = db.ExecContext(ctx, `DROP FUNCTION IF EXISTS fail_repair_advance()`)
	}()
	status, err := store.AdvanceCreditRepair(ctx, req.Account, req.ID, started.Revision, 1)
	if err == nil || status != (CreditRepairStatus{}) {
		t.Fatalf("failed advance status=%+v err=%v", status, err)
	}
	recovered, err := store.CreditRepair(ctx, req.Account, req.ID)
	if err != nil || recovered.Revision != started.Revision || recovered.JournalCursor != started.JournalCursor {
		t.Fatalf("failed advance changed checkpoint=%+v err=%v", recovered, err)
	}
	if _, err := db.ExecContext(ctx, `DROP TRIGGER fail_repair_advance ON billing_credit_repair_jobs`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DROP FUNCTION fail_repair_advance()`); err != nil {
		t.Fatal(err)
	}
	if retry, err := store.AdvanceCreditRepair(ctx, req.Account, req.ID, recovered.Revision, 1); err != nil || retry.Revision <= recovered.Revision {
		t.Fatalf("retry advance status=%+v err=%v", retry, err)
	}
}

func TestCreditRepairPublicationDeferredCommitReturnsZeroAndRetries(t *testing.T) {
	store, db, req, status := creditRepairFailureFixture(t, "repair-publish-failure", "repair-publish")
	ctx := t.Context()
	var err error
	for i := 0; i < 128 && status.Phase != "ready"; i++ {
		status, err = store.AdvanceCreditRepair(ctx, req.Account, req.ID, status.Revision, 10)
		if err != nil {
			t.Fatal(err)
		}
	}
	if status.Phase != "ready" {
		t.Fatalf("repair did not reach ready: %+v", status)
	}
	status, err = store.ApplyCreditRepair(ctx, req.Account, req.ID, status.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE FUNCTION fail_repair_publish() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'repair publish commit failure'; END $$; CREATE CONSTRAINT TRIGGER fail_repair_publish AFTER UPDATE OF phase ON billing_credit_repair_jobs DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN (NEW.phase='completed') EXECUTE FUNCTION fail_repair_publish()`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = db.ExecContext(ctx, `DROP TRIGGER IF EXISTS fail_repair_publish ON billing_credit_repair_jobs`)
		_, _ = db.ExecContext(ctx, `DROP FUNCTION IF EXISTS fail_repair_publish()`)
	}()
	sawFailure := false
	var checkpoint CreditRepairStatus
	for i := 0; i < 256 && status.Phase != "completed"; i++ {
		checkpoint = status
		failed, callErr := store.AdvanceCreditRepair(ctx, req.Account, req.ID, status.Revision, 10)
		if callErr != nil {
			sawFailure = true
			if failed != (CreditRepairStatus{}) {
				t.Fatalf("failed publication returned status=%+v err=%v", failed, callErr)
			}
			break
		}
		status = failed
	}
	if !sawFailure {
		t.Fatal("publication trigger did not force a commit failure")
	}
	recovered, err := store.CreditRepair(ctx, req.Account, req.ID)
	if err != nil {
		t.Fatalf("read publication checkpoint after failed commit: %v", err)
	}
	if !sameCreditRepairStatus(recovered, checkpoint) {
		t.Fatalf("failed publication changed checkpoint: before=%+v after=%+v", checkpoint, recovered)
	}
	var stillReady bool
	var stillFenced sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT ready,repair_job_id FROM billing_credit_balance_projection_state WHERE account_id=$1`, req.Account).Scan(&stillReady, &stillFenced); err != nil {
		t.Fatal(err)
	}
	if stillReady || !stillFenced.Valid || stillFenced.String != req.ID {
		t.Fatalf("failed publication lost fence: ready=%v repair=%v", stillReady, stillFenced)
	}
	status = recovered
	if _, err := db.ExecContext(ctx, `DROP TRIGGER fail_repair_publish ON billing_credit_repair_jobs`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DROP FUNCTION fail_repair_publish()`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 256 && status.Phase != "completed"; i++ {
		next, retryErr := store.AdvanceCreditRepair(ctx, req.Account, req.ID, status.Revision, 10)
		if retryErr != nil {
			t.Fatal(retryErr)
		}
		status = next
	}
	if status.Phase != "completed" {
		t.Fatalf("publication retry did not complete: %+v", status)
	}
	var ready bool
	var repairID sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT ready,repair_job_id FROM billing_credit_balance_projection_state WHERE account_id=$1`, req.Account).Scan(&ready, &repairID); err != nil {
		t.Fatal(err)
	}
	if !ready || repairID.Valid {
		t.Fatalf("completed repair left projection fenced: ready=%v repair=%v", ready, repairID)
	}
	if _, err := credit.New(store, func() time.Time { return testTime() }).Grant(ctx, credit.GrantInput{Account: req.Account, Operation: "repair-publish-resume", LotID: "repair-publish-resume-lot", Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 1, Source: "repair-test", SourceRef: "resume", ValidFrom: testTime()}); err != nil {
		t.Fatalf("grant after completed repair: %v", err)
	}
}

func TestCreditRepairConcurrentSameRevisionOnlyOneCommits(t *testing.T) {
	store, db, req, started := creditRepairFailureFixture(t, "repair-concurrent", "repair-concurrent")
	secondStore := New(secondQueueDB(t, db))
	ctx := t.Context()
	type result struct {
		status CreditRepairStatus
		err    error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	for _, candidate := range []*Store{store, secondStore} {
		go func(service *Store) {
			<-start
			status, err := service.AdvanceCreditRepair(ctx, req.Account, req.ID, started.Revision, 1)
			results <- result{status, err}
		}(candidate)
	}
	close(start)
	first, secondResult := <-results, <-results
	successes := 0
	conflicts := 0
	for _, got := range []result{first, secondResult} {
		if got.err == nil {
			successes++
		}
		if errors.Is(got.err, billing.ErrConflict) {
			conflicts++
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("same-revision results: first=%+v second=%+v", first, secondResult)
	}
}
