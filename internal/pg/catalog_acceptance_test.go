package pg

import (
	"errors"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/catalog"
	"github.com/data-insights-ai/rho-billing/subscription"
)

func acceptancePlans() (catalog.PlanVersion, catalog.PlanVersion) {
	old := catalog.PlanVersion{
		ID: "catalog-acceptance-v1", PlanID: "catalog-acceptance", Version: 1,
		Entitlements: []catalog.EntitlementDefinition{{
			Key: "requests", Kind: catalog.EntitlementLimit, Aggregation: catalog.AggregationMAX,
			Amount: 10, Unit: billing.Unit{Code: "request", Scale: 1},
		}},
	}
	newer := old
	newer.ID = "catalog-acceptance-v2"
	newer.Version = 2
	newer.Entitlements = []catalog.EntitlementDefinition{{
		Key: "requests", Kind: catalog.EntitlementLimit, Aggregation: catalog.AggregationMAX,
		Amount: 20, Unit: billing.Unit{Code: "request", Scale: 1},
	}}
	return old, newer
}

func acceptanceSnapshot(old, newer catalog.PlanVersion) subscription.Snapshot {
	start := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	boundary := time.Date(2026, time.February, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, time.March, 1, 0, 0, 0, 0, time.UTC)
	return subscription.Snapshot{
		Account: "acct-a",
		Ref:     billing.Reference{Scope: billing.Scope{Provider: "catalog-provider", Merchant: "catalog-merchant", Environment: "sandbox"}, ID: "catalog-subscription"},
		Status:  "active",
		Assignments: []catalog.PlanAssignment{
			{ID: "catalog-assignment-old", PlanVersionID: old.ID, Quantity: 1, Effective: billing.Period{Start: start, End: boundary}, Source: catalog.SourceSubscription},
			{ID: "catalog-assignment-new", PlanVersionID: newer.ID, Quantity: 1, Effective: billing.Period{Start: boundary, End: end}, Source: catalog.SourceSubscription},
		},
	}
}

func oldOnlyAcceptanceSnapshot(snapshot subscription.Snapshot) subscription.Snapshot {
	snapshot.Assignments = snapshot.Assignments[:1]
	return snapshot
}

func assertHistoricalEntitlement(t *testing.T, snapshot subscription.Snapshot, at time.Time, load func(string) (catalog.PlanVersion, error), want int64) {
	t.Helper()
	versions := make([]catalog.PlanVersion, 0, len(snapshot.Assignments))
	for _, assignment := range snapshot.Assignments {
		plan, err := load(assignment.PlanVersionID)
		if err != nil {
			t.Fatalf("load historical plan %s: %v", assignment.PlanVersionID, err)
		}
		versions = append(versions, plan)
	}
	resolved, err := catalog.ResolveEntitlements(catalog.EntitlementInput{At: at, Assignments: snapshot.Assignments, Versions: versions})
	if err != nil {
		t.Fatalf("resolve entitlement at %s: %v", at, err)
	}
	value, ok := resolved.Get("requests")
	if !ok || value.Amount != want || value.Unit != (billing.Unit{Code: "request", Scale: 1}) {
		t.Fatalf("resolved entitlement at %s = %#v, want amount %d", at, value, want)
	}
}

