package host_test

import (
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/purchase"
)

func TestHostReservesBudgetAndConfiguresConsentedTopUp(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	period := billing.Period{Start: now, End: now.Add(24 * time.Hour)}
	budgets := credit.NewLimits(credit.NewMemoryLimitRepository("host-limits"), func() time.Time { return now })
	if _, err := budgets.Configure(t.Context(), credit.BudgetInput{Account: "host-limits", ID: "cap", Basis: credit.BasisBillable, Currency: "USD", Period: period, Amount: 20}); err != nil {
		t.Fatal(err)
	}
	hold, err := budgets.Reserve(t.Context(), credit.BudgetReserveInput{Account: "host-limits", GroupID: "host-hold", Basis: credit.BasisBillable, Currency: "USD", Amount: 5, Period: period})
	if err != nil || hold.Bound != 5 {
		t.Fatalf("reserve=%+v err=%v", hold, err)
	}
	purchases := purchase.New(purchase.NewMemoryRepository(purchase.ReferenceAccount{Account: "host-limits"}), func() time.Time { return now })
	policy, err := purchases.ConfigureTopUp(t.Context(), purchase.TopUpPolicy{Account: "host-limits", ID: "auto", ConsentRevision: 1, ConsentedAt: now, ConsentActor: "owner", Threshold: 2, Cooldown: time.Hour, PurchaseCap: 50, Currency: "USD", Amount: 10, Unit: billing.Unit{Code: "credits", Scale: 1}, Enabled: true})
	if err != nil || !policy.Enabled {
		t.Fatalf("policy=%+v err=%v", policy, err)
	}
}
