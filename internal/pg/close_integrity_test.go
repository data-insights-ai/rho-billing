package pg

import (
	"errors"
	"fmt"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/usage"
)

func closeTestInput(account billing.AccountID, operation, batch string) usage.CloseInput {
	period := settlementTestPeriod()
	return usage.CloseInput{Account: account, Operation: billing.OperationID(operation), BatchID: batch, Period: period, Currency: "USD", CreatedAt: period.Cutoff}
}

func TestPostgresStartCloseDeferredCommitRollbackAndRetry(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("close-start-commit")
	if err := store.CreateAccount(ctx, account, "close-start-commit-subject"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
CREATE FUNCTION reject_close_start_commit() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'deliberate close start commit failure'; END $$;
CREATE CONSTRAINT TRIGGER close_start_commit_failure AFTER INSERT ON billing_settlement_close_jobs
DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION reject_close_start_commit();`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = db.ExecContext(ctx, `DROP TRIGGER IF EXISTS close_start_commit_failure ON billing_settlement_close_jobs`)
		_, _ = db.ExecContext(ctx, `DROP FUNCTION IF EXISTS reject_close_start_commit()`)
	}()
	service := usage.NewSettlement(store.Settlements(), func() time.Time { return settlementTestPeriod().Cutoff })
	in := closeTestInput(account, "close-start-commit-op", "close-start-commit-batch")
	job, err := service.StartClose(ctx, in)
	if err == nil || job.BatchID != "" {
		t.Fatalf("failed start result=%+v err=%v", job, err)
	}
	var jobs int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM billing_settlement_close_jobs WHERE account_id=$1`, account).Scan(&jobs); err != nil || jobs != 0 {
		t.Fatalf("failed start left %d close jobs: %v", jobs, err)
	}
	if _, err := db.ExecContext(ctx, `DROP TRIGGER close_start_commit_failure ON billing_settlement_close_jobs`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DROP FUNCTION reject_close_start_commit()`); err != nil {
		t.Fatal(err)
	}
	retry, err := service.StartClose(ctx, in)
	if err != nil || retry.BatchID != in.BatchID || retry.State != usage.ClosePreparing {
		t.Fatalf("retry start=%+v err=%v", retry, err)
	}
}

func TestPostgresAdvanceCloseDeferredCommitRollbackAndRetry(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("close-advance-commit")
	if err := store.CreateAccount(ctx, account, "close-advance-commit-subject"); err != nil {
		t.Fatal(err)
	}
	period := settlementTestPeriod()
	record := settlementTestRecord(t, store, string(account), "close-advance-usage", period.Start.Add(time.Hour), period.Start.Add(2*time.Hour), 1)
	if _, err := recordUsage(ctx, store, record); err != nil {
		t.Fatal(err)
	}
	service := usage.NewSettlement(store.Settlements(), func() time.Time { return period.Cutoff })
	in := closeTestInput(account, "close-advance-commit-op", "close-advance-commit-batch")
	started, err := service.StartClose(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
CREATE FUNCTION reject_close_advance_commit() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'deliberate close advance commit failure'; END $$;
CREATE CONSTRAINT TRIGGER close_advance_commit_failure AFTER INSERT ON billing_settlement_batches
DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION reject_close_advance_commit();`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = db.ExecContext(ctx, `DROP TRIGGER IF EXISTS close_advance_commit_failure ON billing_settlement_batches`)
		_, _ = db.ExecContext(ctx, `DROP FUNCTION IF EXISTS reject_close_advance_commit()`)
	}()
	job, err := service.AdvanceClose(ctx, account, started.BatchID, started.Revision, 100)
	if err == nil || job.BatchID != "" {
		t.Fatalf("failed advance result=%+v err=%v", job, err)
	}
	var batches, lines, claims int
	if err := db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM billing_settlement_batches WHERE account_id=$1),(SELECT count(*) FROM billing_settlement_lines WHERE account_id=$1),(SELECT count(*) FROM billing_settlement_pending_claims WHERE account_id=$1)`, account).Scan(&batches, &lines, &claims); err != nil {
		t.Fatal(err)
	}
	if batches != 0 || lines != 0 || claims != 0 {
		t.Fatalf("failed advance leaked staged state batches=%d lines=%d claims=%d", batches, lines, claims)
	}
	stored, err := service.CloseJob(ctx, account, started.BatchID)
	if err != nil || stored.Revision != started.Revision || stored.State != usage.ClosePreparing {
		t.Fatalf("failed advance changed job=%+v err=%v", stored, err)
	}
	if _, err := db.ExecContext(ctx, `DROP TRIGGER close_advance_commit_failure ON billing_settlement_batches`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DROP FUNCTION reject_close_advance_commit()`); err != nil {
		t.Fatal(err)
	}
	retry, err := service.AdvanceClose(ctx, account, started.BatchID, started.Revision, 100)
	if err != nil || retry.State != usage.CloseReady || retry.Processed != 1 {
		t.Fatalf("retry advance=%+v err=%v", retry, err)
	}
}

