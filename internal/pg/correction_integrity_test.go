package pg

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/usage"
)

type correctionUsageQueryCounter struct {
	usageQueries atomic.Int64
	statements   atomic.Int64
}

func (c *correctionUsageQueryCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	c.statements.Add(1)
	if strings.Contains(strings.ToLower(data.SQL), "from billing_usage") {
		c.usageQueries.Add(1)
	}
	return ctx
}
func (*correctionUsageQueryCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

func TestPostgresCorrectionRollsBackWhenSourceChangesDuringLineInsert(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	const account = billing.AccountID("correction-integrity")
	if err := store.CreateAccount(ctx, account, "correction-integrity-subject"); err != nil {
		t.Fatal(err)
	}
	period := settlementTestPeriod()
	originalRecord := settlementTestRecord(t, store, string(account), "integrity-original", period.Start.Add(time.Hour), period.Start.Add(2*time.Hour), 5)
	if _, err := recordUsage(ctx, store, originalRecord); err != nil {
		t.Fatal(err)
	}
	original, err := finishSettlementClose(ctx, store.Settlements(), store.now, usage.CloseInput{Account: account, Operation: "integrity-close", BatchID: "integrity-original-batch", Period: period, Currency: "USD", CreatedAt: period.Cutoff})
	if err != nil {
		t.Fatal(err)
	}
	late := settlementTestRecord(t, store, string(account), "integrity-late", period.Start.Add(2*time.Hour), period.Cutoff.Add(time.Hour), 3)
	if _, err := recordUsage(ctx, store, late); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		CREATE FUNCTION corrupt_integrity_source() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			UPDATE billing_usage SET fingerprint='corrupted-after-line' WHERE account_id=NEW.account_id AND usage_id=NEW.usage_id;
			RETURN NEW;
		END $$;
		CREATE TRIGGER corrupt_integrity_source AFTER INSERT ON billing_settlement_lines
		FOR EACH ROW WHEN (NEW.usage_id <> '') EXECUTE FUNCTION corrupt_integrity_source()`); err != nil {
		t.Fatal(err)
	}
	repo := usage.NewSettlement(store.Settlements(), store.now)
	_, err = repo.Correct(ctx, usage.CorrectionInput{Account: account, Operation: "integrity-correction", BatchID: "integrity-correction-batch", OriginalBatchID: original.ID, Period: period, Currency: "USD", UsageIDs: []string{late.Observation.ID}, Adjustments: []usage.Adjustment{{ID: "integrity-adjustment", OriginalUsageID: originalRecord.Observation.ID, Amount: -1, Currency: "USD", Reason: "integrity test"}}, CreatedAt: period.Cutoff.Add(time.Hour)})
	if err == nil {
		t.Fatal("correction unexpectedly succeeded after source corruption")
	}
	var batches, lines, claims, outcomes int
	if err := db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM billing_settlement_batches WHERE account_id=$1 AND batch_id='integrity-correction-batch'),(SELECT count(*) FROM billing_settlement_lines WHERE account_id=$1 AND batch_id='integrity-correction-batch'),(SELECT count(*) FROM billing_settlement_usage_claims WHERE account_id=$1 AND batch_id='integrity-correction-batch'),(SELECT count(*) FROM billing_settlement_operations WHERE account_id=$1 AND operation_id='integrity-correction')`, account).Scan(&batches, &lines, &claims, &outcomes); err != nil {
		t.Fatal(err)
	}
	if batches != 0 || lines != 0 || claims != 0 || outcomes != 0 {
		t.Fatalf("failed correction left durable rows: batches=%d lines=%d claims=%d outcomes=%d", batches, lines, claims, outcomes)
	}
	var fingerprint string
	if err := db.QueryRowContext(ctx, `SELECT fingerprint FROM billing_usage WHERE account_id=$1 AND usage_id=$2`, account, late.Observation.ID).Scan(&fingerprint); err != nil {
		t.Fatal(err)
	}
	if fingerprint != late.Fingerprint {
		t.Fatalf("source fingerprint changed despite rollback: %q", fingerprint)
	}
	if _, err := usage.NewSettlement(store.Settlements(), store.now).BatchSummary(ctx, account, original.ID); err != nil && !errors.Is(err, billing.ErrNotFound) {
		t.Fatal(err)
	}
}

