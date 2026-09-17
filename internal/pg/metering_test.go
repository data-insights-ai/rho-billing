package pg

import (
	"errors"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/usage"
)

func TestPostgresScopedDuplicatesOverlapAndPayloadConflict(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	config := usage.RuleConfig{Version: "lib40-rule", Kind: usage.KindWeighted, Target: usage.Target{Currency: "USD"}, Rounding: usage.RoundDown, Weights: map[string]string{"tokens": "1"}}
	if err := store.PublishRating(ctx, config); err != nil {
		t.Fatal(err)
	}
	for _, account := range []billing.AccountID{"lib40-a", "lib40-b"} {
		if err := store.CreateAccount(ctx, account, string(account)+"-subject"); err != nil {
			t.Fatal(err)
		}
	}
	service := usage.New(store.UsageRepository(), func() time.Time { return now })
	shared := usage.Observation{ID: "shared-usage-id", Source: "meter", OccurredAt: now, Funding: usage.Postpaid, Input: usage.RateInput{Metrics: []usage.MetricQuantity{{Name: "tokens", Quantity: 2}}}}
	for _, account := range []billing.AccountID{"lib40-a", "lib40-b"} {
		o := shared
		o.Account = account
		if _, err := service.RateAndRecord(ctx, o, config.Version); err != nil {
			t.Fatalf("account %s: %v", account, err)
		}
	}
	changed := shared
	changed.Account = "lib40-a"
	changed.Input.Metrics = []usage.MetricQuantity{{Name: "tokens", Quantity: 9}}
	if _, err := service.RateAndRecord(ctx, changed, config.Version); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("payload conflict err=%v", err)
	}
	interval := billing.Period{Start: now.Add(2 * time.Hour), End: now.Add(3 * time.Hour)}
	if _, err := service.RateAndRecord(ctx, usage.Observation{Account: "lib40-a", ID: "agg-a", Source: "meter", OccurredAt: now.Add(2*time.Hour + time.Minute), Interval: interval, Funding: usage.Postpaid, Input: usage.RateInput{Metrics: []usage.MetricQuantity{{Name: "tokens", Quantity: 4}}}}, config.Version); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RateAndRecord(ctx, usage.Observation{Account: "lib40-a", ID: "agg-b", Source: "meter", OccurredAt: now.Add(2*time.Hour + 45*time.Minute), Interval: billing.Period{Start: now.Add(2*time.Hour + 30*time.Minute), End: now.Add(4 * time.Hour)}, Funding: usage.Postpaid, Input: usage.RateInput{Metrics: []usage.MetricQuantity{{Name: "tokens", Quantity: 1}}}}, config.Version); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("overlap err=%v, want conflict", err)
	}
}

func TestPostgresPeriodQuotaOnceAcrossSources(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("lib41-acct")
	if err := store.CreateAccount(ctx, account, "lib41-subject"); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	v1 := usage.RuleConfig{Version: "lib41-quota-v1", Kind: usage.KindIncludedQuota, Target: usage.Target{Currency: "USD"}, Rounding: usage.RoundDown, Included: &usage.IncludedQuotaConfig{Metric: "requests", Included: 2, Rate: "3"}}
	v2 := usage.RuleConfig{Version: "lib41-quota-v2", Kind: usage.KindIncludedQuota, Target: usage.Target{Currency: "USD"}, Rounding: usage.RoundDown, Included: &usage.IncludedQuotaConfig{Metric: "requests", Included: 0, Rate: "3"}}
	for _, config := range []usage.RuleConfig{v1, v2} {
		if err := store.PublishRating(ctx, config); err != nil {
			t.Fatal(err)
		}
	}
	service := usage.New(store.UsageRepository(), func() time.Time { return end })
	input := usage.RateInput{Metrics: []usage.MetricQuantity{{Name: "requests", Quantity: 5}}}
	first, err := service.RateAndRecord(ctx, usage.Observation{Account: account, ID: "quota-a", Source: "meter-a", OccurredAt: start.Add(time.Hour), Interval: billing.Period{Start: start, End: end}, Funding: usage.Postpaid, Input: input}, v1.Version)
	if err != nil || first.Rating.RoundedAmount != 9 {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	second, err := service.RateAndRecord(ctx, usage.Observation{Account: account, ID: "quota-b", Source: "meter-b", OccurredAt: start.Add(2 * time.Hour), Interval: billing.Period{Start: start, End: end}, Funding: usage.Postpaid, Input: input}, v2.Version)
	if err != nil {
		t.Fatal(err)
	}
	period := usage.BillingPeriod{Start: start, End: end, Cutoff: end}
	if _, err := finishSettlementClose(ctx, store.Settlements(), func() time.Time { return end }, usage.CloseInput{Account: account, Operation: "lib41-close", BatchID: "lib41-batch", Period: period, Currency: "USD", CreatedAt: end}); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("two period aggregates closed err=%v, want conflict", err)
	}
	if usage.PostpaidAggregateIdentity(first, start, end) != usage.PostpaidAggregateIdentity(second, start, end) {
		t.Fatal("source/rule version escaped the one-aggregate identity")
	}
}

