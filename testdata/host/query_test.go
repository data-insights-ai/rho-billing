package host_test

import (
	"testing"
	"time"

	"github.com/data-insights-ai/rho-billing/catalog"
	"github.com/data-insights-ai/rho-billing/query"
)

func TestHostReadsCustomerOverviewThroughQueryAPI(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	accounts := catalog.NewMemoryAccountRepository()
	if err := accounts.CreateAccount(t.Context(), "host-query", "org"); err != nil {
		t.Fatal(err)
	}
	svc := query.New(query.Deps{Accounts: accounts}, func() time.Time { return now })
	got, err := svc.CustomerOverview(t.Context(), "host-query")
	if err != nil || got.Account.ID != "host-query" || got.AsOf.IsZero() {
		t.Fatalf("overview=%+v err=%v", got, err)
	}
}