func TestPostgresCorrectionRejectsDuplicateDurableAndPendingClaims(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	const account = billing.AccountID("correction-duplicate-claims")
	if err := store.CreateAccount(ctx, account, "correction-duplicate-claims-subject"); err != nil {
		t.Fatal(err)
	}
	period := settlementTestPeriod()
	originalRecord := settlementTestRecord(t, store, string(account), "duplicate-original", period.Start.Add(time.Hour), period.Start.Add(2*time.Hour), 5)
	if _, err := recordUsage(ctx, store, originalRecord); err != nil {
		t.Fatal(err)
	}
	original, err := finishSettlementClose(ctx, store.Settlements(), store.now, usage.CloseInput{Account: account, Operation: "duplicate-close", BatchID: "duplicate-original-batch", Period: period, Currency: "USD", CreatedAt: period.Cutoff})
	if err != nil {
		t.Fatal(err)
	}
	late := settlementTestRecord(t, store, string(account), "duplicate-late", period.Start.Add(2*time.Hour), period.Cutoff.Add(time.Hour), 3)
	if _, err := recordUsage(ctx, store, late); err != nil {
		t.Fatal(err)
	}
	jobPeriod := period
	jobPeriod.Start = period.End.Add(time.Hour)
	jobPeriod.End = jobPeriod.Start.Add(24 * time.Hour)
	jobPeriod.Cutoff = jobPeriod.End
	job, err := usage.NewSettlement(store.Settlements(), store.now).StartClose(ctx, usage.CloseInput{Account: account, Operation: "duplicate-pending-job", BatchID: "duplicate-pending-job", Period: jobPeriod, Currency: "USD", CreatedAt: jobPeriod.Cutoff})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO billing_settlement_usage_claims(account_id,usage_id,batch_id,source_fingerprint) VALUES($1,$2,$3,$4)`, account, late.Observation.ID, original.ID, late.Fingerprint); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO billing_settlement_pending_claims(account_id,usage_id,job_id,source_fingerprint,reserved_at) VALUES($1,$2,$3,$4,$5)`, account, late.Observation.ID, job.BatchID, late.Fingerprint, period.Cutoff); err != nil {
		t.Fatal(err)
	}
	_, err = usage.NewSettlement(store.Settlements(), store.now).Correct(ctx, usage.CorrectionInput{Account: account, Operation: "duplicate-correction", BatchID: "duplicate-correction-batch", OriginalBatchID: original.ID, Period: period, Currency: "USD", UsageIDs: []string{late.Observation.ID}, CreatedAt: period.Cutoff.Add(time.Hour)})
	if !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("duplicate durable/pending claims error=%v, want conflict", err)
	}
}

func TestPostgresCorrectionUsageEvidenceReadIsSetBased(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	const account = billing.AccountID("correction-query-count")
	if err := store.CreateAccount(ctx, account, "correction-query-count-subject"); err != nil {
		t.Fatal(err)
	}
	period := settlementTestPeriod()
	original, err := finishSettlementClose(ctx, store.Settlements(), store.now, usage.CloseInput{Account: account, Operation: "query-close", BatchID: "query-original", Period: period, Currency: "USD", CreatedAt: period.Cutoff})
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 1001)
	for i := range ids {
		ids[i] = "query-usage-" + fmt.Sprintf("%04d", i)
		record := settlementTestRecord(t, store, string(account), ids[i], period.Start.Add(time.Hour), period.Cutoff.Add(time.Hour), 1)
		if _, err := recordUsage(ctx, store, record); err != nil {
			t.Fatal(err)
		}
	}
	var schema string
	if err := db.QueryRowContext(ctx, "SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	config, err := pgx.ParseConfig(os.Getenv("BILLING_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	config.RuntimeParams["search_path"] = schema
	counter := new(correctionUsageQueryCounter)
	config.Tracer = counter
	traced := New(stdlib.OpenDB(*config))
	t.Cleanup(func() { _ = traced.db.Close() })
	var baseline int64
	for i, selected := range [][]string{ids[:1], ids[1:]} {
		counter.statements.Store(0)
		counter.usageQueries.Store(0)
		id := fmt.Sprintf("query-correction-%d", i)
		out, err := usage.NewSettlement(traced.Settlements(), store.now).Correct(ctx, usage.CorrectionInput{Account: account, Operation: billing.OperationID(id), BatchID: id, OriginalBatchID: original.ID, Period: period, Currency: "USD", UsageIDs: selected})
		if err != nil || out.LineCount != int64(len(selected)) || out.Total != int64(len(selected)) {
			t.Fatalf("correction=%+v err=%v", out, err)
		}
		count := counter.statements.Load()
		if i == 0 {
			baseline = count
		} else if count != baseline {
			t.Fatalf("SQL statements grew from %d for 1 line to %d for 1000 lines", baseline, count)
		}
		if counter.usageQueries.Load() != 2 {
			t.Fatalf("expected authoritative lookup and postwrite verification, got %d", counter.usageQueries.Load())
		}
	}
}
