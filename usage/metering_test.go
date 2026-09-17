package usage

import (
	"errors"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
)

func TestDuplicateUsageKeysAreScopedAndPayloadConflictsFail(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	rule := usageTestRule(t, "lib40-rule")
	repo := NewMemoryRepository(MemoryConfig{Accounts: []billing.AccountID{"acct-a", "acct-b"}, Rules: ruleResolver(rule)})
	service := New(repo, func() time.Time { return now })
	base := Observation{ID: "shared-usage-id", Source: "meter", OccurredAt: now, Funding: Postpaid, Input: RateInput{Metrics: []MetricQuantity{{Name: "tokens", Quantity: 2}}}}
	first := base
	first.Account = "acct-a"
	if _, err := service.RateAndRecord(t.Context(), first, rule.Version()); err != nil {
		t.Fatal(err)
	}
	second := base
	second.Account = "acct-b"
	if _, err := service.RateAndRecord(t.Context(), second, rule.Version()); err != nil {
		t.Fatalf("same usage id on another account err=%v", err)
	}
	changed := first
	changed.Input.Metrics = []MetricQuantity{{Name: "tokens", Quantity: 3}}
	if _, err := service.RateAndRecord(t.Context(), changed, rule.Version()); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("payload conflict err=%v, want conflict", err)
	}
	replay, err := service.RateAndRecord(t.Context(), first, rule.Version())
	if err != nil || replay.Observation.Input.Metrics[0].Quantity != 2 {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}

	interval := billing.Period{Start: now.Add(2 * time.Hour), End: now.Add(3 * time.Hour)}
	agg := Observation{Account: "acct-b", ID: "agg-a", Source: "meter", OccurredAt: now.Add(2*time.Hour + time.Minute), Interval: interval, Funding: Postpaid, Input: RateInput{Metrics: []MetricQuantity{{Name: "tokens", Quantity: 4}}}}
	if _, err := service.RateAndRecord(t.Context(), agg, rule.Version()); err != nil {
		t.Fatal(err)
	}
	overlap := Observation{Account: "acct-b", ID: "agg-b", Source: "meter", OccurredAt: now.Add(2*time.Hour + 45*time.Minute), Interval: billing.Period{Start: now.Add(2*time.Hour + 30*time.Minute), End: now.Add(4 * time.Hour)}, Funding: Postpaid, Input: RateInput{Metrics: []MetricQuantity{{Name: "tokens", Quantity: 1}}}}
	if _, err := service.RateAndRecord(t.Context(), overlap, rule.Version()); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("overlapping aggregate err=%v, want conflict", err)
	}
	otherSource := overlap
	otherSource.Source = "other-meter"
	if _, err := service.RateAndRecord(t.Context(), otherSource, rule.Version()); err != nil {
		t.Fatalf("distinct source overlap at measurement err=%v", err)
	}
}

