package usage

import (
	"errors"
	"testing"
)

func TestFixedCreditsAndEvidence(t *testing.T) {
	rule := mustRule(t, RuleConfig{
		Version:   "fixed-v1",
		Kind:      KindFixed,
		Target:    Target{CreditUnit: "api_tokens"},
		Rounding:  RoundHalfEven,
		FixedRate: "2.5",
	})
	result, err := rule.Rate(RateInput{ActionCount: 3})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExactAmount != "15/2" || result.RoundedAmount != 8 || result.Credits == nil || result.Credits.Subunits != 8 {
		t.Fatalf("unexpected fixed result: %#v", result)
	}
	if result.Money != nil || result.RuleVersion != "fixed-v1" || result.Evidence.ActionCount != 3 {
		t.Fatalf("unexpected result evidence: %#v", result)
	}
}

func TestWeightedRatesRoundAggregateOnce(t *testing.T) {
	rule := mustRule(t, RuleConfig{
		Version:  "weighted-v1",
		Kind:     KindWeighted,
		Target:   Target{Currency: "USD"},
		Rounding: RoundHalfUp,
		Weights:  map[string]string{"input": "0.6", "output": "0.6"},
	})
	result, err := rule.Rate(RateInput{Metrics: []MetricQuantity{{Name: "output", Quantity: 1}, {Name: "input", Quantity: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExactAmount != "6/5" || result.RoundedAmount != 1 || result.Money == nil || result.Money.Currency != "USD" {
		t.Fatalf("aggregate rounding was not applied: %#v", result)
	}
	if got := result.Evidence.Metrics[0].Name; got != "input" {
		t.Fatalf("metrics were not normalized deterministically: %q", got)
	}
}

func TestIncludedQuotaBoundary(t *testing.T) {
	rule := mustRule(t, RuleConfig{
		Version:  "quota-v1",
		Kind:     KindIncludedQuota,
		Target:   Target{Currency: "EUR"},
		Rounding: RoundDown,
		Included: &IncludedQuotaConfig{Metric: "requests", Included: 2, Rate: "3"},
	})
	for _, test := range []struct {
		quantity int64
		want     int64
	}{
		{quantity: 0, want: 0},
		{quantity: 2, want: 0},
		{quantity: 3, want: 3},
	} {
		result, err := rule.Rate(RateInput{Metrics: []MetricQuantity{{Name: "requests", Quantity: test.quantity}}})
		if err != nil {
			t.Fatal(err)
		}
		if result.RoundedAmount != test.want {
			t.Errorf("quantity %d: got %d, want %d", test.quantity, result.RoundedAmount, test.want)
		}
	}
}

func TestGraduatedTierBoundaries(t *testing.T) {
	rule := mustRule(t, RuleConfig{
		Version:  "tiers-v1",
		Kind:     KindGraduated,
		Target:   Target{CreditUnit: "compute"},
		Rounding: RoundDown,
		Metric:   "units",
		Tiers: []Tier{
			{UpTo: 2, Rate: "1"},
			{UpTo: 4, Rate: "2"},
			{Rate: "3"},
		},
	})
	for _, test := range []struct {
		quantity int64
		want     int64
	}{
		{quantity: 1, want: 1},
		{quantity: 2, want: 2},
		{quantity: 3, want: 4},
		{quantity: 4, want: 6},
		{quantity: 5, want: 9},
	} {
		result, err := rule.Rate(RateInput{Metrics: []MetricQuantity{{Name: "units", Quantity: test.quantity}}})
		if err != nil {
			t.Fatal(err)
		}
		if result.RoundedAmount != test.want {
			t.Errorf("quantity %d: got %d, want %d", test.quantity, result.RoundedAmount, test.want)
		}
	}
}

func TestAdjustmentAndWaiver(t *testing.T) {
	rule := mustRule(t, RuleConfig{Version: "adjust-v1", Kind: KindFixed, Target: Target{Currency: "USD"}, Rounding: RoundDown, FixedRate: "10"})
	result, err := rule.Rate(RateInput{ActionCount: 2, Adjustment: "-3/2", AdjustmentReason: "service credit"})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExactAmount != "37/2" || result.RoundedAmount != 18 || result.Evidence.Adjustment != "-3/2" {
		t.Fatalf("unexpected adjustment result: %#v", result)
	}
	waived, err := rule.Rate(RateInput{ActionCount: 2, Waived: true})
	if err != nil {
		t.Fatal(err)
	}
	if waived.RoundedAmount != 0 || waived.ExactAmount != "0" || !waived.Evidence.Waived {
		t.Fatalf("unexpected waiver result: %#v", waived)
	}
	_, err = rule.Rate(RateInput{ActionCount: 2, Waived: true, Adjustment: "1", AdjustmentReason: "bad combination"})
	if !errors.Is(err, ErrConflictingInput) {
		t.Fatalf("waiver and adjustment error = %v", err)
	}
}

func TestInvalidAndConflictingInputs(t *testing.T) {
	rule := mustRule(t, RuleConfig{Version: "input-v1", Kind: KindWeighted, Target: Target{CreditUnit: "requests"}, Rounding: RoundDown, Weights: map[string]string{"a": "1"}})
	for _, input := range []RateInput{
		{Metrics: []MetricQuantity{{Name: "a", Quantity: -1}}},
		{Metrics: []MetricQuantity{{Name: "a", Quantity: 1}, {Name: "a", Quantity: 2}}},
		{Metrics: []MetricQuantity{{Name: "other", Quantity: 1}}},
		{ActionCount: 1},
	} {
		if _, err := rule.Rate(input); err == nil {
			t.Errorf("input %#v unexpectedly succeeded", input)
		}
	}
	_, err := NewRule(RuleConfig{Version: "bad", Kind: KindFixed, Target: Target{Currency: "USD", CreditUnit: "x"}, Rounding: RoundDown, FixedRate: "1"})
	if !errors.Is(err, ErrInvalidRule) {
		t.Fatalf("incompatible target error = %v", err)
	}
}

func TestOverflow(t *testing.T) {
	rule := mustRule(t, RuleConfig{Version: "overflow-v1", Kind: KindFixed, Target: Target{Currency: "USD"}, Rounding: RoundDown, FixedRate: "9223372036854775807"})
	_, err := rule.Rate(RateInput{ActionCount: 2})
	if !errors.Is(err, ErrOverflow) {
		t.Fatalf("overflow error = %v", err)
	}
}

func TestRuleCopiesMutableConfiguration(t *testing.T) {
	weights := map[string]string{"input": "2"}
	config := RuleConfig{Version: "copy-v1", Kind: KindWeighted, Target: Target{CreditUnit: "tokens"}, Rounding: RoundDown, Weights: weights}
	rule := mustRule(t, config)
	weights["input"] = "1000"
	delete(weights, "input")
	result, err := rule.Rate(RateInput{Metrics: []MetricQuantity{{Name: "input", Quantity: 3}}})
	if err != nil {
		t.Fatal(err)
	}
	if result.RoundedAmount != 6 {
		t.Fatalf("rule changed after caller mutation: %#v", result)
	}

	tiers := []Tier{{UpTo: 2, Rate: "1"}, {Rate: "2"}}
	graduated := mustRule(t, RuleConfig{Version: "copy-tier-v1", Kind: KindGraduated, Target: Target{Currency: "USD"}, Rounding: RoundDown, Metric: "units", Tiers: tiers})
	tiers[0].Rate = "1000"
	result, err = graduated.Rate(RateInput{Metrics: []MetricQuantity{{Name: "units", Quantity: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	if result.RoundedAmount != 1 {
		t.Fatalf("graduated rule changed after caller mutation: %#v", result)
	}
}

func FuzzWeightedNeverPanicsOrReturnsNegative(f *testing.F) {
	f.Add(int64(1), int64(2), int64(3))
	f.Fuzz(func(t *testing.T, first, second, third int64) {
		rule := mustRule(t, RuleConfig{Version: "fuzz-v1", Kind: KindWeighted, Target: Target{CreditUnit: "units"}, Rounding: RoundDown, Weights: map[string]string{"a": "1/3", "b": "2", "c": "5/7"}})
		result, err := rule.Rate(RateInput{Metrics: []MetricQuantity{{Name: "a", Quantity: nonNegative(first)}, {Name: "b", Quantity: nonNegative(second)}, {Name: "c", Quantity: nonNegative(third)}}})
		if err == nil && result.RoundedAmount < 0 {
			t.Fatalf("negative result: %#v", result)
		}
	})
}

func nonNegative(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}

func mustRule(t *testing.T, config RuleConfig) *Rule {
	t.Helper()
	rule, err := NewRule(config)
	if err != nil {
		t.Fatal(err)
	}
	return rule
}

// A bounded final tier leaves every unit above it unpriced. Before this was
// rejected, tiers [{UpTo:2,Rate:1},{UpTo:4,Rate:2}] priced 1,000,000 units at
// 6 minor units, and the under-billing froze into the settlement batch.
func TestGraduatedRuleRequiresAnUnboundedLastTier(t *testing.T) {
	bounded := RuleConfig{
		Version: "bounded-tail", Kind: KindGraduated, Target: Target{Currency: "USD"},
		Rounding: RoundDown, Metric: "calls",
		Tiers: []Tier{{UpTo: 2, Rate: "1"}, {UpTo: 4, Rate: "2"}},
	}
	if _, err := NewRule(bounded); !errors.Is(err, ErrInvalidRule) {
		t.Fatalf("bounded last tier accepted: err=%v", err)
	}
	unbounded := bounded
	unbounded.Tiers = []Tier{{UpTo: 2, Rate: "1"}, {UpTo: 0, Rate: "2"}}
	rule, err := NewRule(unbounded)
	if err != nil {
		t.Fatalf("unbounded ladder rejected: %v", err)
	}
	result, err := rule.Rate(RateInput{Metrics: []MetricQuantity{{Name: "calls", Quantity: 10}}})
	if err != nil || result.RoundedAmount != 2*1+8*2 {
		t.Fatalf("graduated amount=%d err=%v, want 18", result.RoundedAmount, err)
	}
}
