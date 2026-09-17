package host_test

import (
	"context"
	"errors"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/usage"
)

func TestHostConstructsUsageServiceWithAuthoritativeRuleResolver(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	rule, err := usage.NewRule(usage.RuleConfig{Version: "host-usage-rule", Kind: usage.KindFixed, Target: usage.Target{Currency: "USD"}, Rounding: usage.RoundDown, FixedRate: "2"})
	if err != nil {
		t.Fatal(err)
	}
	repo := usage.NewMemoryRepository(usage.MemoryConfig{
		Accounts: []billing.AccountID{"host-usage"},
		Rules: func(_ context.Context, version string) (*usage.Rule, error) {
			if version != rule.Version() {
				return nil, billing.ErrNotFound
			}
			return rule, nil
		},
	})
	service := usage.New(repo, func() time.Time { return now })
	record, err := service.RateAndRecord(t.Context(), usage.Observation{Account: "host-usage", ID: "host-usage-1", Source: "meter", OccurredAt: now, Funding: usage.Postpaid, Input: usage.RateInput{ActionCount: 1}}, rule.Version())
	if err != nil || record.Rating.RuleVersion != rule.Version() {
		t.Fatalf("record=%+v error=%v", record, err)
	}
	if _, err := service.RateAndRecord(t.Context(), usage.Observation{Account: "host-usage", ID: "host-usage-2", Source: "meter", OccurredAt: now, Funding: usage.Postpaid, Input: usage.RateInput{ActionCount: 1}}, "unknown-rule"); !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("unknown authoritative rule error=%v, want not found", err)
	}
}
