package catalog

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/data-insights-ai/rho-billing"
)

func TestRegistryPublishesDetachedHistoricalVersions(t *testing.T) {
	r := NewRegistry()
	plan := testPlan("v1", 1, 10)
	if err := r.PublishPlan(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	plan.Entitlements[0].Amount = 99
	plan.Entitlements = append(plan.Entitlements, EntitlementDefinition{Key: "extra", Kind: EntitlementFeature, Aggregation: AggregationOR, Enabled: true})
	got, err := r.Plan(context.Background(), "v1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Entitlements[0].Amount != 10 || len(got.Entitlements) != 1 {
		t.Fatalf("published plan was mutated: %#v", got)
	}
	got.Entitlements[0].Amount = 42
	again, err := r.Plan(context.Background(), "v1")
	if err != nil || again.Entitlements[0].Amount != 10 {
		t.Fatalf("read returned aliased plan: %#v %v", again, err)
	}
	if err := r.PublishPlan(context.Background(), plan); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("changed content should conflict, got %v", err)
	}
	otherID := testPlan("v1-copy", 1, 10)
	if err := r.PublishPlan(context.Background(), otherID); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("same plan/version under another ID should conflict, got %v", err)
	}
}

func TestRegistryPriceMappingIsAccountAndMerchantScoped(t *testing.T) {
	r := NewRegistry()
	if err := r.PublishPlan(context.Background(), testPlan("v1", 1, 10)); err != nil {
		t.Fatal(err)
	}
	base := PriceMapping{Scope: MappingScope{Provider: "paddle", Account: "acct-a", Environment: "sandbox", Merchant: "merchant-a"}, ExternalPriceID: "price", Target: MappingTarget{Kind: TargetPlanVersion, ID: "v1"}, Revision: 1}
	if err := r.PutPriceMapping(context.Background(), base); err != nil {
		t.Fatal(err)
	}
	if _, err := r.PriceMapping(context.Background(), MappingScope{Provider: "paddle", Account: "acct-b", Environment: "sandbox", Merchant: "merchant-a"}, "price"); !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("cross-account lookup should miss, got %v", err)
	}
	if _, err := r.PriceMapping(context.Background(), MappingScope{Provider: "paddle", Account: "acct-a", Environment: "sandbox", Merchant: "merchant-b"}, "price"); !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("cross-merchant lookup should miss, got %v", err)
	}
	if _, err := r.PriceMapping(context.Background(), MappingScope{Provider: "paddle", Account: "acct-a", Environment: "live", Merchant: "merchant-a"}, "price"); !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("cross-environment lookup should miss, got %v", err)
	}
	if err := r.PutPriceMapping(context.Background(), base); err != nil {
		t.Fatal(err)
	}
	base.Revision = 2
	if err := r.PutPriceMapping(context.Background(), base); err != nil {
		t.Fatal(err)
	}
	if historical, err := r.PriceMappingAtRevision(context.Background(), base.Scope, base.ExternalPriceID, 1); err != nil || historical.Target.ID != "v1" {
		t.Fatalf("historical mapping missing: %#v %v", historical, err)
	}
	base.Revision = 4
	if err := r.PutPriceMapping(context.Background(), base); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("skipped revision should conflict, got %v", err)
	}
}

func testPlan(id string, version, amount int64) PlanVersion {
	return PlanVersion{
		ID: id, PlanID: "pro", Version: version,
		Entitlements: []EntitlementDefinition{{Key: "jobs", Kind: EntitlementLimit, Aggregation: AggregationSUM, Amount: amount, Unit: billing.Unit{Code: "job", Scale: 1}}},
	}
}

func TestAllowancePolicyRequiresTypedScopeAndPositiveAmount(t *testing.T) {
	plan := PlanVersion{ID: "allowance", PlanID: "pro", Version: 1, Allowances: []AllowanceDefinition{{
		ID: "monthly", Unit: billing.Unit{Code: "credit", Scale: 100}, Amount: 10,
		Recurrence: AllowanceMonthly, Scope: AllowanceAccount, SpendScope: "AI_STANDARD",
	}}}
	if err := plan.validate(); err != nil {
		t.Fatalf("valid allowance rejected: %v", err)
	}
	plan.Allowances[0].Amount = 0
	if !errors.Is(plan.validate(), billing.ErrInvalid) {
		t.Fatal("zero allowance amount should be invalid")
	}
	plan.Allowances[0].Amount = 10
	plan.Allowances[0].SpendScope = ""
	if !errors.Is(plan.validate(), billing.ErrInvalid) {
		t.Fatal("missing spend scope should be invalid")
	}
}

func TestRegistryConcurrentIdempotentReadsAndPublishes(t *testing.T) {
	r := NewRegistry()
	plan := testPlan("concurrent", 1, 10)
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			if err := r.PublishPlan(context.Background(), plan); err != nil {
				t.Errorf("publish: %v", err)
			}
			if _, err := r.Plan(context.Background(), plan.ID); err != nil {
				t.Errorf("lookup: %v", err)
			}
		})
	}
	wg.Wait()
}
