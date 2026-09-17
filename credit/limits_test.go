package credit

import (
	"errors"
	"sync"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
)

func testPeriod() billing.Period {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	return billing.Period{Start: start, End: start.Add(24 * time.Hour)}
}

func TestConcurrentAccountActorProjectBudgetsCannotExceedCap(t *testing.T) {
	repo := NewMemoryLimitRepository("acct")
	svc := NewLimits(repo, func() time.Time { return testPeriod().Start })
	period := testPeriod()
	for _, in := range []BudgetInput{
		{Account: "acct", ID: "account-cap", Basis: BasisBillable, Currency: "USD", Period: period, Amount: 10},
		{Account: "acct", ID: "actor-cap", Actor: "actor-1", Basis: BasisBillable, Currency: "USD", Period: period, Amount: 7},
		{Account: "acct", ID: "project-cap", Project: "proj-1", Basis: BasisBillable, Currency: "USD", Period: period, Amount: 6},
	} {
		if _, err := svc.Configure(t.Context(), in); err != nil {
			t.Fatal(err)
		}
	}
	first, err := svc.Reserve(t.Context(), BudgetReserveInput{Account: "acct", GroupID: "g1", Actor: "actor-1", Project: "proj-1", Basis: BasisBillable, Currency: "USD", Amount: 5, Period: period})
	if err != nil || len(first.Holds) != 3 {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	if _, err := svc.Reserve(t.Context(), BudgetReserveInput{Account: "acct", GroupID: "g2", Actor: "actor-1", Project: "proj-1", Basis: BasisBillable, Currency: "USD", Amount: 2, Period: period}); !errors.Is(err, billing.ErrLimit) {
		t.Fatalf("over project cap err=%v, want limit", err)
	}
	replay, err := svc.Reserve(t.Context(), BudgetReserveInput{Account: "acct", GroupID: "g1", Actor: "actor-1", Project: "proj-1", Basis: BasisBillable, Currency: "USD", Amount: 5, Period: period})
	if err != nil || len(replay.Holds) != 3 {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}

	race := NewLimits(NewMemoryLimitRepository("race"), nil)
	if _, err := race.Configure(t.Context(), BudgetInput{Account: "race", ID: "cap", Basis: BasisBillable, Currency: "USD", Period: period, Amount: 10}); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, id := range []string{"race-a", "race-b"} {
		wg.Go(func() {
			<-start
			_, err := race.Reserve(t.Context(), BudgetReserveInput{Account: "race", GroupID: id, Basis: BasisBillable, Currency: "USD", Amount: 8, Period: period})
			errs <- err
		})
	}
	close(start)
	wg.Wait()
	close(errs)
	var success, limited int
	for err := range errs {
		if err == nil {
			success++
		} else if errors.Is(err, billing.ErrLimit) {
			limited++
		} else {
			t.Fatalf("race err=%v", err)
		}
	}
	if success != 1 || limited != 1 {
		t.Fatalf("race success=%d limited=%d", success, limited)
	}
}

func TestBudgetBasisAndUnknownCostBound(t *testing.T) {
	svc := NewLimits(NewMemoryLimitRepository("acct"), nil)
	period := testPeriod()
	if _, err := svc.Configure(t.Context(), BudgetInput{Account: "acct", ID: "billable", Basis: BasisBillable, Currency: "USD", Period: period, Amount: 100}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Configure(t.Context(), BudgetInput{Account: "acct", ID: "internal", Basis: BasisInternal, Currency: "USD", Period: period, Amount: 8}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Configure(t.Context(), BudgetInput{Account: "acct", ID: "topup", Basis: BasisTopUp, Currency: "USD", Period: period, Amount: 50}); err != nil {
		t.Fatal(err)
	}
	billable, err := svc.Reserve(t.Context(), BudgetReserveInput{Account: "acct", GroupID: "bill", Basis: BasisBillable, Currency: "USD", Amount: 9, Period: period})
	if err != nil || billable.Bound != 9 {
		t.Fatalf("billable=%+v err=%v", billable, err)
	}
	if _, err := svc.Reserve(t.Context(), BudgetReserveInput{Account: "acct", GroupID: "cost", Basis: BasisInternal, Currency: "USD", Amount: 9, Period: period}); !errors.Is(err, billing.ErrLimit) {
		t.Fatalf("internal cap ignored err=%v", err)
	}
	internal, err := svc.Reserve(t.Context(), BudgetReserveInput{Account: "acct", GroupID: "cost-ok", Basis: BasisInternal, Currency: "USD", Amount: 8, Period: period})
	if err != nil || internal.Bound != 8 {
		t.Fatalf("internal bound=%+v err=%v", internal, err)
	}
	if _, err := svc.Reserve(t.Context(), BudgetReserveInput{Account: "acct", GroupID: "unbounded", Basis: BasisBillable, Currency: "USD", Amount: 0, Period: period}); !errors.Is(err, billing.ErrInvalid) {
		t.Fatalf("missing bound err=%v, want invalid", err)
	}
	if _, err := svc.Settle(t.Context(), BudgetSettleInput{Account: "acct", GroupID: "cost-ok", Actual: 9}); !errors.Is(err, billing.ErrLimit) {
		t.Fatalf("settle above bound err=%v", err)
	}
	settled, err := svc.Settle(t.Context(), BudgetSettleInput{Account: "acct", GroupID: "cost-ok", Actual: 3})
	if err != nil || settled.Holds[0].Settled != 3 {
		t.Fatalf("settle=%+v err=%v", settled, err)
	}
}

func TestRateLimitEntitlementQuotaAndBudgetStayIndependent(t *testing.T) {
	limiter := NewMemoryLimiter()
	svc := NewLimits(NewMemoryLimitRepository("acct"), nil).WithRateLimiter(limiter)
	period := testPeriod()
	if _, err := svc.Configure(t.Context(), BudgetInput{Account: "acct", ID: "cap", Basis: BasisBillable, Currency: "USD", Period: period, Amount: 10}); err != nil {
		t.Fatal(err)
	}
	limit := RateLimitInput{Key: "acct-actor-api", Window: period, Limit: 1}
	if _, err := svc.Authorize(t.Context(), BudgetReserveInput{Account: "acct", GroupID: "auth-1", Basis: BasisBillable, Currency: "USD", Amount: 1, Period: period}, limit); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authorize(t.Context(), BudgetReserveInput{Account: "acct", GroupID: "auth-2", Basis: BasisBillable, Currency: "USD", Amount: 1, Period: period}, limit); !errors.Is(err, billing.ErrLimit) {
		t.Fatalf("rate limit err=%v, want limit", err)
	}
	if _, err := svc.Reserve(t.Context(), BudgetReserveInput{Account: "acct", GroupID: "auth-2", Basis: BasisBillable, Currency: "USD", Amount: 1, Period: period}); err != nil {
		t.Fatalf("budget reserve after rate-limit deny err=%v", err)
	}
	if _, err := svc.Reserve(t.Context(), BudgetReserveInput{Account: "acct", GroupID: "over", Basis: BasisBillable, Currency: "USD", Amount: 20, Period: period}); !errors.Is(err, billing.ErrLimit) {
		t.Fatalf("budget deny err=%v", err)
	}
	if err := limiter.Allow(t.Context(), RateLimitInput{Key: "other-key", Window: period, Limit: 1}); err != nil {
		t.Fatalf("independent rate key err=%v", err)
	}
}

func TestHybridFallbackIsExplicitAndBounded(t *testing.T) {
	svc := NewLimits(NewMemoryLimitRepository("acct"), nil)
	period := testPeriod()
	if _, err := svc.Configure(t.Context(), BudgetInput{Account: "acct", ID: "debt", Basis: BasisBillable, Currency: "USD", Period: period, Amount: 20}); err != nil {
		t.Fatal(err)
	}
	in := HybridBudgetReserveInput{BudgetReserveInput: BudgetReserveInput{Account: "acct", GroupID: "hyb-off", Basis: BasisBillable, Currency: "USD", Amount: 10, Period: period}, PrepaidAvailable: 3}
	if _, err := svc.ReserveHybrid(t.Context(), in); !errors.Is(err, billing.ErrInsufficient) {
		t.Fatalf("disabled hybrid err=%v, want insufficient", err)
	}
	in.Policy = HybridPolicy{Enabled: true, MaxDebt: 5, Basis: BasisBillable, Currency: "USD"}
	if _, err := svc.ReserveHybrid(t.Context(), in); !errors.Is(err, billing.ErrLimit) {
		t.Fatalf("debt 7 over max 5 err=%v, want limit", err)
	}
	in.GroupID = "hyb-ok"
	in.Policy.MaxDebt = 8
	got, err := svc.ReserveHybrid(t.Context(), in)
	if err != nil || got.Bound != 7 || len(got.Holds) != 1 {
		t.Fatalf("hybrid debt=%+v err=%v", got, err)
	}
	covered := HybridBudgetReserveInput{BudgetReserveInput: BudgetReserveInput{Account: "acct", GroupID: "hyb-prepaid", Basis: BasisBillable, Currency: "USD", Amount: 3, Period: period}, PrepaidAvailable: 10, Policy: HybridPolicy{Enabled: true, MaxDebt: 8, Basis: BasisBillable, Currency: "USD"}}
	none, err := svc.ReserveHybrid(t.Context(), covered)
	if err != nil || none.Bound != 0 || len(none.Holds) != 0 {
		t.Fatalf("covered prepaid created debt: %+v err=%v", none, err)
	}
}
