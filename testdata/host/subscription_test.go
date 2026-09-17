package host_test

import (
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/catalog"
	"github.com/data-insights-ai/rho-billing/subscription"
)

func TestHostActivatesFreeTrialWithoutProviderPayment(t *testing.T) {
	accounts := catalog.NewMemoryAccountRepository()
	if err := accounts.CreateAccount(t.Context(), "tenant-trial", "organization"); err != nil {
		t.Fatal(err)
	}
	svc := subscription.New(subscription.NewMemoryRepository(subscription.MemoryConfig{Accounts: accounts}))
	start := time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)
	life, err := svc.Activate(t.Context(), subscription.ActivateInput{
		Account: "tenant-trial", ID: "life-trial", Operation: "activate-trial", Quantity: 1, Trial: true,
		Items: []subscription.Item{{ID: "seat", PlanVersion: "trial-plan", Quantity: 1, Period: billing.Period{Start: start, End: start.AddDate(0, 0, 14)}}},
		Policies: subscription.Policies{
			Access: subscription.AccessImmediate, Collection: subscription.CollectionNone,
			Proration: subscription.ProrationNone, Allowance: subscription.AllowanceKeepPeriod,
		},
		Coverage: billing.Period{Start: start, End: start.AddDate(0, 0, 14)}, At: start,
	})
	if err != nil || life.Access != subscription.AccessTrial || life.Collection != subscription.CollectionIdle {
		t.Fatalf("trial=%+v err=%v", life, err)
	}
}

func TestHostQuantityRevisionSurvivesLateProviderSnapshot(t *testing.T) {
	accounts := catalog.NewMemoryAccountRepository()
	if err := accounts.CreateAccount(t.Context(), "tenant-seats", "organization"); err != nil {
		t.Fatal(err)
	}
	svc := subscription.New(subscription.NewMemoryRepository(subscription.MemoryConfig{Accounts: accounts}))
	start := time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	if _, err := svc.Activate(t.Context(), subscription.ActivateInput{
		Account: "tenant-seats", ID: "life-seats", Operation: "activate-seats", Quantity: 2,
		Items: []subscription.Item{{ID: "seat", PlanVersion: "team-plan", Quantity: 2, Period: billing.Period{Start: start, End: end}}},
		Policies: subscription.Policies{
			Access: subscription.AccessImmediate, Collection: subscription.CollectionAutomatic,
			Proration: subscription.ProrationImmediate, Allowance: subscription.AllowanceKeepPeriod,
		},
		Coverage: billing.Period{Start: start, End: end}, At: start,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := svc.SetDesiredQuantity(t.Context(), subscription.QuantityInput{Account: "tenant-seats", ID: "life-seats", Operation: "seats-5", Quantity: 5, Revision: 2, At: start.Add(time.Minute)})
	if err != nil || got.DesiredQuantity != 5 || got.QuantityRevision != 2 {
		t.Fatalf("desired=%+v err=%v", got, err)
	}
}
