package credit

import (
	"errors"
	"testing"
	"time"

	"github.com/data-insights-ai/rho-billing/catalog"
)

func TestScheduleTransitionUsesEverySharedRecurringBoundary(t *testing.T) {
	monthly := monthlyDefinition()
	annual := monthly
	annual.ID = "annual"
	annual.Recurrence = catalog.AllowanceAnnual
	previousPlan := transitionPlan("previous", monthly, annual)
	nextMonthly := monthly
	nextMonthly.Amount = 5
	nextAnnual := annual
	nextAnnual.Amount = 5
	nextPlan := transitionPlan("next", nextMonthly, nextAnnual)
	previous := assignment("previous-assignment", previousPlan.ID, utcDate(2026, time.January, 1), utcDate(2030, time.January, 1))
	next := assignment("next-assignment", nextPlan.ID, utcDate(2026, time.February, 1), previous.Effective.End)

	if err := scheduleTransition(previousPlan, nextPlan, previous, next, previous.Effective.Start, next.Effective.Start, AdjustmentDelta); !errors.Is(err, ErrAdjustment) {
		t.Fatalf("downgrade at monthly-only boundary = %v, want ErrAdjustment", err)
	}
	next.Effective.Start = utcDate(2027, time.January, 1)
	if err := scheduleTransition(previousPlan, nextPlan, previous, next, previous.Effective.Start, next.Effective.Start, AdjustmentDelta); err != nil {
		t.Fatalf("downgrade at shared annual boundary = %v", err)
	}
}

func TestScheduleTransitionRespectsExplicitDefinitionAnchors(t *testing.T) {
	first := monthlyDefinition()
	first.ID = "first"
	first.Anchor = utcDate(2026, time.January, 15)
	second := monthlyDefinition()
	second.ID = "second"
	second.Anchor = utcDate(2026, time.January, 31)
	previousPlan := transitionPlan("previous", first, second)
	nextPlan := transitionPlan("next", first, second)
	previous := assignment("previous-assignment", previousPlan.ID, utcDate(2026, time.January, 1), utcDate(2027, time.January, 1))
	next := assignment("next-assignment", nextPlan.ID, utcDate(2026, time.February, 15), previous.Effective.End)
	if err := scheduleTransition(previousPlan, nextPlan, previous, next, previous.Effective.Start, next.Effective.Start, AdjustmentDelta); !errors.Is(err, ErrAdjustment) {
		t.Fatalf("one explicit anchor boundary should not cover another definition = %v", err)
	}

	next.Effective.Start = utcDate(2026, time.February, 28)
	if err := scheduleTransition(previousPlan, nextPlan, previous, next, previous.Effective.Start, next.Effective.Start, AdjustmentDelta); !errors.Is(err, ErrAdjustment) {
		t.Fatalf("the other explicit anchor boundary should not cover the first definition = %v", err)
	}
}

func TestScheduleTransitionRejectsRecurringRemovalMidCycle(t *testing.T) {
	monthly := monthlyDefinition()
	previousPlan := transitionPlan("previous", monthly)
	nextPlan := transitionPlan("next")
	previous := assignment("previous-assignment", previousPlan.ID, utcDate(2026, time.January, 1), utcDate(2026, time.April, 1))
	next := assignment("next-assignment", nextPlan.ID, utcDate(2026, time.February, 15), previous.Effective.End)
	if err := scheduleTransition(previousPlan, nextPlan, previous, next, previous.Effective.Start, next.Effective.Start, AdjustmentDelta); !errors.Is(err, ErrAdjustment) {
		t.Fatalf("recurring removal with delta mid-cycle = %v", err)
	}
	if err := scheduleTransition(previousPlan, nextPlan, previous, next, previous.Effective.Start, next.Effective.Start, 0); !errors.Is(err, ErrAdjustment) {
		t.Fatalf("recurring removal with default policy mid-cycle = %v", err)
	}
	next.Effective.Start = utcDate(2026, time.February, 1)
	if err := scheduleTransition(previousPlan, nextPlan, previous, next, previous.Effective.Start, next.Effective.Start, 0); err != nil {
		t.Fatalf("recurring removal at boundary = %v", err)
	}
}

