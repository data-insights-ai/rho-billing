package usage

import (
	"errors"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
)

func TestMidPeriodRuleChangeDoesNotMintSecondPeriodAggregate(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	period := BillingPeriod{Start: start, End: end, Cutoff: end}
	v1, err := NewRule(RuleConfig{Version: "quota-v1", Kind: KindIncludedQuota, Target: Target{Currency: "USD"}, Rounding: RoundDown, Included: &IncludedQuotaConfig{Metric: "requests", Included: 2, Rate: "3"}})
	if err != nil {
		t.Fatal(err)
	}
	v2, err := NewRule(RuleConfig{Version: "quota-v2", Kind: KindIncludedQuota, Target: Target{Currency: "USD"}, Rounding: RoundDown, Included: &IncludedQuotaConfig{Metric: "requests", Included: 0, Rate: "3"}})
	if err != nil {
		t.Fatal(err)
	}
	input := RateInput{Metrics: []MetricQuantity{{Name: "requests", Quantity: 5}}}
	first, err := Prepare(Observation{Account: settlementAccount, ID: "quota-source-a", Source: "meter-a", OccurredAt: start.Add(time.Hour), Interval: billing.Period{Start: start, End: end}, Funding: Postpaid, Input: input}, v1, end)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Prepare(Observation{Account: settlementAccount, ID: "quota-source-b", Source: "meter-b", OccurredAt: start.Add(2 * time.Hour), Interval: billing.Period{Start: start, End: end}, Funding: Postpaid, Input: input}, v2, end)
	if err != nil {
		t.Fatal(err)
	}
	closeInput := CloseInput{Account: settlementAccount, Operation: "quota-op", BatchID: "quota-batch", Period: period, Currency: "USD", CreatedAt: end}
	if _, err := prepareBatch(closeInput, []Record{first, second}); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("second source/rule aggregate err=%v, want conflict", err)
	}
	batch, err := prepareBatch(closeInput, []Record{first})
	if err != nil || batch.Total != 9 || len(batch.Lines) != 1 {
		t.Fatalf("period-once quota batch=%+v err=%v", batch, err)
	}
}

func TestLateUsageAfterCloseDoesNotChangeFinalizedBatch(t *testing.T) {
	service, _, clock := newSettlementService()
	period := settlementPeriod(clock)
	originalRecord := postpaidRecord(t, "closed-usage", 5, clock.Now(), clock.Now(), "USD")
	original := finalize(t, service, clock, "closed-batch", []Record{originalRecord}, period)
	if original.Total != 5 || original.State != BatchReady {
		t.Fatalf("original=%+v", original)
	}
	clock.Advance(2 * 24 * time.Hour)
	late := postpaidRecord(t, "late-usage", 3, period.Start.Add(2*time.Hour), clock.Now(), "USD")
	seedSettlementUsage(t, service, []Record{late})
	correction, err := service.Correct(t.Context(), CorrectionInput{Account: settlementAccount, Operation: "late-correction", BatchID: "late-correction-batch", OriginalBatchID: original.ID, Period: period, Currency: "USD", UsageIDs: []string{late.Observation.ID}, CreatedAt: clock.Now().Add(time.Hour)})
	if err != nil || correction.Total != 3 || correction.OriginalBatchID != original.ID {
		t.Fatalf("correction=%+v err=%v", correction, err)
	}
	lines, _, more, err := service.BatchLinesPage(t.Context(), settlementAccount, correction.ID, "", 10)
	if err != nil || more || len(lines) != 1 || lines[0].UsageID != "late-usage" || lines[0].Kind != "usage" {
		t.Fatalf("correction lines=%+v more=%v err=%v", lines, more, err)
	}
	unchanged, err := service.BatchSummary(t.Context(), settlementAccount, original.ID)
	if err != nil || unchanged.Total != 5 || unchanged.State != BatchReady || unchanged.LineCount != 1 {
		t.Fatalf("finalized invoice basis changed: %+v err=%v", unchanged, err)
	}
	if _, err := service.finishClose(t.Context(), CloseInput{Account: settlementAccount, Operation: "reclose", BatchID: "reclose-batch", Period: period, Currency: "USD", CreatedAt: period.Cutoff}); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("re-close after publish err=%v, want conflict", err)
	}
}
