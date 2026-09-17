package usage

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
)

func TestServiceRateAndRecordReplayAndConflict(t *testing.T) {
	now := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)
	rule := usageTestRule(t, "v1")
	repo := NewMemoryRepository(MemoryConfig{Accounts: []billing.AccountID{"acct"}, Rules: ruleResolver(rule)})
	service := New(repo, func() time.Time { return now })
	o := Observation{Account: "acct", ID: "usage-1", Source: "meter", OccurredAt: now, Funding: Postpaid, Input: RateInput{Metrics: []MetricQuantity{{Name: "tokens", Quantity: 2}}}}
	first, err := service.RateAndRecord(t.Context(), o, rule.Version())
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	replay, err := service.RateAndRecord(t.Context(), o, rule.Version())
	if err != nil || replay.ReceivedAt != first.ReceivedAt {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	o.Input.Metrics[0].Quantity++
	if _, err := service.RateAndRecord(t.Context(), o, rule.Version()); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("changed evidence err=%v, want conflict", err)
	}
}

func TestServiceRecordRejectsForgedRatingAndCanceledContext(t *testing.T) {
	now := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)
	rule := usageTestRule(t, "v1")
	repo := NewMemoryRepository(MemoryConfig{Accounts: []billing.AccountID{"acct"}, Rules: ruleResolver(rule)})
	service := New(repo, func() time.Time { return now })
	record, err := Prepare(Observation{Account: "acct", ID: "usage-1", Source: "meter", OccurredAt: now, Funding: Postpaid, Input: RateInput{Metrics: []MetricQuantity{{Name: "tokens", Quantity: 2}}}}, rule, now)
	if err != nil {
		t.Fatal(err)
	}
	record.Rating.RoundedAmount++
	record.Fingerprint = Identity(record)
	if _, err := service.Record(t.Context(), record); !errors.Is(err, billing.ErrConflict) && !errors.Is(err, billing.ErrInvalid) {
		t.Fatalf("forged rating err=%v, want invalid or conflict", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if result, err := service.Record(ctx, record); !errors.Is(err, context.Canceled) || result.Fingerprint != "" {
		t.Fatalf("canceled result=%+v err=%v", result, err)
	}
}

func TestServiceScansMoreThanOneCandidatePage(t *testing.T) {
	now := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)
	rule := usageTestRule(t, "v1")
	repo := NewMemoryRepository(MemoryConfig{Accounts: []billing.AccountID{"acct"}, Rules: ruleResolver(rule)})
	service := New(repo, func() time.Time { return now })
	for i := range 1005 {
		metric := "noise"
		if i == 1004 {
			metric = "tokens"
		}
		o := Observation{Account: "acct", ID: "usage-" + formatID(i), Source: "meter", OccurredAt: now, Funding: Postpaid, Input: RateInput{Metrics: []MetricQuantity{{Name: metric, Quantity: 1}}}}
		if _, err := service.RateAndRecord(t.Context(), o, rule.Version()); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
	}
	o := Observation{Account: "acct", ID: "target", Source: "meter", OccurredAt: now, Interval: billing.Period{Start: now.Add(-time.Minute), End: now.Add(time.Minute)}, Funding: Postpaid, Input: RateInput{Metrics: []MetricQuantity{{Name: "tokens", Quantity: 1}}}}
	if _, err := service.RateAndRecord(t.Context(), o, rule.Version()); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("overlap beyond first candidate page err=%v, want conflict", err)
	}
}

func TestServiceCommitFailureReturnsZeroAndRollsBack(t *testing.T) {
	now := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)
	rule := usageTestRule(t, "v1")
	base := NewMemoryRepository(MemoryConfig{Accounts: []billing.AccountID{"acct"}, Rules: ruleResolver(rule)}).(*memoryRepository)
	failure := errors.New("commit failed")
	service := New(failingRepository{base: base, err: failure}, func() time.Time { return now })
	o := Observation{Account: "acct", ID: "usage-1", Source: "meter", OccurredAt: now, Funding: Postpaid, Input: RateInput{Metrics: []MetricQuantity{{Name: "tokens", Quantity: 1}}}}
	result, err := service.RateAndRecord(t.Context(), o, rule.Version())
	if !errors.Is(err, failure) || result.Fingerprint != "" {
		t.Fatalf("result=%+v err=%v, want zero result and commit error", result, err)
	}
	if _, ok := base.accounts["acct"][o.ID]; ok {
		t.Fatal("failed transaction leaked staged usage")
	}
}