func TestIncludedQuotaAndGraduatedTiersApplyOncePerPeriod(t *testing.T) {
	quota, err := NewRule(RuleConfig{Version: "quota-v1", Kind: KindIncludedQuota, Target: Target{Currency: "USD"}, Rounding: RoundDown, Included: &IncludedQuotaConfig{Metric: "requests", Included: 2, Rate: "3"}})
	if err != nil {
		t.Fatal(err)
	}
	periodQty := RateInput{Metrics: []MetricQuantity{{Name: "requests", Quantity: 5}}}
	period, err := quota.Rate(periodQty)
	if err != nil {
		t.Fatal(err)
	}
	var perEvent int64
	for range 5 {
		one, err := quota.Rate(RateInput{Metrics: []MetricQuantity{{Name: "requests", Quantity: 1}}})
		if err != nil {
			t.Fatal(err)
		}
		perEvent += one.RoundedAmount
	}
	if period.RoundedAmount != 9 || perEvent != 0 {
		t.Fatalf("quota period=%d per-event-sum=%d, want period 9 and per-event 0", period.RoundedAmount, perEvent)
	}

	graduated, err := NewRule(RuleConfig{Version: "tiers-v1", Kind: KindGraduated, Target: Target{Currency: "USD"}, Rounding: RoundDown, Metric: "units", Tiers: []Tier{{UpTo: 2, Rate: "1"}, {Rate: "10"}}})
	if err != nil {
		t.Fatal(err)
	}
	once, err := graduated.Rate(RateInput{Metrics: []MetricQuantity{{Name: "units", Quantity: 5}}})
	if err != nil {
		t.Fatal(err)
	}
	var chunkSum int64
	for range 5 {
		chunk, err := graduated.Rate(RateInput{Metrics: []MetricQuantity{{Name: "units", Quantity: 1}}})
		if err != nil {
			t.Fatal(err)
		}
		chunkSum += chunk.RoundedAmount
	}
	if once.RoundedAmount != 32 || chunkSum != 5 {
		t.Fatalf("graduated period=%d chunk-sum=%d, want 32 vs 5", once.RoundedAmount, chunkSum)
	}

	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	first, err := Prepare(Observation{Account: "acct", ID: "quota-a", Source: "meter-a", OccurredAt: start.Add(time.Hour), Interval: billing.Period{Start: start, End: end}, Funding: Postpaid, Input: periodQty}, quota, end)
	if err != nil {
		t.Fatal(err)
	}
	laterRule, err := NewRule(RuleConfig{Version: "quota-v2", Kind: KindIncludedQuota, Target: Target{Currency: "USD"}, Rounding: RoundDown, Included: &IncludedQuotaConfig{Metric: "requests", Included: 0, Rate: "3"}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Prepare(Observation{Account: "acct", ID: "quota-b", Source: "meter-b", OccurredAt: start.Add(2 * time.Hour), Interval: billing.Period{Start: start, End: end}, Funding: Postpaid, Input: periodQty}, laterRule, end)
	if err != nil {
		t.Fatal(err)
	}
	if PostpaidAggregateIdentity(first, start, end) == "" || PostpaidAggregateIdentity(first, start, end) != PostpaidAggregateIdentity(second, start, end) {
		t.Fatalf("mid-period rule version split the period identity: %s vs %s", PostpaidAggregateIdentity(first, start, end), PostpaidAggregateIdentity(second, start, end))
	}
}

func TestInternalCostsRetainEvidenceAndMissingIsVisible(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	rule := usageTestRule(t, "lib43-usage")
	repo := NewMemoryRepository(MemoryConfig{Accounts: []billing.AccountID{"acct"}, Rules: ruleResolver(rule)})
	service := New(repo, func() time.Time { return now })
	usageRecord, err := service.RateAndRecord(t.Context(), Observation{Account: "acct", ID: "usage-1", Source: "meter", OccurredAt: now, Funding: Postpaid, Input: RateInput{Metrics: []MetricQuantity{{Name: "tokens", Quantity: 2}}}}, rule.Version())
	if err != nil {
		t.Fatal(err)
	}
	estimate, err := service.RecordCost(t.Context(), CostInput{Account: "acct", ID: "cost-est", UsageID: usageRecord.Observation.ID, Resource: "gpu", Model: "large-1", Quantity: 2, RuleVersion: "cost-v1", Currency: "USD", Amount: 40, ExactAmount: "40", State: CostEstimated, OccurredAt: now})
	if err != nil || estimate.State != CostEstimated || estimate.Resource != "gpu" || estimate.Model != "large-1" {
		t.Fatalf("estimate=%+v err=%v", estimate, err)
	}
	replay, err := service.RecordCost(t.Context(), CostInput{Account: "acct", ID: "cost-est", UsageID: usageRecord.Observation.ID, Resource: "gpu", Model: "large-1", Quantity: 2, RuleVersion: "cost-v1", Currency: "USD", Amount: 40, ExactAmount: "40", State: CostEstimated, OccurredAt: now})
	if err != nil || replay.Fingerprint != estimate.Fingerprint || replay.RecordedAt != estimate.RecordedAt {
		t.Fatalf("cost replay=%+v err=%v", replay, err)
	}
	changed := CostInput{Account: "acct", ID: "cost-est", UsageID: usageRecord.Observation.ID, Resource: "gpu", Model: "large-1", Quantity: 3, RuleVersion: "cost-v1", Currency: "USD", Amount: 40, ExactAmount: "40", State: CostEstimated, OccurredAt: now}
	if _, err := service.RecordCost(t.Context(), changed); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("changed cost payload err=%v, want conflict", err)
	}
	actual, err := service.RecordCost(t.Context(), CostInput{Account: "acct", ID: "cost-act", UsageID: usageRecord.Observation.ID, Resource: "gpu", Model: "large-1", Quantity: 2, RuleVersion: "cost-v1", Currency: "USD", Amount: 37, ExactAmount: "37", State: CostActual, CorrectsID: estimate.ID, OccurredAt: now})
	if err != nil || actual.CorrectsID != estimate.ID || actual.State != CostActual {
		t.Fatalf("actual=%+v err=%v", actual, err)
	}
	if _, err := service.RecordCost(t.Context(), CostInput{Account: "acct", ID: "cost-act-2", Resource: "gpu", Model: "large-1", Quantity: 2, RuleVersion: "cost-v1", Currency: "USD", Amount: 36, ExactAmount: "36", State: CostActual, CorrectsID: estimate.ID, OccurredAt: now}); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("second correction err=%v, want conflict", err)
	}
	missing, err := service.RecordCost(t.Context(), CostInput{Account: "acct", ID: "cost-miss", Resource: "storage", Model: "blob", Quantity: 1, RuleVersion: "cost-v1", Currency: "USD", State: CostMissing, MissingReason: "provider-invoice-absent", OccurredAt: now})
	if err != nil || missing.State != CostMissing || missing.Amount != 0 || missing.MissingReason == "" {
		t.Fatalf("missing=%+v err=%v", missing, err)
	}
	got, err := service.Cost(t.Context(), "acct", missing.ID)
	if err != nil || got.State != CostMissing || got.MissingReason != "provider-invoice-absent" {
		t.Fatalf("stored missing=%+v err=%v", got, err)
	}
}

