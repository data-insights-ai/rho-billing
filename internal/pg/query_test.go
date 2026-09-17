package pg

import (
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/catalog"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/query"
	"github.com/data-insights-ai/rho-billing/usage"
)

func TestPostgresStatementAndOverview(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	acct := billing.AccountID("lib51-acct")
	if err := store.CreateAccount(ctx, acct, "lib51-subject"); err != nil {
		t.Fatal(err)
	}
	period := settlementTestPeriod()
	record := settlementTestRecord(t, store, string(acct), "stmt-usage", period.Start.Add(time.Hour), period.Start.Add(2*time.Hour), 4)
	if _, err := recordUsage(ctx, store, record); err != nil {
		t.Fatal(err)
	}
	closed, err := finishSettlementClose(ctx, store.Settlements(), store.now, usage.CloseInput{Account: acct, Operation: "lib51-close", BatchID: "lib51-batch", Period: period, Currency: "USD", CreatedAt: period.Cutoff})
	if err != nil || closed.Total != 4 {
		t.Fatalf("closed=%+v err=%v", closed, err)
	}
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	svc := query.New(query.Deps{
		Accounts:    store,
		Credits:     credit.New(store, func() time.Time { return now }),
		Usage:       usage.New(store.UsageRepository(), func() time.Time { return now }),
		Estimates:   store,
		Settlements: usage.NewSettlement(store.Settlements(), func() time.Time { return now }),
		CreditUnit:  "credits",
	}, func() time.Time { return now })
	overview, err := svc.CustomerOverview(ctx, acct)
	if err != nil || overview.Account.ID != acct {
		t.Fatalf("overview=%+v err=%v", overview, err)
	}
	stmt, err := svc.Statement(ctx, acct, closed.ID)
	if err != nil || stmt.Total != 4 || len(stmt.Lines) != 1 || stmt.Lines[0].UsageID != "stmt-usage" || stmt.Pending {
		t.Fatalf("statement=%+v err=%v", stmt, err)
	}
	est, err := svc.Estimate(ctx, usage.EstimateInput{Account: acct, Period: usage.EstimatePeriod{Start: period.Start, End: period.End, Cutoff: period.Cutoff}, Currency: "USD", BatchID: closed.ID, Limit: 10})
	if err != nil || est.Total != 4 || est.Status != usage.StatusFinalized {
		t.Fatalf("estimate=%+v err=%v", est, err)
	}
}

func TestPostgresUsagePageIgnoresWritesAfterSnapshot(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	acct := billing.AccountID("lib52-acct")
	if err := store.CreateAccount(ctx, acct, "lib52-subject"); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	clock := now
	usageSvc := usage.New(store.UsageRepository(), func() time.Time { return clock })
	record := settlementTestRecord(t, store, string(acct), "snap-1", now, now, 1)
	if _, err := usageSvc.Record(ctx, record); err != nil {
		t.Fatal(err)
	}
	svc := query.New(query.Deps{Accounts: store, Usage: usageSvc}, func() time.Time { return clock })
	first, err := svc.UsagePage(ctx, acct, billing.Period{Start: now.Add(-time.Hour), End: now.Add(24 * time.Hour)}, query.Cursor{}, 10)
	if err != nil || len(first.Records) != 1 {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	clock = now.Add(time.Hour)
	late := settlementTestRecord(t, store, string(acct), "snap-2", now.Add(time.Minute), clock, 1)
	if _, err := usageSvc.Record(ctx, late); err != nil {
		t.Fatal(err)
	}
	second, err := svc.UsagePage(ctx, acct, billing.Period{Start: now.Add(-time.Hour), End: now.Add(24 * time.Hour)}, query.Cursor{Snapshot: first.Snapshot, After: first.NextAfter}, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range second.Records {
		if row.Observation.ID == "snap-2" {
			t.Fatal("post-snapshot usage leaked")
		}
	}
}

var _ catalog.AccountRepository = (*Store)(nil)
