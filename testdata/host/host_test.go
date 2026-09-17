package host_test

import (
	"context"
	"testing"
	"time"

	adapter "example.com/billing-adapter-contract"
	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/catalog"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/integration"
	"github.com/data-insights-ai/rho-billing/postgres"
	"github.com/data-insights-ai/rho-billing/purchase"
	"github.com/data-insights-ai/rho-billing/subscription"
	"github.com/data-insights-ai/rho-billing/usage"
)

type inbox struct{ message integration.Message }

func TestHostUsesSubscriptionServiceThroughPublicRepository(t *testing.T) {
	accounts := catalog.NewMemoryAccountRepository()
	if err := accounts.CreateAccount(t.Context(), "tenant-subscription", "organization"); err != nil {
		t.Fatal(err)
	}
	service := subscription.New(subscription.NewMemoryRepository(subscription.MemoryConfig{Accounts: accounts}))
	ref := billing.Reference{Scope: billing.Scope{Provider: "example", Merchant: "merchant", Environment: "sandbox"}, ID: "subscription"}
	observed, err := service.Observe(t.Context(), subscription.Observation{
		Snapshot: subscription.Snapshot{Account: "tenant-subscription", Ref: ref, Status: "active"},
		EventID:  "event", OccurredAt: time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC),
	})
	if err != nil || observed.Revision != 1 {
		t.Fatalf("observation=%+v error=%v", observed, err)
	}
	stored, err := service.Subscription(t.Context(), "tenant-subscription", ref)
	if err != nil || stored.Ref != ref || stored.Revision != observed.Revision {
		t.Fatalf("stored=%+v error=%v", stored, err)
	}
	store := postgres.New(nil)
	if subscription.New(store.Subscriptions()) == nil {
		t.Fatal("missing durable subscription service")
	}
	if err := store.Migrate(t.Context()); err == nil {
		t.Fatal("constructor migrated without an explicit Migrate call succeeding against a live database")
	}
}

func TestHostConstructsDomainServicesThroughPostgresPorts(t *testing.T) {
	store := postgres.New(nil)
	if credit.New(store.Credits(), nil) == nil {
		t.Fatal("missing credit engine on Credits()")
	}
	if purchase.New(store.Purchases(), nil) == nil {
		t.Fatal("missing purchase service on Purchases()")
	}
	if usage.NewSettlement(store.Settlements(), nil) == nil {
		t.Fatal("missing settlement service on Settlements()")
	}
	if store.Queue() == nil {
		t.Fatal("missing queue port")
	}
	if store.Ratings() == nil {
		t.Fatal("missing ratings port")
	}
	if store.Catalog() == nil || store.Accounts() == nil || store.Usage() == nil {
		t.Fatal("missing catalog, account or usage port")
	}
	if store.CreditRepair() == nil || store.CreditProjection() == nil || store.Estimates() == nil {
		t.Fatal("missing operator ports")
	}
	if store.Allowances() == nil || store.Limits() == nil || store.Ops() == nil || store.Entitlements() == nil {
		t.Fatal("missing remaining domain ports")
	}
}

func TestHostResolvesCatalogEntitlements(t *testing.T) {
	start := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)
	plan := catalog.PlanVersion{ID: "professional-1", PlanID: "professional", Version: 1,
		Entitlements: []catalog.EntitlementDefinition{{Key: "export", Kind: catalog.EntitlementFeature, Aggregation: catalog.AggregationOR, Enabled: true}}}
	snapshot, err := catalog.ResolveEntitlements(catalog.EntitlementInput{At: start,
		Assignments: []catalog.PlanAssignment{{ID: "contract-1", PlanVersionID: plan.ID, Quantity: 1, Source: catalog.SourceManual,
			Effective: billing.Period{Start: start, End: start.AddDate(0, 1, 0)}}},
		Versions: []catalog.PlanVersion{plan}})
	if err != nil {
		t.Fatal(err)
	}
	feature, ok := snapshot.Get("export")
	if !ok || !feature.Enabled {
		t.Fatal("host did not receive its assigned export entitlement")
	}
}

func (i *inbox) Receive(_ context.Context, m integration.Message) error {
	if err := m.Validate(); err != nil {
		return err
	}
	i.message = m
	return nil
}

func TestIndependentHostAndAdapter(t *testing.T) {
	now := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)
	account := billing.AccountID("tenant-a")
	engine := credit.New(credit.NewMemoryRepository(account), func() time.Time { return now })
	_, err := engine.Grant(t.Context(), credit.GrantInput{
		Account: account, Operation: "grant-1", LotID: "lot-1",
		Unit: billing.Unit{Code: "ai", Scale: 1000}, Amount: 5000,
		Source: "manual", SourceRef: "support-1", ValidFrom: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	balance, err := engine.Balance(t.Context(), account, "ai", "")
	if err != nil || balance.Available != 5000 {
		t.Fatalf("balance=%+v error=%v", balance, err)
	}
	receiver := new(inbox)
	if err := adapter.ReceiveVerified(t.Context(), receiver, account, "event-1", now, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	if receiver.message.Account != account || receiver.message.ID == "" {
		t.Fatal("lost scoped delivery")
	}
	// Construction must be inert: a host owns connection setup and migration.
	if postgres.New(nil) == nil {
		t.Fatal("missing store")
	}
}
