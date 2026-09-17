package pg

import (
	"context"
	"errors"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/integration"
	"github.com/data-insights-ai/rho-billing/usage"
)

func boundSettlementFixture(t *testing.T, store *Store) (usage.Batch, usage.BeginSubmissionInput) {
	t.Helper()
	ctx := t.Context()
	if err := store.CreateAccount(ctx, "bound-settlement", "bound-settlement-subject"); err != nil {
		t.Fatal(err)
	}
	period := settlementTestPeriod()
	record := settlementTestRecord(t, store, "bound-settlement", "usage", period.Start.Add(time.Hour), period.Start.Add(2*time.Hour), 7)
	if _, err := recordUsage(ctx, store, record); err != nil {
		t.Fatal(err)
	}
	batch, err := finishSettlementClose(ctx, store.Settlements(), store.now, usage.CloseInput{Account: "bound-settlement", Operation: "close", BatchID: "batch", Period: period, Currency: "USD", CreatedAt: period.Cutoff})
	if err != nil {
		t.Fatal(err)
	}
	return batch, usage.BeginSubmissionInput{Account: batch.Account, Operation: "begin", BatchID: batch.ID, AttemptID: "attempt", Provider: "sim", IdempotencyKey: "key", Capability: usage.SettlementCapability{SupportsLookup: true}, CreatedAt: period.Cutoff}
}

func TestSettlementSessionRollsBackSubmissionAndCreditTogether(t *testing.T) {
	store, _ := testStore(t)
	batch, begin := boundSettlementFixture(t, store)
	ctx := t.Context()
	failure := errors.New("later host effect failed")
	apply := func(scope integration.Session) error {
		repo := scope.Settlements()
		service := usage.NewSettlement(repo, testTime)
		attempt, err := service.BeginSubmission(ctx, begin)
		if err != nil {
			return err
		}
		// These reads must see the same transaction; root reads would see neither.
		storedAttempt, err := repo.Attempt(ctx, batch.Account, attempt.ID)
		if err != nil || !sameSettlementAttempt(storedAttempt, attempt) {
			t.Fatalf("bound attempt=%+v err=%v", storedAttempt, err)
		}
		storedBatch, err := usage.NewSettlement(repo, testTime).BatchSummary(ctx, batch.Account, batch.ID)
		if err != nil || storedBatch.State != usage.BatchSubmitting {
			t.Fatalf("bound batch=%+v err=%v", storedBatch, err)
		}
		for _, err := range []error{
			repo.WithinAccount(ctx, "other", func(usage.SettlementTx) error { t.Fatal("cross-account callback ran"); return nil }),
			func() error {
				_, e := usage.NewSettlement(repo, testTime).BatchSummary(ctx, "other", batch.ID)
				return e
			}(),
			func() error { _, e := repo.Attempt(ctx, "other", attempt.ID); return e }(),
		} {
			if !errors.Is(err, billing.ErrNotFound) {
				t.Fatalf("scope error=%v", err)
			}
		}
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := usage.NewSettlement(repo, testTime).BatchSummary(canceled, batch.Account, batch.ID); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled read=%v", err)
		}
		if err := repo.WithinAccount(canceled, batch.Account, func(usage.SettlementTx) error { t.Fatal("canceled callback ran"); return nil }); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled transaction=%v", err)
		}
		_, err = credit.New(scope.Credits(), testTime).Grant(ctx, credit.GrantInput{Account: batch.Account, Operation: "host-grant", LotID: "host-lot", Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 3, Source: "manual", SourceRef: "host", ValidFrom: testTime()})
		return err
	}
	if err := store.Atomic(ctx, batch.Account, func(scope integration.Session) error {
		if err := apply(scope); err != nil {
			return err
		}
		return failure
	}); !errors.Is(err, failure) {
		t.Fatalf("outer rollback=%v", err)
	}
	if _, err := store.Settlements().Attempt(ctx, batch.Account, begin.AttemptID); !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("attempt survived rollback: %v", err)
	}
	stored, err := usage.NewSettlement(store.Settlements(), store.now).BatchSummary(ctx, batch.Account, batch.ID)
	if err != nil || stored.State != usage.BatchReady {
		t.Fatalf("rollback batch=%+v err=%v", stored, err)
	}
	balance, err := credit.New(store, testTime).Balance(ctx, batch.Account, "credits", "")
	if err != nil || balance.Available != 0 {
		t.Fatalf("rollback balance=%+v err=%v", balance, err)
	}
	if err := store.Atomic(ctx, batch.Account, apply); err != nil {
		t.Fatal(err)
	}
	if err := store.Atomic(ctx, batch.Account, apply); err != nil {
		t.Fatal(err)
	}
	balance, err = credit.New(store, testTime).Balance(ctx, batch.Account, "credits", "")
	if err != nil || balance.Available != 3 {
		t.Fatalf("replayed balance=%+v err=%v", balance, err)
	}
}

