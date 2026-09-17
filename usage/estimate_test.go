package usage

import (
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
)

func TestEstimatePeriodAllowsFutureCutoffAndCanonicalKeys(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 1234, time.FixedZone("offset", 2*60*60))
	period := EstimatePeriod{Start: start, End: start.Add(48 * time.Hour), Cutoff: start.Add(60 * time.Hour)}
	if !period.Valid() {
		t.Fatal("future-capable estimate period should be valid")
	}
	if got := period.UTC(); !got.Start.Equal(billing.CanonicalTime(start)) || got.Start.Nanosecond() != 1000 {
		t.Fatalf("period was not canonicalized: %+v", got)
	}
	if UsageEstimateKey("usage-1") != "usage:usage-1" || AdjustmentEstimateKey("adjust-1") != "adjustment:adjust-1" {
		t.Fatal("estimate cursor keys are not canonical")
	}
}

func TestEstimateInputValidation(t *testing.T) {
	period := EstimatePeriod{Start: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC), Cutoff: time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)}
	in := EstimateInput{Account: "acct", Period: period, Currency: "USD", Limit: 10}
	if !in.Valid() {
		t.Fatal("valid estimate input rejected")
	}
	in.BatchID = "bad\nbatch"
	if in.Valid() {
		t.Fatal("invalid frozen batch ID accepted")
	}
	in.BatchID = "batch-1"
	if !in.Valid() {
		t.Fatal("valid frozen batch ID rejected")
	}
}
