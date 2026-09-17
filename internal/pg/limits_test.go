package pg

import (
	"errors"
	"sync"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/purchase"
)

func TestPostgresBudgetCapsAndBasis(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("lib45-acct")
	if err := store.CreateAccount(ctx, account, "lib45-subject"); err != nil {
		t.Fatal(err)
	}
	period := billing.Period{Start: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
	svc := credit.NewLimits(store.Limits(), nil)
	if _, err := svc.Configure(ctx, credit.BudgetInput{Account: account, ID: "account", Basis: credit.BasisBillable, Currency: "USD", Period: period, Amount: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Configure(ctx, credit.BudgetInput{Account: account, ID: "internal", Basis: credit.BasisInternal, Currency: "USD", Period: period, Amount: 4}); err != nil {
		t.Fatal(err)
	}
	got, err := svc.Reserve(ctx, credit.BudgetReserveInput{Account: account, GroupID: "g1", Basis: credit.BasisBillable, Currency: "USD", Amount: 8, Period: period})
	if err != nil || got.Bound != 8 {
		t.Fatalf("reserve=%+v err=%v", got, err)
	}
	if _, err := svc.Reserve(ctx, credit.BudgetReserveInput{Account: account, GroupID: "g2", Basis: credit.BasisBillable, Currency: "USD", Amount: 3, Period: period}); !errors.Is(err, billing.ErrLimit) {
		t.Fatalf("account cap err=%v", err)
	}
	if _, err := svc.Reserve(ctx, credit.BudgetReserveInput{Account: account, GroupID: "cost", Basis: credit.BasisInternal, Currency: "USD", Amount: 5, Period: period}); !errors.Is(err, billing.ErrLimit) {
		t.Fatalf("internal basis err=%v", err)
	}

	raceAccount := billing.AccountID("lib45-race")
	if err := store.CreateAccount(ctx, raceAccount, "lib45-race-subject"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Configure(ctx, credit.BudgetInput{Account: raceAccount, ID: "cap", Basis: credit.BasisBillable, Currency: "USD", Period: period, Amount: 10}); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	second := New(secondQueueDB(t, store.db))
	for i, id := range []string{"race-a", "race-b"} {
		repo := store.Limits()
		if i == 1 {
			repo = second.Limits()
		}
		wg.Go(func() {
			<-start
			_, err := credit.NewLimits(repo, nil).Reserve(ctx, credit.BudgetReserveInput{Account: raceAccount, GroupID: id, Basis: credit.BasisBillable, Currency: "USD", Amount: 8, Period: period})
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

func TestPostgresTopUpUnknownBlocksAndGrantOnce(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("lib49-acct")
	if err := store.CreateAccount(ctx, account, "lib49-subject"); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	svc := purchase.New(store.Purchases(), func() time.Time { return now })
	policy := purchase.TopUpPolicy{Account: account, ID: "auto", ConsentRevision: 1, ConsentedAt: now, ConsentActor: "owner", Threshold: 5, Cooldown: time.Hour, PurchaseCap: 500, Currency: "USD", Amount: 100, Unit: billing.Unit{Code: "credits", Scale: 1}, Enabled: true}
	if _, err := svc.ConfigureTopUp(ctx, policy); err != nil {
		t.Fatal(err)
	}
	attempt, err := svc.EvaluateTopUp(ctx, account, "auto", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.EvaluateTopUp(ctx, account, "auto", 0); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("second attempt err=%v", err)
	}
	if _, err := svc.DispatchTopUp(ctx, account, attempt.ID, "intent-pending"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ObserveTopUp(ctx, account, attempt.ID, purchase.TopUpUnknown); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.EvaluateTopUp(ctx, account, "auto", 0); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("unknown still active err=%v", err)
	}
	if _, err := svc.GrantTopUp(ctx, account, attempt.ID); !errors.Is(err, billing.ErrState) {
		t.Fatalf("grant unknown err=%v", err)
	}
}
