package pg

import (
	"context"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/usage"
)

func finishSettlementClose(ctx context.Context, repo usage.SettlementRepository, now func() time.Time, in usage.CloseInput) (usage.Batch, error) {
	svc := usage.NewSettlement(repo, now)
	job, err := svc.StartClose(ctx, usage.CloseInput(in))
	if err != nil {
		return usage.Batch{}, err
	}
	for job.State == usage.ClosePreparing {
		job, err = svc.AdvanceClose(ctx, in.Account, job.BatchID, job.Revision, 1000)
		if err != nil {
			return usage.Batch{}, err
		}
	}
	if job.State == usage.CloseReady {
		job, err = svc.PublishClose(ctx, in.Account, job.BatchID, job.Revision)
		if err != nil {
			return usage.Batch{}, err
		}
	}
	summary, err := svc.BatchSummary(ctx, in.Account, in.BatchID)
	if err != nil {
		return usage.Batch{}, err
	}
	batch := usage.Batch{Account: summary.Account, ID: summary.ID, OriginalBatchID: summary.OriginalBatchID, Period: summary.Period, Currency: summary.Currency, Total: summary.Total, State: summary.State, Revision: summary.Revision, CreatedAt: summary.CreatedAt, UpdatedAt: summary.UpdatedAt}
	batch.Fingerprint = job.StagedChecksum
	after := ""
	for {
		lines, next, more, e := svc.BatchLinesPage(ctx, in.Account, in.BatchID, after, 1000)
		if e != nil {
			return usage.Batch{}, e
		}
		batch.Lines = append(batch.Lines, lines...)
		if !more {
			break
		}
		after = next
	}
	return batch, nil
}

func readPagedSettlementBatch(ctx context.Context, repo usage.SettlementRepository, now func() time.Time, account, id string) (usage.Batch, error) {
	svc := usage.NewSettlement(repo, now)
	summary, err := svc.BatchSummary(ctx, billing.AccountID(account), id)
	if err != nil {
		return usage.Batch{}, err
	}
	batch := usage.Batch{Account: summary.Account, ID: summary.ID, OriginalBatchID: summary.OriginalBatchID, Period: summary.Period, Currency: summary.Currency, Total: summary.Total, State: summary.State, Revision: summary.Revision, CreatedAt: summary.CreatedAt, UpdatedAt: summary.UpdatedAt}
	after := ""
	for {
		lines, next, more, e := svc.BatchLinesPage(ctx, billing.AccountID(account), id, after, 1000)
		if e != nil {
			return usage.Batch{}, e
		}
		batch.Lines = append(batch.Lines, lines...)
		if !more {
			break
		}
		after = next
	}
	return batch, nil
}

// A session event whose interval straddles the period boundary is accepted at
// ingestion but rejected by the close validator. When the page predicate
// selected it anyway, the whole page aborted, the cursor never advanced, and
// because billing_usage is insert-only the period could never be closed.
func TestClosePageExcludesRowsTheValidatorWouldReject(t *testing.T) {
	ctx := t.Context()
	store, _ := testStore(t)
	createTestAccounts(t, store)
	now := testTime()
	period := usage.BillingPeriod{Start: now.Add(-2 * time.Hour), End: now, Cutoff: now}
	config := usage.RuleConfig{Version: "straddle-rate", Kind: usage.KindFixed, Target: usage.Target{Currency: "USD"}, Rounding: usage.RoundDown, FixedRate: "1"}
	if err := store.PublishRating(ctx, config); err != nil {
		t.Fatal(err)
	}
	rule, err := usage.NewRule(config)
	if err != nil {
		t.Fatal(err)
	}
	inside, err := usage.Prepare(usage.Observation{Account: "acct-a", ID: "inside", Source: "meter", OccurredAt: period.Start.Add(time.Minute), Funding: usage.Postpaid, Input: usage.RateInput{ActionCount: 1}}, rule, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := recordUsage(ctx, store, inside); err != nil {
		t.Fatal(err)
	}
	// Occurs inside the period, but its interval runs past the boundary.
	straddle, err := usage.Prepare(usage.Observation{
		Account: "acct-a", ID: "straddle", Source: "meter", OccurredAt: period.End.Add(-10 * time.Minute),
		Interval: billing.Period{Start: period.End.Add(-time.Hour), End: period.End.Add(time.Hour)},
		Funding:  usage.Postpaid, Input: usage.RateInput{ActionCount: 1},
	}, rule, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := recordUsage(ctx, store, straddle); err != nil {
		t.Fatalf("ingestion rejected the straddling row: %v", err)
	}
	batch, err := finishSettlementClose(ctx, store.Settlements(), testTime, usage.CloseInput{
		Account: "acct-a", Operation: "straddle-close", BatchID: "straddle-batch", Period: period, Currency: "USD", CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("close wedged on a row the validator rejects: %v", err)
	}
	if len(batch.Lines) != 1 {
		t.Fatalf("batch lines=%d, want only the contained row", len(batch.Lines))
	}
}
