package host_test

import (
	"context"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/usage"
)

func TestHostRecordsUsageAndInternalCostIndependently(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	rule, err := usage.NewRule(usage.RuleConfig{Version: "host-meter-rule", Kind: usage.KindWeighted, Target: usage.Target{Currency: "USD"}, Rounding: usage.RoundDown, Weights: map[string]string{"tokens": "2"}})
	if err != nil {
		t.Fatal(err)
	}
	repo := usage.NewMemoryRepository(usage.MemoryConfig{
		Accounts: []billing.AccountID{"host-meter"},
		Rules: func(_ context.Context, version string) (*usage.Rule, error) {
			if version != rule.Version() {
				return nil, billing.ErrNotFound
			}
			return rule, nil
		},
	})
	service := usage.New(repo, func() time.Time { return now })
	record, err := service.RateAndRecord(t.Context(), usage.Observation{Account: "host-meter", ID: "host-usage-1", Source: "meter", OccurredAt: now, Funding: usage.Postpaid, Input: usage.RateInput{Metrics: []usage.MetricQuantity{{Name: "tokens", Quantity: 3}}}}, rule.Version())
	if err != nil || record.Rating.RoundedAmount != 6 {
		t.Fatalf("usage=%+v err=%v", record, err)
	}
	cost, err := service.RecordCost(t.Context(), usage.CostInput{Account: "host-meter", ID: "host-cost-1", UsageID: record.Observation.ID, Resource: "gpu", Model: "host-model", Quantity: 3, RuleVersion: "host-cost-v1", Currency: "EUR", Amount: 11, ExactAmount: "11", State: usage.CostActual, OccurredAt: now})
	if err != nil || cost.Currency != "EUR" || cost.Amount == record.Rating.RoundedAmount {
		t.Fatalf("cost=%+v rating=%+v err=%v", cost, record.Rating, err)
	}
}
