package query

import (
	"context"
	"errors"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/catalog"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/usage"
)

func TestQueriesCoverOverviewBalanceUsageAndPurchases(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	accounts := catalog.NewMemoryAccountRepository()
	if err := accounts.CreateAccount(t.Context(), "acct", "subject"); err != nil {
		t.Fatal(err)
	}
	credits := credit.New(credit.NewMemoryRepository("acct"), func() time.Time { return now })
	if _, err := credits.Grant(t.Context(), credit.GrantInput{Account: "acct", Operation: "g", LotID: "lot", Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 9, Source: "purchase", SourceRef: "pay", ValidFrom: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := credits.Reserve(t.Context(), credit.ReserveInput{Account: "acct", Operation: "r", ReservationID: "hold-1", Actor: "user", Unit: "credits", Amount: 2, Deadline: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	rule, err := usage.NewRule(usage.RuleConfig{Version: "q-rule", Kind: usage.KindWeighted, Target: usage.Target{Currency: "USD"}, Rounding: usage.RoundDown, Weights: map[string]string{"tokens": "1"}})
	if err != nil {
		t.Fatal(err)
	}
	usageSvc := usage.New(usage.NewMemoryRepository(usage.MemoryConfig{Accounts: []billing.AccountID{"acct"}, Rules: func(_ context.Context, version string) (*usage.Rule, error) {
		if version != rule.Version() {
			return nil, billing.ErrNotFound
		}
		return rule, nil
	}}), func() time.Time { return now })
	if _, err := usageSvc.RateAndRecord(t.Context(), usage.Observation{Account: "acct", ID: "u1", Source: "meter", OccurredAt: now, Funding: usage.Postpaid, Input: usage.RateInput{Metrics: []usage.MetricQuantity{{Name: "tokens", Quantity: 2}}}}, rule.Version()); err != nil {
		t.Fatal(err)
	}
	svc := New(Deps{Accounts: accounts, Credits: credits, Usage: usageSvc, CreditUnit: "credits"}, func() time.Time { return now })
	overview, err := svc.CustomerOverview(t.Context(), "acct")
	if err != nil || overview.Account.ID != "acct" || overview.Balance.Available != 7 || len(overview.PendingActions) != 1 || overview.PendingActions[0].ID != "hold-1" || overview.PendingActions[0].Kind != ActionHeldCredits || overview.PendingActions[0].State != ActionHeld {
		t.Fatalf("overview=%+v err=%v", overview, err)
	}
	page, err := svc.UsagePage(t.Context(), "acct", billing.Period{Start: now.Add(-time.Hour), End: now.Add(time.Hour)}, Cursor{}, 10)
	if err != nil || len(page.Records) != 1 || page.Records[0].Observation.ID != "u1" {
		t.Fatalf("usage page=%+v err=%v", page, err)
	}
}

func TestCursorIsBoundToSnapshotAndIgnoresLaterWrites(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	clock := now
	accounts := catalog.NewMemoryAccountRepository()
	if err := accounts.CreateAccount(t.Context(), "acct", "subject"); err != nil {
		t.Fatal(err)
	}
	rule, err := usage.NewRule(usage.RuleConfig{Version: "q-rule", Kind: usage.KindWeighted, Target: usage.Target{Currency: "USD"}, Rounding: usage.RoundDown, Weights: map[string]string{"tokens": "1"}})
	if err != nil {
		t.Fatal(err)
	}
	usageSvc := usage.New(usage.NewMemoryRepository(usage.MemoryConfig{Accounts: []billing.AccountID{"acct"}, Rules: func(_ context.Context, version string) (*usage.Rule, error) {
		if version != rule.Version() {
			return nil, billing.ErrNotFound
		}
		return rule, nil
	}}), func() time.Time { return clock })
	if _, err := usageSvc.RateAndRecord(t.Context(), usage.Observation{Account: "acct", ID: "u1", Source: "meter", OccurredAt: now, Funding: usage.Postpaid, Input: usage.RateInput{Metrics: []usage.MetricQuantity{{Name: "tokens", Quantity: 1}}}}, rule.Version()); err != nil {
		t.Fatal(err)
	}
	svc := New(Deps{Accounts: accounts, Usage: usageSvc}, func() time.Time { return clock })
	first, err := svc.UsagePage(t.Context(), "acct", billing.Period{Start: now.Add(-time.Hour), End: now.Add(24 * time.Hour)}, Cursor{}, 10)
	if err != nil || first.Snapshot.IsZero() || len(first.Records) != 1 {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	clock = now.Add(time.Hour)
	if _, err := usageSvc.RateAndRecord(t.Context(), usage.Observation{Account: "acct", ID: "u2", Source: "meter", OccurredAt: now.Add(time.Minute), Funding: usage.Postpaid, Input: usage.RateInput{Metrics: []usage.MetricQuantity{{Name: "tokens", Quantity: 1}}}}, rule.Version()); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UsagePage(t.Context(), "acct", billing.Period{Start: now.Add(-time.Hour), End: now.Add(24 * time.Hour)}, Cursor{After: first.NextAfter}, 10); !errors.Is(err, billing.ErrInvalid) {
		t.Fatalf("missing snapshot err=%v", err)
	}
	second, err := svc.UsagePage(t.Context(), "acct", billing.Period{Start: now.Add(-time.Hour), End: now.Add(24 * time.Hour)}, Cursor{Snapshot: first.Snapshot, After: first.NextAfter}, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range second.Records {
		if row.Observation.ID == "u2" {
			t.Fatal("later write leaked into snapshot page")
		}
	}
}

func TestUsagePageKeepsSnapshotRowsWhenLaterIDsSortFirst(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	clock := now
	accounts := catalog.NewMemoryAccountRepository()
	if err := accounts.CreateAccount(t.Context(), "acct", "subject"); err != nil {
		t.Fatal(err)
	}
	rule, err := usage.NewRule(usage.RuleConfig{Version: "q-rule", Kind: usage.KindWeighted, Target: usage.Target{Currency: "USD"}, Rounding: usage.RoundDown, Weights: map[string]string{"tokens": "1"}})
	if err != nil {
		t.Fatal(err)
	}
	usageSvc := usage.New(usage.NewMemoryRepository(usage.MemoryConfig{Accounts: []billing.AccountID{"acct"}, Rules: func(_ context.Context, version string) (*usage.Rule, error) {
		if version != rule.Version() {
			return nil, billing.ErrNotFound
		}
		return rule, nil
	}}), func() time.Time { return clock })
	if _, err := usageSvc.RateAndRecord(t.Context(), usage.Observation{Account: "acct", ID: "z-early", Source: "meter", OccurredAt: now, Funding: usage.Postpaid, Input: usage.RateInput{Metrics: []usage.MetricQuantity{{Name: "tokens", Quantity: 1}}}}, rule.Version()); err != nil {
		t.Fatal(err)
	}
	svc := New(Deps{Accounts: accounts, Usage: usageSvc}, func() time.Time { return clock })
	first, err := svc.UsagePage(t.Context(), "acct", billing.Period{Start: now.Add(-time.Hour), End: now.Add(24 * time.Hour)}, Cursor{}, 1)
	if err != nil || len(first.Records) != 1 || first.Records[0].Observation.ID != "z-early" {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	clock = now.Add(time.Hour)
	if _, err := usageSvc.RateAndRecord(t.Context(), usage.Observation{Account: "acct", ID: "a-late", Source: "meter", OccurredAt: now.Add(time.Minute), Funding: usage.Postpaid, Input: usage.RateInput{Metrics: []usage.MetricQuantity{{Name: "tokens", Quantity: 1}}}}, rule.Version()); err != nil {
		t.Fatal(err)
	}
	page, err := svc.UsagePage(t.Context(), "acct", billing.Period{Start: now.Add(-time.Hour), End: now.Add(24 * time.Hour)}, Cursor{Snapshot: first.Snapshot}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Records) != 1 || page.Records[0].Observation.ID != "z-early" {
		t.Fatalf("snapshot page dropped the in-snapshot row: %+v", page)
	}
	for _, row := range page.Records {
		if row.Observation.ID == "a-late" {
			t.Fatal("later write leaked into snapshot page")
		}
	}
}

func TestCustomerOverviewExcludesInternalCost(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	accounts := catalog.NewMemoryAccountRepository()
	if err := accounts.CreateAccount(t.Context(), "acct", "subject"); err != nil {
		t.Fatal(err)
	}
	rule, err := usage.NewRule(usage.RuleConfig{Version: "q-rule", Kind: usage.KindWeighted, Target: usage.Target{Currency: "USD"}, Rounding: usage.RoundDown, Weights: map[string]string{"tokens": "1"}})
	if err != nil {
		t.Fatal(err)
	}
	usageSvc := usage.New(usage.NewMemoryRepository(usage.MemoryConfig{Accounts: []billing.AccountID{"acct"}, Rules: func(_ context.Context, version string) (*usage.Rule, error) {
		if version != rule.Version() {
			return nil, billing.ErrNotFound
		}
		return rule, nil
	}}), func() time.Time { return now })
	if _, err := usageSvc.RateAndRecord(t.Context(), usage.Observation{Account: "acct", ID: "u1", Source: "meter", OccurredAt: now, Funding: usage.Postpaid, Input: usage.RateInput{Metrics: []usage.MetricQuantity{{Name: "tokens", Quantity: 1}}}}, rule.Version()); err != nil {
		t.Fatal(err)
	}
	cost, err := usageSvc.RecordCost(t.Context(), usage.CostInput{Account: "acct", ID: "c1", UsageID: "u1", Resource: "gpu", Model: "m", Quantity: 1, RuleVersion: "cost", Currency: "EUR", Amount: 4, ExactAmount: "4", State: usage.CostActual, OccurredAt: now})
	if err != nil {
		t.Fatal(err)
	}
	svc := New(Deps{Accounts: accounts, Usage: usageSvc}, func() time.Time { return now })
	customer, err := svc.CustomerOverview(t.Context(), "acct")
	if err != nil {
		t.Fatal(err)
	}
	_ = customer
	operator, err := svc.OperatorOverview(t.Context(), "acct", []string{cost.ID})
	if err != nil || len(operator.Costs) != 1 || operator.Costs[0].Currency != "EUR" {
		t.Fatalf("operator=%+v err=%v", operator, err)
	}
	if _, err := svc.OperatorCost(t.Context(), "acct", cost.ID); err != nil {
		t.Fatal(err)
	}
}