func TestPostgresLateUsageCorrectionLeavesPublishedTotal(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("lib42-acct")
	if err := store.CreateAccount(ctx, account, "lib42-subject"); err != nil {
		t.Fatal(err)
	}
	period := settlementTestPeriod()
	original := settlementTestRecord(t, store, string(account), "closed-usage", period.Start.Add(time.Hour), period.Start.Add(2*time.Hour), 5)
	if _, err := recordUsage(ctx, store, original); err != nil {
		t.Fatal(err)
	}
	closed, err := finishSettlementClose(ctx, store.Settlements(), store.now, usage.CloseInput{Account: account, Operation: "lib42-close", BatchID: "lib42-batch", Period: period, Currency: "USD", CreatedAt: period.Cutoff})
	if err != nil || closed.Total != 5 {
		t.Fatalf("closed=%+v err=%v", closed, err)
	}
	late := settlementTestRecord(t, store, string(account), "late-usage", period.Start.Add(3*time.Hour), period.Cutoff.Add(time.Hour), 3)
	if _, err := recordUsage(ctx, store, late); err != nil {
		t.Fatal(err)
	}
	svc := usage.NewSettlement(store.Settlements(), func() time.Time { return period.Cutoff.Add(2 * time.Hour) })
	correction, err := svc.Correct(ctx, usage.CorrectionInput{Account: account, Operation: "lib42-correct", BatchID: "lib42-correction", OriginalBatchID: closed.ID, Period: period, Currency: "USD", UsageIDs: []string{late.Observation.ID}, CreatedAt: period.Cutoff.Add(2 * time.Hour)})
	if err != nil || correction.Total != 3 || correction.OriginalBatchID != closed.ID {
		t.Fatalf("correction=%+v err=%v", correction, err)
	}
	unchanged, err := svc.BatchSummary(ctx, account, closed.ID)
	if err != nil || unchanged.Total != 5 || unchanged.State != usage.BatchReady {
		t.Fatalf("published total changed: %+v err=%v", unchanged, err)
	}
}

func TestPostgresCostPersistenceCurrencyAndOverflow(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("lib43-acct")
	if err := store.CreateAccount(ctx, account, "lib43-subject"); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	config := usage.RuleConfig{Version: "lib43-usage", Kind: usage.KindWeighted, Target: usage.Target{Currency: "USD"}, Rounding: usage.RoundDown, Weights: map[string]string{"tokens": "1"}}
	if err := store.PublishRating(ctx, config); err != nil {
		t.Fatal(err)
	}
	service := usage.New(store.UsageRepository(), func() time.Time { return now })
	usageRecord, err := service.RateAndRecord(ctx, usage.Observation{Account: account, ID: "usage-1", Source: "meter", OccurredAt: now, Funding: usage.Postpaid, Input: usage.RateInput{Metrics: []usage.MetricQuantity{{Name: "tokens", Quantity: 2}}}}, config.Version)
	if err != nil {
		t.Fatal(err)
	}
	estimate, err := service.RecordCost(ctx, usage.CostInput{Account: account, ID: "cost-est", UsageID: usageRecord.Observation.ID, Resource: "gpu", Model: "large-1", Quantity: 2, RuleVersion: "cost-v1", Currency: "EUR", Amount: 40, ExactAmount: "40", State: usage.CostEstimated, OccurredAt: now})
	if err != nil || estimate.Currency != "EUR" || estimate.Amount == usageRecord.Rating.RoundedAmount {
		t.Fatalf("estimate=%+v rating=%+v err=%v", estimate, usageRecord.Rating, err)
	}
	actual, err := service.RecordCost(ctx, usage.CostInput{Account: account, ID: "cost-act", UsageID: usageRecord.Observation.ID, Resource: "gpu", Model: "large-1", Quantity: 2, RuleVersion: "cost-v1", Currency: "EUR", Amount: 37, ExactAmount: "37", State: usage.CostActual, CorrectsID: estimate.ID, OccurredAt: now})
	if err != nil || actual.CorrectsID != estimate.ID {
		t.Fatalf("actual=%+v err=%v", actual, err)
	}
	missing, err := service.RecordCost(ctx, usage.CostInput{Account: account, ID: "cost-miss", Resource: "storage", Model: "blob", Quantity: 1, RuleVersion: "cost-v1", Currency: "EUR", State: usage.CostMissing, MissingReason: "invoice-absent", OccurredAt: now})
	if err != nil || missing.State != usage.CostMissing {
		t.Fatalf("missing=%+v err=%v", missing, err)
	}
	if _, err := service.RecordCost(ctx, usage.CostInput{Account: account, ID: "cost-fx", Resource: "gpu", Model: "large-1", Quantity: 1, RuleVersion: "cost-v1", Currency: "EUR", Amount: 1, ExactAmount: "1", State: usage.CostActual, SourceCurrency: "USD", OccurredAt: now}); !errors.Is(err, billing.ErrInvalid) {
		t.Fatalf("fx err=%v", err)
	}
	if _, err := service.RecordCost(ctx, usage.CostInput{Account: account, ID: "cost-overflow", Resource: "gpu", Model: "large-1", Quantity: 1, RuleVersion: "cost-v1", Currency: "EUR", Amount: 0, ExactAmount: "9223372036854775808", State: usage.CostActual, OccurredAt: now}); !errors.Is(err, billing.ErrOverflow) {
		t.Fatalf("overflow err=%v", err)
	}
	stored, err := service.Cost(ctx, account, missing.ID)
	if err != nil || stored.MissingReason != "invoice-absent" {
		t.Fatalf("stored missing=%+v err=%v", stored, err)
	}
	priced, err := service.Usage(ctx, account, usageRecord.Observation.ID)
	if err != nil || priced.Rating.Money.Currency != "USD" || priced.Rating.RoundedAmount != 2 {
		t.Fatalf("customer price changed: %+v err=%v", priced, err)
	}
}