func TestPrepareScopeAndPrepaidValidation(t *testing.T) {
	now := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)
	rule := usageTestRule(t, "v1")
	base := Observation{Account: "acct", ID: "usage", Source: "meter", OccurredAt: now, Funding: Postpaid, Input: RateInput{Metrics: []MetricQuantity{{Name: "tokens", Quantity: 1}}}}
	if _, err := Prepare(base, rule, now); err != nil {
		t.Fatal(err)
	}
	base.Scope.Subscription.ID = "subscription"
	base.Scope.Subscription.Scope.Provider = "provider"
	base.Scope.Subscription.Scope.Merchant = "merchant"
	base.Scope.Subscription.Scope.Environment = "sandbox"
	if _, err := Prepare(base, rule, now); !errors.Is(err, billing.ErrInvalid) {
		t.Fatalf("missing scoped item err=%v", err)
	}
	base.Scope.ItemID = "item"
	base.Funding = Prepaid
	base.ReservationID = "reservation"
	creditRule, err := NewRule(RuleConfig{Version: "credits", Kind: KindWeighted, Target: Target{CreditUnit: "credits"}, Rounding: RoundDown, Weights: map[string]string{"tokens": "1"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Prepare(base, creditRule, now); err != nil {
		t.Fatal(err)
	}
}

func TestServiceRateAndRecordWaivedTargets(t *testing.T) {
	now := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)
	moneyRule := usageTestRule(t, "waived-money")
	creditRule, err := NewRule(RuleConfig{Version: "waived-credit", Kind: KindWeighted, Target: Target{CreditUnit: "credits"}, Rounding: RoundDown, Weights: map[string]string{"tokens": "1"}})
	if err != nil {
		t.Fatal(err)
	}
	repo := NewMemoryRepository(MemoryConfig{Accounts: []billing.AccountID{"money", "credit"}, Rules: func(ctx context.Context, version string) (*Rule, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		switch version {
		case moneyRule.Version():
			return moneyRule, nil
		case creditRule.Version():
			return creditRule, nil
		default:
			return nil, billing.ErrNotFound
		}
	}})
	service := New(repo, func() time.Time { return now })
	for _, tc := range []struct {
		account string
		id      string
		version string
		credit  bool
	}{
		{account: "money", id: "waived-money", version: moneyRule.Version()},
		{account: "credit", id: "waived-credit", version: creditRule.Version(), credit: true},
	} {
		record, err := service.RateAndRecord(t.Context(), Observation{Account: billing.AccountID(tc.account), ID: tc.id, Source: "meter", OccurredAt: now, Funding: Waived, Input: RateInput{Metrics: []MetricQuantity{{Name: "tokens", Quantity: 2}}, Waived: true}}, tc.version)
		if err != nil {
			t.Fatalf("%s: %v", tc.account, err)
		}
		if record.Rating.RoundedAmount != 0 || (tc.credit && (record.Rating.Money != nil || record.Rating.Credits == nil)) || (!tc.credit && (record.Rating.Money == nil || record.Rating.Credits != nil)) {
			t.Fatalf("%s: unexpected result %+v", tc.account, record.Rating)
		}
		stored, err := service.Usage(t.Context(), billing.AccountID(tc.account), tc.id)
		if err != nil || stored.Fingerprint != record.Fingerprint {
			t.Fatalf("%s roundtrip=%+v err=%v", tc.account, stored, err)
		}
	}
}

func usageTestRule(t *testing.T, version string) *Rule {
	t.Helper()
	rule, err := NewRule(RuleConfig{Version: version, Kind: KindWeighted, Target: Target{Currency: "USD"}, Rounding: RoundDown, Weights: map[string]string{"tokens": "1", "noise": "1"}})
	if err != nil {
		t.Fatal(err)
	}
	return rule
}

func ruleResolver(rule *Rule) RuleResolver {
	return func(ctx context.Context, version string) (*Rule, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if version != rule.Version() {
			return nil, billing.ErrNotFound
		}
		return rule, nil
	}
}

type failingRepository struct {
	base *memoryRepository
	err  error
}

func (r failingRepository) WithinAccount(ctx context.Context, account billing.AccountID, fn func(Tx) error) error {
	r.base.mu.Lock()
	defer r.base.mu.Unlock()
	rows, ok := r.base.accounts[account]
	if !ok {
		return billing.ErrNotFound
	}
	if err := fn(&memoryTx{repo: r.base, account: account, rows: cloneRows(rows), costs: cloneCosts(r.base.costs[account])}); err != nil {
		return err
	}
	return r.err
}

func formatID(i int) string {
	return "" + strconv.Itoa(10000+i)
}
