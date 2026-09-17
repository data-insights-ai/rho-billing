package host_test

import (
	"errors"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/postgres"
	"github.com/data-insights-ai/rho-billing/usage"
)

func TestSettlementServiceConstructionAndInvalidIdentityAreLocal(t *testing.T) {
	service := usage.NewSettlement(postgres.New(nil).Settlements(), nil)
	_, err := service.BeginSubmission(t.Context(), usage.BeginSubmissionInput{})
	if !errors.Is(err, billing.ErrInvalid) {
		t.Fatalf("invalid input: %v", err)
	}
	if _, recorded := errors.AsType[*usage.SettlementRejection](err); recorded {
		t.Fatal("unrecorded validation failure labeled durable rejection")
	}
}

func TestExternalStoredUsageFinalization(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	period := usage.BillingPeriod{Start: now.Add(-time.Hour), End: now, Cutoff: now}
	rule, err := usage.NewRule(usage.RuleConfig{Version: "fixed", Kind: usage.KindFixed, Target: usage.Target{Currency: "USD"}, Rounding: usage.RoundDown, FixedRate: "1"})
	if err != nil {
		t.Fatal(err)
	}
	record, err := usage.Prepare(usage.Observation{Account: "host", ID: "usage", Source: "meter", OccurredAt: period.Start, Funding: usage.Postpaid, Input: usage.RateInput{ActionCount: 2}}, rule, now)
	if err != nil {
		t.Fatal(err)
	}
	repo := usage.NewMemorySettlementRepository(usage.ReferenceAccount{ID: "host", Usage: []usage.Record{record}})
	service := usage.NewSettlement(repo, func() time.Time { return now })
	job, err := service.StartClose(t.Context(), usage.CloseInput{Account: "host", Operation: "close", BatchID: "batch", Period: period, Currency: "USD"})
	if err != nil {
		t.Fatal(err)
	}
	for job.State == usage.ClosePreparing {
		job, err = service.AdvanceClose(t.Context(), "host", job.BatchID, job.Revision, 1)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := service.PublishClose(t.Context(), "host", job.BatchID, job.Revision); err != nil {
		t.Fatal(err)
	}
	batch, err := service.BatchSummary(t.Context(), "host", "batch")
	if err != nil || batch.Total != 2 || batch.LineCount != 1 {
		t.Fatalf("finalized batch=%+v err=%v", batch, err)
	}
	lines, _, more, err := service.BatchLinesPage(t.Context(), "host", "batch", "", 1)
	if err != nil || more || len(lines) != 1 || lines[0].UsageID != "usage" || lines[0].Amount != 2 || lines[0].Kind != usage.ChargeUsage {
		t.Fatalf("published lines=%+v more=%v err=%v", lines, more, err)
	}
}