func TestPostgresPublishCloseDeferredCommitRollbackAndRetry(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("close-publish-commit")
	if err := store.CreateAccount(ctx, account, "close-publish-commit-subject"); err != nil {
		t.Fatal(err)
	}
	period := settlementTestPeriod()
	record := settlementTestRecord(t, store, string(account), "close-publish-usage", period.Start.Add(time.Hour), period.Start.Add(2*time.Hour), 1)
	if _, err := recordUsage(ctx, store, record); err != nil {
		t.Fatal(err)
	}
	service := usage.NewSettlement(store.Settlements(), func() time.Time { return period.Cutoff })
	in := closeTestInput(account, "close-publish-commit-op", "close-publish-commit-batch")
	started, err := service.StartClose(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	ready, err := service.AdvanceClose(ctx, account, started.BatchID, started.Revision, 100)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
CREATE FUNCTION reject_close_publish_commit() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'deliberate close publish commit failure'; END $$;
CREATE CONSTRAINT TRIGGER close_publish_commit_failure AFTER UPDATE OF state ON billing_settlement_batches
DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN (NEW.state = 'ready') EXECUTE FUNCTION reject_close_publish_commit();`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = db.ExecContext(ctx, `DROP TRIGGER IF EXISTS close_publish_commit_failure ON billing_settlement_batches`)
		_, _ = db.ExecContext(ctx, `DROP FUNCTION IF EXISTS reject_close_publish_commit()`)
	}()
	published, err := service.PublishClose(ctx, account, ready.BatchID, ready.Revision)
	if err == nil || published.BatchID != "" {
		t.Fatalf("failed publish result=%+v err=%v", published, err)
	}
	summary, err := service.BatchSummary(ctx, account, ready.BatchID)
	if err == nil || summary.ID != "" {
		t.Fatalf("failed publish exposed summary=%+v err=%v", summary, err)
	}
	if _, err := db.ExecContext(ctx, `DROP TRIGGER close_publish_commit_failure ON billing_settlement_batches`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DROP FUNCTION reject_close_publish_commit()`); err != nil {
		t.Fatal(err)
	}
	retry, err := service.PublishClose(ctx, account, ready.BatchID, ready.Revision)
	if err != nil || retry.State != usage.ClosePublished {
		t.Fatalf("retry publish=%+v err=%v", retry, err)
	}
}

func TestPostgresStagedCloseIsHiddenFromSubmissionAndReads(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("close-hidden-staged")
	if err := store.CreateAccount(ctx, account, "close-hidden-staged-subject"); err != nil {
		t.Fatal(err)
	}
	period := settlementTestPeriod()
	record := settlementTestRecord(t, store, string(account), "close-hidden-usage", period.Start.Add(time.Hour), period.Start.Add(2*time.Hour), 1)
	if _, err := recordUsage(ctx, store, record); err != nil {
		t.Fatal(err)
	}
	service := usage.NewSettlement(store.Settlements(), func() time.Time { return period.Cutoff })
	in := closeTestInput(account, "close-hidden-op", "close-hidden-batch")
	started, err := service.StartClose(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	ready, err := service.AdvanceClose(ctx, account, started.BatchID, started.Revision, 100)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.BeginSubmission(ctx, usage.BeginSubmissionInput{Account: account, Operation: "hidden-submit-op", BatchID: ready.BatchID, AttemptID: "hidden-attempt", Provider: "provider", IdempotencyKey: "hidden-idempotency", Capability: usage.SettlementCapability{SupportsIdempotency: true}, CreatedAt: period.Cutoff})
	if !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("staged BeginSubmission error=%v, want not found", err)
	}
	if _, err := service.BatchSummary(ctx, account, ready.BatchID); !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("staged BatchSummary error=%v, want not found", err)
	}
	if _, _, _, err := service.BatchLinesPage(ctx, account, ready.BatchID, "", 100); !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("staged BatchLinesPage error=%v, want not found", err)
	}
}