func TestExactArithmeticNoImplicitFXIndependentOfCustomerPrice(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	rule := usageTestRule(t, "lib44-usage")
	repo := NewMemoryRepository(MemoryConfig{Accounts: []billing.AccountID{"acct"}, Rules: ruleResolver(rule)})
	service := New(repo, func() time.Time { return now })
	usageRecord, err := service.RateAndRecord(t.Context(), Observation{Account: "acct", ID: "usage-price", Source: "meter", OccurredAt: now, Funding: Postpaid, Input: RateInput{Metrics: []MetricQuantity{{Name: "tokens", Quantity: 2}}}}, rule.Version())
	if err != nil || usageRecord.Rating.Money == nil || usageRecord.Rating.Money.Currency != "USD" || usageRecord.Rating.RoundedAmount != 2 {
		t.Fatalf("customer rating=%+v err=%v", usageRecord.Rating, err)
	}
	cost, err := service.RecordCost(t.Context(), CostInput{Account: "acct", ID: "cost-eur", UsageID: usageRecord.Observation.ID, Resource: "gpu", Model: "large-1", Quantity: 2, RuleVersion: "cost-eur-v1", Currency: "EUR", Amount: 90, ExactAmount: "90", State: CostActual, OccurredAt: now})
	if err != nil || cost.Currency != "EUR" || cost.Amount != 90 || cost.Amount == usageRecord.Rating.RoundedAmount {
		t.Fatalf("internal cost must stay independent of customer price: cost=%+v rating=%+v err=%v", cost, usageRecord.Rating, err)
	}
	if _, err := service.RecordCost(t.Context(), CostInput{Account: "acct", ID: "cost-fx", Resource: "gpu", Model: "large-1", Quantity: 1, RuleVersion: "cost-eur-v1", Currency: "EUR", Amount: 1, ExactAmount: "1", State: CostActual, SourceCurrency: "USD", OccurredAt: now}); !errors.Is(err, billing.ErrInvalid) {
		t.Fatalf("implicit FX err=%v, want invalid", err)
	}
	if _, err := service.RecordCost(t.Context(), CostInput{Account: "acct", ID: "cost-overflow", Resource: "gpu", Model: "large-1", Quantity: 1, RuleVersion: "cost-eur-v1", Currency: "EUR", Amount: 0, ExactAmount: "9223372036854775808", State: CostActual, OccurredAt: now}); !errors.Is(err, billing.ErrOverflow) {
		t.Fatalf("overflow err=%v, want overflow", err)
	}
	storedUsage, err := service.Usage(t.Context(), "acct", usageRecord.Observation.ID)
	if err != nil || storedUsage.Rating.RoundedAmount != 2 || storedUsage.Rating.Money.Currency != "USD" {
		t.Fatalf("customer usage mutated by internal cost: %+v err=%v", storedUsage, err)
	}
}