func TestSettlementSessionRejectionAndCompletionEvidenceFailure(t *testing.T) {
	store, db := testStore(t)
	batch, begin := boundSettlementFixture(t, store)
	ctx := t.Context()
	root := usage.NewSettlement(store.Settlements(), testTime)
	attempt, err := root.BeginSubmission(ctx, begin)
	if err != nil {
		t.Fatal(err)
	}
	completion := usage.CompleteSubmissionInput{Account: batch.Account, Operation: "complete", BatchID: batch.ID, AttemptID: attempt.ID, ExpectedRevision: attempt.Revision, Status: usage.SubmissionConfirmed, ProviderReference: "charge", CompletedAt: batch.Period.Cutoff}
	rejected := completion
	rejected.Operation, rejected.ExpectedRevision = "stale", 99
	if err := store.Atomic(ctx, batch.Account, func(scope integration.Session) error {
		_, err := usage.NewSettlement(scope.Settlements(), testTime).CompleteSubmission(ctx, rejected)
		if _, recorded := errors.AsType[*usage.SettlementRejection](err); !recorded || !errors.Is(err, usage.ErrStaleRevision) {
			t.Fatalf("rejection=%v", err)
		}
		return nil // Explicitly commit the recorded terminal rejection.
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := root.CompleteSubmission(ctx, rejected); !errors.Is(err, usage.ErrStaleRevision) {
		t.Fatalf("replayed rejection=%v", err)
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM billing_settlement_operations WHERE account_id=$1 AND operation_id='stale'`, batch.Account).Scan(&count); err != nil || count != 1 {
		t.Fatalf("rejection count=%d err=%v", count, err)
	}
	if _, err := db.ExecContext(ctx, `CREATE FUNCTION fail_settlement_outcome() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected settlement outcome failure'; END $$`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TRIGGER fail_settlement_outcome BEFORE INSERT ON billing_settlement_operations FOR EACH ROW EXECUTE FUNCTION fail_settlement_outcome()`); err != nil {
		t.Fatal(err)
	}
	err = store.Atomic(ctx, batch.Account, func(scope integration.Session) error {
		result, err := usage.NewSettlement(scope.Settlements(), testTime).CompleteSubmission(ctx, completion)
		if err == nil || result.ID != "" {
			t.Fatalf("storage failure result=%+v err=%v", result, err)
		}
		if _, recorded := errors.AsType[*usage.SettlementRejection](err); recorded {
			t.Fatal("storage failure labeled recorded rejection")
		}
		return err
	})
	if err == nil {
		t.Fatal("expected evidence insertion failure")
	}
	stored, err := readPagedSettlementBatch(ctx, store.Settlements(), store.now, string(batch.Account), batch.ID)
	if err != nil || stored.State != usage.BatchSubmitting || stored.Revision != attempt.Revision {
		t.Fatalf("failed completion batch=%+v err=%v", stored, err)
	}
	storedAttempt, err := root.Attempt(ctx, batch.Account, attempt.ID)
	if err != nil || !sameSettlementAttempt(storedAttempt, attempt) {
		t.Fatalf("failed completion attempt=%+v err=%v", storedAttempt, err)
	}
	if _, err := db.ExecContext(ctx, `DROP TRIGGER fail_settlement_outcome ON billing_settlement_operations`); err != nil {
		t.Fatal(err)
	}
	message := queueMessage(batch.Account, "provider-confirmation", integration.Inbound, "normalized settlement confirmation")
	if err := store.Receive(ctx, message); err != nil {
		t.Fatal(err)
	}
	claim, found, err := store.Claim(ctx, integration.Inbound, "worker", store.now(), time.Minute)
	if err != nil || !found {
		t.Fatalf("inbox claim found=%v err=%v", found, err)
	}
	if err := store.ProcessInbox(ctx, claim, func(scope integration.Session) error {
		result, err := usage.NewSettlement(scope.Settlements(), testTime).CompleteSubmission(ctx, completion)
		if err == nil && result.State != usage.BatchConfirmed {
			t.Fatalf("completion=%+v", result)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	delivery, err := store.Delivery(ctx, message)
	if err != nil || delivery.State != "processed" {
		t.Fatalf("inbox delivery=%+v err=%v", delivery, err)
	}
	if err := store.ProcessInbox(ctx, claim, func(integration.Session) error { t.Fatal("processed inbox replay ran callback"); return nil }); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("completed claim should be fenced: %v", err)
	}
	if result, err := root.CompleteSubmission(ctx, completion); err != nil || result.State != usage.BatchConfirmed {
		t.Fatalf("replayed completion=%+v err=%v", result, err)
	}
}

func TestFinalizeStorageConflictRollsBackClaimsAndBatch(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("close-storage-conflict")
	if err := store.CreateAccount(ctx, account, "close-storage-conflict-subject"); err != nil {
		t.Fatal(err)
	}
	period := settlementTestPeriod()
	record := settlementTestRecord(t, store, string(account), "usage", period.Start.Add(time.Hour), period.Start.Add(2*time.Hour), 7)
	if _, err := recordUsage(ctx, store, record); err != nil {
		t.Fatal(err)
	}
	// Cause a storage consistency conflict after usage claims and batch insertion,
	// without aborting the SQL transaction. Broad sentinel matching must not commit it.
	if _, err := db.ExecContext(ctx, `CREATE FUNCTION corrupt_settlement_source() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN UPDATE billing_usage SET source='corrupted' WHERE account_id=NEW.account_id; RETURN NEW; END $$`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TRIGGER corrupt_settlement_source AFTER INSERT ON billing_settlement_lines FOR EACH ROW EXECUTE FUNCTION corrupt_settlement_source()`); err != nil {
		t.Fatal(err)
	}
	input := usage.CloseInput{Account: account, Operation: "close", BatchID: "batch", Period: period, Currency: "USD", CreatedAt: period.Cutoff}
	batch, err := finishSettlementClose(ctx, store.Settlements(), store.now, input)
	if !errors.Is(err, billing.ErrConflict) || batch.ID != "" {
		t.Fatalf("storage conflict batch=%+v err=%v", batch, err)
	}
	if _, recorded := errors.AsType[*usage.SettlementRejection](err); recorded {
		t.Fatal("unrecorded storage conflict labeled rejection")
	}
	var claims, batches, commands int
	if err := db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM billing_settlement_usage_claims WHERE account_id=$1), (SELECT count(*) FROM billing_settlement_batches WHERE account_id=$1), (SELECT count(*) FROM billing_settlement_operations WHERE account_id=$1)`, account).Scan(&claims, &batches, &commands); err != nil {
		t.Fatal(err)
	}
	if claims != 0 || batches != 0 || commands != 0 {
		t.Fatalf("storage conflict committed partial close: claims=%d batches=%d commands=%d", claims, batches, commands)
	}
	if _, err := db.ExecContext(ctx, `DROP TRIGGER corrupt_settlement_source ON billing_settlement_lines`); err != nil {
		t.Fatal(err)
	}
	batch, err = finishSettlementClose(ctx, store.Settlements(), store.now, input)
	if err != nil || batch.Total != 7 {
		t.Fatalf("retry batch=%+v err=%v", batch, err)
	}
}

func TestSettlementSessionFinalizesNewUsageInSameTransaction(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("bound-close")
	if err := store.CreateAccount(ctx, account, "bound-close-subject"); err != nil {
		t.Fatal(err)
	}
	period := settlementTestPeriod()
	record := settlementTestRecord(t, store, string(account), "usage", period.Start.Add(time.Hour), period.Start.Add(2*time.Hour), 5)
	input := usage.CloseInput{Account: account, Operation: "close", BatchID: "batch", Period: period, Currency: "USD", CreatedAt: period.Cutoff}
	apply := func(scope integration.Session) error {
		if _, err := usage.New(scope.Usage(), nil).Record(ctx, record); err != nil {
			return err
		}
		batch, err := finishSettlementClose(ctx, scope.Settlements(), testTime, input)
		if err != nil {
			return err
		}
		if batch.Total != 5 || len(batch.Lines) != 1 {
			t.Fatalf("bound close=%+v", batch)
		}
		return nil
	}
	failure := errors.New("host rejected transaction")
	if err := store.Atomic(ctx, account, func(scope integration.Session) error {
		if err := apply(scope); err != nil {
			return err
		}
		return failure
	}); !errors.Is(err, failure) {
		t.Fatalf("outer failure=%v", err)
	}
	var records, batches, claims, operations int
	if err := db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM billing_usage WHERE account_id=$1),(SELECT count(*) FROM billing_settlement_batches WHERE account_id=$1),(SELECT count(*) FROM billing_settlement_usage_claims WHERE account_id=$1),(SELECT count(*) FROM billing_settlement_operations WHERE account_id=$1)`, account).Scan(&records, &batches, &claims, &operations); err != nil {
		t.Fatal(err)
	}
	if records+batches+claims+operations != 0 {
		t.Fatalf("rollback leaked records=%d batches=%d claims=%d operations=%d", records, batches, claims, operations)
	}
	if err := store.Atomic(ctx, account, apply); err != nil {
		t.Fatal(err)
	}
	if err := store.Atomic(ctx, account, apply); err != nil {
		t.Fatal(err)
	}
}
