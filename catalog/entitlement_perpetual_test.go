package catalog

import (
	billing "github.com/data-insights-ai/rho-billing"

	"errors"
	"testing"
	"time"
)

func TestPerpetualAssignmentUsesExplicitUnboundedTerm(t *testing.T) {
	start := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	plan := PlanVersion{ID: "license-v1", PlanID: "license", Version: 1, Entitlements: []EntitlementDefinition{{Key: "export", Kind: EntitlementFeature, Aggregation: AggregationOR, Enabled: true}}}
	assignment := PlanAssignment{ID: "license", PlanVersionID: plan.ID, Quantity: 1, Source: SourcePurchase, Effective: billing.Period{Start: start}, Perpetual: true}
	for _, at := range []time.Time{start.Add(-time.Nanosecond), start, start.AddDate(1000, 0, 0)} {
		result, err := ResolveEntitlements(EntitlementInput{At: at, Assignments: []PlanAssignment{assignment}, Versions: []PlanVersion{plan}})
		if err != nil {
			t.Fatal(err)
		}
		got, exists := result.Get("export")
		want := !at.Before(start)
		if exists != want || (exists && !got.Enabled) {
			t.Fatalf("at %v got=%+v exists=%v", at, got, exists)
		}
	}
	for _, source := range []AssignmentSource{SourceManual, SourceFree, SourceMigration} {
		assignment.Source = source
		if !assignment.Valid() {
			t.Fatalf("source %v rejected", source)
		}
	}
	assignment.Source = SourceSubscription
	if assignment.Valid() {
		t.Fatal("provider subscription became perpetual")
	}
	assignment.Source = SourcePurchase
	assignment.Effective.End = start.Add(time.Hour)
	if _, err := ResolveEntitlements(EntitlementInput{At: start, Assignments: []PlanAssignment{assignment}, Versions: []PlanVersion{plan}}); !errors.Is(err, billing.ErrInvalid) {
		t.Fatalf("ambiguous finite/perpetual term: %v", err)
	}
}

func TestPerpetualAssignmentOverlapIsNotHiddenByZeroEnd(t *testing.T) {
	start := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	plan := PlanVersion{ID: "v1", PlanID: "plan", Version: 1}
	perpetual := PlanAssignment{ID: "same", PlanVersionID: plan.ID, Quantity: 1, Source: SourceManual, Effective: billing.Period{Start: start}, Perpetual: true}
	finite := perpetual
	finite.Perpetual = false
	finite.Effective = billing.Period{Start: start.Add(time.Hour), End: start.Add(2 * time.Hour)}
	for _, assignments := range [][]PlanAssignment{{finite, perpetual}, {perpetual, finite}} {
		if _, err := ResolveEntitlements(EntitlementInput{At: start, Assignments: assignments, Versions: []PlanVersion{plan}}); !errors.Is(err, billing.ErrConflict) {
			t.Fatalf("overlapping identity accepted: %v", err)
		}
	}
	finite.Effective = billing.Period{Start: start.Add(-time.Hour), End: start}
	if _, err := ResolveEntitlements(EntitlementInput{At: start, Assignments: []PlanAssignment{finite, perpetual}, Versions: []PlanVersion{plan}}); err != nil {
		t.Fatalf("adjacent history rejected: %v", err)
	}
}