func seedCloseUsage(t *testing.T, store *Store, account billing.AccountID, ruleVersion string, count int, at time.Time) {
	t.Helper()
	config := usage.RuleConfig{Version: ruleVersion, Kind: usage.KindFixed, Target: usage.Target{Currency: "USD"}, Rounding: usage.RoundDown, FixedRate: "1"}
	if err := store.PublishRating(t.Context(), config); err != nil {
		t.Fatal(err)
	}
	service := usage.New(store.UsageRepository(), func() time.Time { return at })
	for i := range count {
		id := fmt.Sprintf("%s-%04d", ruleVersion, i)
		if _, err := service.RateAndRecord(t.Context(), usage.Observation{Account: account, ID: id, Source: "close-meter", OccurredAt: at, Funding: usage.Postpaid, Input: usage.RateInput{ActionCount: 1}}, ruleVersion); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPostgresCloseChunksBoundedAndFreezesWatermark(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("close-chunks-bounded")
	if err := store.CreateAccount(ctx, account, "close-chunks-bounded-subject"); err != nil {
		t.Fatal(err)
	}
	period := settlementTestPeriod()
	at := period.Start.Add(time.Hour)
	const count = 1001
	seedCloseUsage(t, store, account, "close-chunks-rule", count, at)
	service := usage.NewSettlement(store.Settlements(), func() time.Time { return at })
	input := closeTestInput(account, "close-chunks-op", "close-chunks-batch")
	job, err := service.StartClose(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	lateRule := usage.RuleConfig{Version: "close-chunks-late-rule", Kind: usage.KindFixed, Target: usage.Target{Currency: "USD"}, Rounding: usage.RoundDown, FixedRate: "1"}
	if err := store.PublishRating(ctx, lateRule); err != nil {
		t.Fatal(err)
	}
	if _, err := usage.New(store.UsageRepository(), func() time.Time { return at }).RateAndRecord(ctx, usage.Observation{Account: account, ID: "close-chunks-late", Source: "close-meter", OccurredAt: at, Funding: usage.Postpaid, Input: usage.RateInput{ActionCount: 1}}, lateRule.Version); err != nil {
		t.Fatal(err)
	}
	var previous int64
	for job.State == usage.ClosePreparing {
		before := job
		job, err = usage.NewSettlement(store.Settlements(), func() time.Time { return at }).AdvanceClose(ctx, account, job.BatchID, job.Revision, 127)
		if err != nil {
			t.Fatal(err)
		}
		delta := job.Processed - previous
		if delta < 0 || delta > 127 || job.Cursor <= before.Cursor || job.Revision != before.Revision+1 {
			t.Fatalf("unbounded close step before=%+v after=%+v delta=%d", before, job, delta)
		}
		previous = job.Processed
	}
	if job.State != usage.CloseReady || job.Processed != count {
		t.Fatalf("chunked close ready=%+v, want processed=%d", job, count)
	}
	published, err := usage.NewSettlement(store.Settlements(), func() time.Time { return at }).PublishClose(ctx, account, job.BatchID, job.Revision)
	if err != nil || published.State != usage.ClosePublished {
		t.Fatalf("published close=%+v err=%v", published, err)
	}
	summary, err := usage.NewSettlement(store.Settlements(), func() time.Time { return at }).BatchSummary(ctx, account, job.BatchID)
	if err != nil || summary.LineCount != count || summary.Total != count {
		t.Fatalf("published summary=%+v err=%v", summary, err)
	}
	var lateClaims int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM billing_settlement_pending_claims WHERE account_id=$1 AND usage_id='close-chunks-late'`, account).Scan(&lateClaims); err != nil || lateClaims != 0 {
		t.Fatalf("late usage claim count=%d err=%v", lateClaims, err)
	}
	t.Logf("processed %d usage rows in bounded chunks of 127", job.Processed)
}

func TestPostgresCancelCloseCleansBoundedPagesAndReusesKey(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("close-cancel-bounded")
	if err := store.CreateAccount(ctx, account, "close-cancel-bounded-subject"); err != nil {
		t.Fatal(err)
	}
	period := settlementTestPeriod()
	at := period.Start.Add(time.Hour)
	const count = 300
	seedCloseUsage(t, store, account, "close-cancel-rule", count, at)
	service := usage.NewSettlement(store.Settlements(), func() time.Time { return at })
	input := closeTestInput(account, "close-cancel-op", "close-cancel-batch")
	started, err := service.StartClose(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	advanced, err := service.AdvanceClose(ctx, account, started.BatchID, started.Revision, 127)
	if err != nil || advanced.Processed != 127 {
		t.Fatalf("initial cancel advance=%+v err=%v", advanced, err)
	}
	canceling, err := service.CancelClose(ctx, account, advanced.BatchID, advanced.Revision)
	if err != nil || canceling.State != usage.CloseCanceling {
		t.Fatalf("cancel start=%+v err=%v", canceling, err)
	}
	job := canceling
	for job.State == usage.CloseCanceling {
		var beforeLines, beforeClaims int
		if err := db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM billing_settlement_lines WHERE account_id=$1 AND batch_id=$2),(SELECT count(*) FROM billing_settlement_pending_claims WHERE account_id=$1 AND job_id=$2)`, account, job.BatchID).Scan(&beforeLines, &beforeClaims); err != nil {
			t.Fatal(err)
		}
		next, err := usage.NewSettlement(store.Settlements(), func() time.Time { return at }).AdvanceClose(ctx, account, job.BatchID, job.Revision, 127)
		if err != nil {
			t.Fatal(err)
		}
		var afterLines, afterClaims int
		if err := db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM billing_settlement_lines WHERE account_id=$1 AND batch_id=$2),(SELECT count(*) FROM billing_settlement_pending_claims WHERE account_id=$1 AND job_id=$2)`, account, job.BatchID).Scan(&afterLines, &afterClaims); err != nil {
			t.Fatal(err)
		}
		if beforeLines-afterLines > 127 || beforeClaims-afterClaims > 127 {
			t.Fatalf("cancel cleanup exceeded page before lines/claims=%d/%d after=%d/%d", beforeLines, beforeClaims, afterLines, afterClaims)
		}
		job = next
	}
	if job.State != usage.CloseCanceled {
		t.Fatalf("cancel did not finish: %+v", job)
	}
	if _, err := service.BatchSummary(ctx, account, job.BatchID); !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("canceled summary error=%v, want not found", err)
	}
	var lines, claims, nonlinear int
	if err := db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM billing_settlement_lines WHERE account_id=$1 AND batch_id=$2),(SELECT count(*) FROM billing_settlement_pending_claims WHERE account_id=$1 AND job_id=$2),(SELECT count(*) FROM billing_settlement_close_nonlinear WHERE account_id=$1 AND job_id=$2)`, account, job.BatchID).Scan(&lines, &claims, &nonlinear); err != nil {
		t.Fatal(err)
	}
	if lines != 0 || claims != 0 || nonlinear != 0 {
		t.Fatalf("canceled close left lines=%d claims=%d nonlinear=%d", lines, claims, nonlinear)
	}
	reused, err := service.StartClose(ctx, usage.CloseInput{Account: account, Operation: "close-cancel-reuse-op", BatchID: "close-cancel-reuse-batch", Period: period, Currency: "USD", CreatedAt: period.Cutoff})
	if err != nil || reused.BatchID != "close-cancel-reuse-batch" {
		t.Fatalf("reused close key=%+v err=%v", reused, err)
	}
}