func TestScheduleTransitionRejectsRecurringAdditionUnlessItsBoundary(t *testing.T) {
	monthly := monthlyDefinition()
	previousPlan := transitionPlan("previous")
	nextPlan := transitionPlan("next", monthly)
	previous := assignment("previous-assignment", previousPlan.ID, utcDate(2026, time.January, 1), utcDate(2026, time.April, 1))
	next := assignment("next-assignment", nextPlan.ID, utcDate(2026, time.February, 15), previous.Effective.End)
	if err := scheduleTransition(previousPlan, nextPlan, previous, next, previous.Effective.Start, next.Effective.Start, 0); !errors.Is(err, ErrAdjustment) {
		t.Fatalf("new recurring allowance mid-cycle = %v", err)
	}
	next.Effective.Start = utcDate(2026, time.February, 1)
	if err := scheduleTransition(previousPlan, nextPlan, previous, next, previous.Effective.Start, next.Effective.Start, 0); err != nil {
		t.Fatalf("new recurring allowance at its boundary = %v", err)
	}
}

func TestScheduleTransitionRequiresSharedRecurringIDsForMidCycleDelta(t *testing.T) {
	old := monthlyDefinition()
	old.ID = "old"
	newDefinition := monthlyDefinition()
	newDefinition.ID = "new"
	previousPlan := transitionPlan("previous", old)
	nextPlan := transitionPlan("next", newDefinition)
	previous := assignment("previous-assignment", previousPlan.ID, utcDate(2026, time.January, 1), utcDate(2026, time.April, 1))
	next := assignment("next-assignment", nextPlan.ID, utcDate(2026, time.February, 15), previous.Effective.End)
	if err := scheduleTransition(previousPlan, nextPlan, previous, next, previous.Effective.Start, next.Effective.Start, AdjustmentDelta); !errors.Is(err, ErrAdjustment) {
		t.Fatalf("unshared recurring IDs should not evade the default boundary = %v", err)
	}
}

func TestScheduleTransitionAllowsPositiveSharedDeltaMidCycle(t *testing.T) {
	old := monthlyDefinition()
	newDefinition := old
	newDefinition.Amount = old.Amount + 1
	previousPlan := transitionPlan("previous", old)
	nextPlan := transitionPlan("next", newDefinition)
	previous := assignment("previous-assignment", previousPlan.ID, utcDate(2026, time.January, 1), utcDate(2026, time.April, 1))
	next := assignment("next-assignment", nextPlan.ID, utcDate(2026, time.February, 15), previous.Effective.End)
	if err := scheduleTransition(previousPlan, nextPlan, previous, next, previous.Effective.Start, next.Effective.Start, AdjustmentDelta); err != nil {
		t.Fatalf("positive shared recurring delta mid-cycle = %v", err)
	}
}

func TestIsDefinitionBoundaryUsesCalendarDates(t *testing.T) {
	monthly := monthlyDefinition()
	monthlyCases := []struct {
		name      string
		candidate time.Time
		want      bool
	}{
		{"february clamp", utcDate(2026, time.February, 28), true},
		{"march recovers anchor day", utcDate(2026, time.March, 31), true},
		{"wrong time of day", time.Date(2026, time.February, 28, 0, 0, 1, 0, time.UTC), false},
		{"one microsecond before", utcDate(2026, time.February, 27).Add(24*time.Hour - time.Microsecond), false},
	}
	for _, tc := range monthlyCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isDefinitionBoundary(utcDate(2026, time.January, 31), tc.candidate, monthly); got != tc.want {
				t.Fatalf("boundary = %v, want %v", got, tc.want)
			}
		})
	}

	annual := monthly
	annual.Recurrence = catalog.AllowanceAnnual
	anchor := utcDate(2024, time.February, 29)
	for _, candidate := range []time.Time{utcDate(2025, time.February, 28), utcDate(2028, time.February, 29)} {
		if !isDefinitionBoundary(anchor, candidate, annual) {
			t.Fatalf("annual boundary %s was rejected", candidate)
		}
	}
	if isDefinitionBoundary(anchor, utcDate(2025, time.March, 1), annual) {
		t.Fatal("non-boundary annual date was accepted")
	}

	far := monthly
	if !isDefinitionBoundary(utcDate(2000, time.January, 31), utcDate(3025, time.January, 31), far) {
		t.Fatal("far calendar boundary was rejected")
	}
}

func transitionPlan(id string, definitions ...catalog.AllowanceDefinition) catalog.PlanVersion {
	plan := catalog.PlanVersion{ID: id, PlanID: "transition-plan", Version: 1, PublishedAt: utcDate(2026, time.January, 1), Allowances: definitions}
	if !catalog.ValidPlan(plan) {
		panic("invalid transition test plan")
	}
	return plan
}
