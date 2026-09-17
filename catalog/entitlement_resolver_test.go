package catalog

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/data-insights-ai/rho-billing"
)

func TestResolveQuantityAwareAggregationAndExactBoundaries(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	plan := PlanVersion{
		ID: "v1", PlanID: "pro", Version: 1,
		Entitlements: []EntitlementDefinition{
			{Key: "feature", Kind: EntitlementFeature, Aggregation: AggregationOR, Enabled: true},
			{Key: "jobs", Kind: EntitlementLimit, Aggregation: AggregationSUM, Amount: 4, Unit: billing.Unit{Code: "job", Scale: 1}},
		},
	}
	assignments := []PlanAssignment{{ID: "base", PlanVersionID: "v1", Quantity: 2, Effective: billing.Period{Start: start, End: end}}}
	got, err := ResolveEntitlements(EntitlementInput{At: start, Assignments: assignments, Versions: []PlanVersion{plan}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Entitlements["jobs"].Amount != 8 || !got.Entitlements["feature"].Enabled {
		t.Fatalf("unexpected result: %#v", got.Entitlements)
	}
	if _, err := ResolveEntitlements(EntitlementInput{At: end, Assignments: assignments, Versions: []PlanVersion{plan}}); err != nil {
		t.Fatal(err)
	}
	boundary, _ := ResolveEntitlements(EntitlementInput{At: end, Assignments: assignments, Versions: []PlanVersion{plan}})
	if len(boundary.Entitlements) != 0 {
		t.Fatalf("end boundary must be inactive: %#v", boundary)
	}
}

func TestResolveRejectsMixedCompositionAndOverflow(t *testing.T) {
	period := billing.Period{Start: time.Unix(1, 0).UTC(), End: time.Unix(2, 0).UTC()}
	versions := []PlanVersion{
		{ID: "max", PlanID: "max", Version: 1, Entitlements: []EntitlementDefinition{{Key: "quota", Kind: EntitlementLimit, Aggregation: AggregationMAX, Amount: 2, Unit: billing.Unit{Code: "u", Scale: 1}}}},
		{ID: "sum", PlanID: "sum", Version: 1, Entitlements: []EntitlementDefinition{{Key: "quota", Kind: EntitlementLimit, Aggregation: AggregationSUM, Amount: 2, Unit: billing.Unit{Code: "u", Scale: 1}}}},
	}
	assignments := []PlanAssignment{{ID: "a", PlanVersionID: "max", Quantity: 1, Effective: period}, {ID: "b", PlanVersionID: "sum", Quantity: 1, Effective: period}}
	if _, err := ResolveEntitlements(EntitlementInput{At: period.Start, Assignments: assignments, Versions: versions}); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("mixed mode should conflict, got %v", err)
	}
	overflow := PlanVersion{ID: "overflow", PlanID: "overflow", Version: 1, Entitlements: []EntitlementDefinition{{Key: "quota", Kind: EntitlementLimit, Aggregation: AggregationSUM, Amount: 2, Unit: billing.Unit{Code: "u", Scale: 1}}}}
	if _, err := ResolveEntitlements(EntitlementInput{At: period.Start, Assignments: []PlanAssignment{{PlanVersionID: "overflow", Quantity: math.MaxInt64, Effective: period}}, Versions: []PlanVersion{overflow}}); !errors.Is(err, billing.ErrOverflow) {
		t.Fatalf("overflow should reject, got %v", err)
	}
}

func TestResolveRejectsUnknownAndOverlappingAssignmentID(t *testing.T) {
	period := billing.Period{Start: time.Unix(1, 0).UTC(), End: time.Unix(2, 0).UTC()}
	plan := PlanVersion{ID: "v1", PlanID: "p", Version: 1}
	assignments := []PlanAssignment{{ID: "same", PlanVersionID: "v1", Quantity: 1, Effective: period}, {ID: "same", PlanVersionID: "v1", Quantity: 2, Effective: period}}
	if _, err := ResolveEntitlements(EntitlementInput{At: period.Start, Assignments: assignments, Versions: []PlanVersion{plan}}); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("overlapping assignment should conflict, got %v", err)
	}
	if _, err := ResolveEntitlements(EntitlementInput{At: period.Start, Assignments: []PlanAssignment{{PlanVersionID: "missing", Quantity: 1, Effective: period}}, Versions: nil}); !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("unknown version should fail, got %v", err)
	}
}