func TestHistoricalSubscriptionEntitlementAcceptanceMemory(t *testing.T) {
	ctx := t.Context()
	old, newer := acceptancePlans()
	plans := catalog.NewRegistry()
	if err := plans.PublishPlan(ctx, old); err != nil {
		t.Fatal(err)
	}
	accounts := catalog.NewMemoryAccountRepository()
	if err := accounts.CreateAccount(ctx, "acct-a", "catalog-acceptance-subject"); err != nil {
		t.Fatal(err)
	}
	subs := subscription.New(subscription.NewMemoryRepository(subscription.MemoryConfig{Catalog: plans, Accounts: accounts}))
	snapshot := acceptanceSnapshot(old, newer)
	if _, err := subs.Observe(ctx, subscription.Observation{Snapshot: oldOnlyAcceptanceSnapshot(snapshot), EventID: "catalog-subscription-old", OccurredAt: time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)}); err != nil {
		t.Fatal(err)
	}
	if err := plans.PublishPlan(ctx, newer); err != nil {
		t.Fatal(err)
	}
	observed, err := subs.Observe(ctx, subscription.Observation{Snapshot: snapshot, EventID: "catalog-subscription-upgrade", OccurredAt: time.Date(2026, time.February, 1, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := subs.Subscription(ctx, snapshot.Account, snapshot.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if observed.Ref != stored.Ref || observed.Revision != stored.Revision || observed.SourceFingerprint != stored.SourceFingerprint {
		t.Fatalf("observe returned %+v, stored %+v", observed, stored)
	}
	load := func(id string) (catalog.PlanVersion, error) { return plans.Plan(ctx, id) }
	assertHistoricalEntitlement(t, stored, time.Date(2026, time.January, 31, 23, 59, 59, 999999000, time.UTC), load, 10)
	assertHistoricalEntitlement(t, stored, time.Date(2026, time.February, 1, 0, 0, 0, 0, time.UTC), load, 20)
}

func TestPostgresCatalogHistoricalEntitlementAndMappingAcceptance(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	createTestAccounts(t, store)
	old, newer := acceptancePlans()
	if err := store.PublishPlan(ctx, old); err != nil {
		t.Fatal(err)
	}
	scope := catalog.MappingScope{Provider: "catalog-provider", Account: "acct-a", Environment: "sandbox", Merchant: "catalog-merchant"}
	if err := store.PutPriceMapping(ctx, catalog.PriceMapping{Scope: scope, ExternalPriceID: "catalog-price", Target: catalog.MappingTarget{Kind: catalog.TargetPlanVersion, ID: old.ID}, Revision: 1}); err != nil {
		t.Fatal(err)
	}
	snapshot := acceptanceSnapshot(old, newer)
	if _, err := subscription.New(store.Subscriptions()).Observe(ctx, subscription.Observation{Snapshot: oldOnlyAcceptanceSnapshot(snapshot), EventID: "catalog-subscription-old", OccurredAt: time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)}); err != nil {
		t.Fatal(err)
	}
	if err := store.PublishPlan(ctx, newer); err != nil {
		t.Fatal(err)
	}
	if err := store.PutPriceMapping(ctx, catalog.PriceMapping{Scope: scope, ExternalPriceID: "catalog-price", Target: catalog.MappingTarget{Kind: catalog.TargetPlanVersion, ID: newer.ID}, Revision: 2}); err != nil {
		t.Fatal(err)
	}
	historical, err := store.PriceMappingAtRevision(ctx, scope, "catalog-price", 1)
	if err != nil || historical.Target.ID != old.ID {
		t.Fatalf("historical price mapping = %#v, err %v", historical, err)
	}
	current, err := store.PriceMapping(ctx, scope, "catalog-price")
	if err != nil || current.Target.ID != newer.ID {
		t.Fatalf("current price mapping = %#v, err %v", current, err)
	}

	if _, err := subscription.New(store.Subscriptions()).Observe(ctx, subscription.Observation{Snapshot: snapshot, EventID: "catalog-subscription-upgrade", OccurredAt: time.Date(2026, time.February, 1, 0, 0, 0, 0, time.UTC)}); err != nil {
		t.Fatal(err)
	}
	stored, err := subscription.New(store.Subscriptions()).Subscription(ctx, snapshot.Account, snapshot.Ref)
	if err != nil {
		t.Fatal(err)
	}
	load := func(id string) (catalog.PlanVersion, error) { return store.Plan(ctx, id) }
	assertHistoricalEntitlement(t, stored, time.Date(2026, time.January, 31, 23, 59, 59, 999999000, time.UTC), load, 10)
	assertHistoricalEntitlement(t, stored, time.Date(2026, time.February, 1, 0, 0, 0, 0, time.UTC), load, 20)
	retained, err := store.Plan(ctx, old.ID)
	if err != nil || retained.Entitlements[0].Amount != 10 {
		t.Fatalf("old plan was not retained: %#v, err %v", retained, err)
	}

	for name, candidate := range map[string]catalog.MappingScope{
		"missing":           scope,
		"inactive":          scope,
		"wrong environment": func() catalog.MappingScope { s := scope; s.Environment = "production"; return s }(),
		"wrong account":     func() catalog.MappingScope { s := scope; s.Account = "acct-b"; return s }(),
	} {
		priceID := "catalog-price-missing"
		if name == "inactive" {
			// There is no active flag in the core mapping contract. An inactive
			// provider price is represented by the absence of an active mapping.
			priceID = "catalog-price-inactive"
		} else if name == "wrong environment" || name == "wrong account" {
			priceID = "catalog-price"
		}
		if _, err := store.PriceMapping(ctx, candidate, priceID); !errors.Is(err, billing.ErrNotFound) {
			t.Errorf("%s mapping lookup = %v, want not found", name, err)
		}
	}
}
