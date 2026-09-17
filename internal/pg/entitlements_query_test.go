package pg

import (
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/catalog"
	"github.com/data-insights-ai/rho-billing/query"
	"github.com/data-insights-ai/rho-billing/subscription"
)

// "What may this account do right now?" is the question a host asks on nearly
// every request. It must be one call, it must not need a provider reference,
// and an account that never subscribed must be distinguishable from a lookup
// that failed — those two call for opposite behaviour in a caller.
//
// The entitlements themselves come from catalog assignments, which is what
// purchase fulfilment actually writes. Resolving from anywhere else reads a
// field nobody populates and quietly answers "you get nothing".
func TestEntitlementsReadModel(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("ent-query")
	if err := store.CreateAccount(ctx, account, "ent-query-subject"); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	plan := catalog.PlanVersion{
		ID: "plan-pro", PlanID: "pro", Version: 1, PublishedAt: now.Add(-time.Hour),
		Entitlements: []catalog.EntitlementDefinition{
			{Key: "requests_hr", Kind: catalog.EntitlementLimit, Aggregation: catalog.AggregationMAX, Amount: 5000, Unit: billing.Unit{Code: "request", Scale: 1}},
			{Key: "webhooks", Kind: catalog.EntitlementFeature, Aggregation: catalog.AggregationOR, Enabled: true},
		},
	}
	if err := store.PublishPlan(ctx, plan); err != nil {
		t.Fatal(err)
	}

	entitlements := catalog.NewEntitlement(store.Entitlements(), clock)
	subs := subscription.New(store.Subscriptions())
	svc := query.New(query.Deps{Subscriptions: subs, Catalog: store, Entitlements: entitlements}, clock)

	// An account that never subscribed and holds nothing is not an error.
	before, err := svc.Entitlements(ctx, account, "org-acme")
	if err != nil {
		t.Fatalf("unsubscribed account returned an error: %v", err)
	}
	if before.Subscribed || before.Active {
		t.Fatalf("unsubscribed account reads as subscribed: %+v", before)
	}
	if _, _, ok := before.Limit("requests_hr"); ok {
		t.Fatal("an unsubscribed account must hold no limit")
	}

	start := now.Add(-time.Minute)
	end := start.AddDate(0, 1, 0)
	if _, err := subs.Activate(ctx, subscription.ActivateInput{
		Account: account, ID: "org-acme", Operation: "activate-acme", Quantity: 1,
		Items:    []subscription.Item{{ID: "seat", PlanVersion: plan.ID, Quantity: 1, Period: billing.Period{Start: start, End: end}}},
		Policies: subscription.Policies{Access: subscription.AccessImmediate, Collection: subscription.CollectionAutomatic, Proration: subscription.ProrationImmediate, Allowance: subscription.AllowanceKeepPeriod},
		Coverage: billing.Period{Start: start, End: end}, At: start,
	}); err != nil {
		t.Fatalf("activate: %v", err)
	}
	if _, err := entitlements.Assign(ctx, catalog.Assignment{
		Account: account, SourceRef: "org-acme", Actor: "test", Reason: "subscription",
		Plan: catalog.PlanAssignment{
			ID: "assign-acme", PlanVersionID: plan.ID, Quantity: 1,
			Effective: billing.Period{Start: start, End: end}, Source: catalog.SourcePurchase,
		},
	}); err != nil {
		t.Fatalf("assign: %v", err)
	}

	got, err := svc.Entitlements(ctx, account, "org-acme")
	if err != nil {
		t.Fatalf("Entitlements: %v", err)
	}
	if !got.Subscribed || !got.Active {
		t.Fatalf("active subscription reads as %+v", got)
	}
	limit, unit, ok := got.Limit("requests_hr")
	if !ok || limit != 5000 || unit.Code != "request" {
		t.Fatalf("limit = %d %+v, %v; want 5000 request, true", limit, unit, ok)
	}
	if !got.Feature("webhooks") {
		t.Fatal("feature entitlement did not resolve")
	}
	if got.Feature("something-else") {
		t.Fatal("an undeclared feature must not resolve")
	}

	// A revoked assignment stops entitling. Resolution that ignores revocations
	// keeps serving a cancelled customer.
	if _, err := entitlements.Revoke(ctx, catalog.Revocation{
		Account: account, AssignmentID: "assign-acme", SourceRef: "org-acme",
		Actor: "test", Reason: "cancelled", EffectiveAt: now.Add(-time.Second),
	}); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	after, err := svc.Entitlements(ctx, account, "org-acme")
	if err != nil {
		t.Fatalf("Entitlements after revoke: %v", err)
	}
	if _, _, ok := after.Limit("requests_hr"); ok {
		t.Fatal("a revoked assignment still entitles")
	}
}
