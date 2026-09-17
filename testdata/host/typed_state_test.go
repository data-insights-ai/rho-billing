package host_test

import (
	"errors"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/catalog"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/integration"
	"github.com/data-insights-ai/rho-billing/postgres"
	"github.com/data-insights-ai/rho-billing/query"
	"github.com/data-insights-ai/rho-billing/usage"
)

func TestHostReadsTypedClosedSetStatesThroughPublicPorts(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	account := billing.AccountID("host-typed")
	accounts := catalog.NewMemoryAccountRepository()
	if err := accounts.CreateAccount(t.Context(), account, "org"); err != nil {
		t.Fatal(err)
	}
	credits := credit.New(credit.NewMemoryRepository(account), func() time.Time { return now })
	if _, err := credits.Grant(t.Context(), credit.GrantInput{Account: account, Operation: "g", LotID: "lot", Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 9, Source: "purchase", SourceRef: "pay", ValidFrom: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := credits.Reserve(t.Context(), credit.ReserveInput{Account: account, Operation: "r", ReservationID: "hold-1", Actor: "user", Unit: "credits", Amount: 2, Deadline: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	overview, err := query.New(query.Deps{Accounts: accounts, Credits: credits, CreditUnit: "credits"}, func() time.Time { return now }).CustomerOverview(t.Context(), account)
	if err != nil || len(overview.PendingActions) != 1 || overview.PendingActions[0].State != query.ActionHeld || overview.PendingActions[0].Kind != query.ActionHeldCredits {
		t.Fatalf("overview=%+v err=%v", overview, err)
	}

	ops := integration.NewOps(integration.NewMemoryOpsRepository(account), func() time.Time { return now })
	running, err := ops.AdvanceBackfill(t.Context(), integration.BackfillInput{Account: account, ID: "bf-1", Limit: 10})
	if err != nil || running.State != integration.BackfillRunning {
		t.Fatalf("backfill running=%+v err=%v", running, err)
	}
	done, err := ops.FinishBackfill(t.Context(), account, "bf-1")
	if err != nil || done.State != integration.BackfillDone {
		t.Fatalf("backfill done=%+v err=%v", done, err)
	}

	line := usage.ChargeLine{Kind: usage.ChargeUsage, UsageID: "usage-1"}
	if line.Kind != usage.ChargeUsage {
		t.Fatalf("charge kind=%q", line.Kind)
	}
	repair := credit.RepairStatus{Phase: credit.RepairPhaseReplay}
	if repair.Phase != credit.RepairPhaseReplay {
		t.Fatalf("repair phase=%q", repair.Phase)
	}

	store := postgres.New(nil)
	status, err := store.CreditRepair().Status(t.Context(), "missing", "repair")
	if !errors.Is(err, billing.ErrInvalid) || status.Phase != "" {
		t.Fatalf("credit repair via Store accessor: status=%+v err=%v", status, err)
	}
	if _, err := store.CreditProjection().Stored(t.Context(), account, "credits", ""); !errors.Is(err, billing.ErrInvalid) {
		t.Fatalf("CreditProjection().Stored via Store accessor: %v, want invalid without a database", err)
	}
	creditsPort := store.Credits()
	if creditsPort == nil {
		t.Fatal("Credits() returned nil")
	}
	engine := credit.New(creditsPort, nil)
	if _, err := engine.Balance(t.Context(), account, "credits", ""); !errors.Is(err, billing.ErrInvalid) {
		t.Fatalf("Credits() port returned %v, want invalid without a database", err)
	}
}
