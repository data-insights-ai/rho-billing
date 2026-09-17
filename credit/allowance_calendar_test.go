package credit

import (
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/catalog"
)

func TestPeriodsPreserveOriginalMonthDay(t *testing.T) {
	anchor := utcDate(2026, time.January, 31)
	input := PeriodInput{
		Definition: monthlyDefinition(),
		Assignment: assignment("a", "v1", anchor, utcDate(2027, time.January, 31)),
		Anchor:     anchor,
		Through:    utcDate(2026, time.April, 1),
	}
	periods, err := Periods(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(periods) != 3 {
		t.Fatalf("periods = %#v", periods)
	}
	want := []time.Time{utcDate(2026, time.January, 31), utcDate(2026, time.February, 28), utcDate(2026, time.March, 31)}
	for i, period := range periods {
		if !period.Start.Equal(want[i]) {
			t.Fatalf("period %d starts %s, want %s", i, period.Start, want[i])
		}
	}
}

func TestPeriodsLeapYearAndAnnualRecurrence(t *testing.T) {
	anchor := utcDate(2028, time.January, 29)
	periods, err := Periods(PeriodInput{Definition: monthlyDefinition(), Assignment: assignment("a", "v1", anchor, utcDate(2029, time.January, 29)), Anchor: anchor, Through: utcDate(2028, time.April, 1)})
	if err != nil || len(periods) != 3 {
		t.Fatalf("leap periods = %#v, err %v", periods, err)
	}
	if !periods[0].End.Equal(utcDate(2028, time.February, 29)) || !periods[1].Start.Equal(utcDate(2028, time.February, 29)) || !periods[1].End.Equal(utcDate(2028, time.March, 29)) {
		t.Fatalf("leap clamp did not recover original day: %#v", periods)
	}
	yearly := monthlyDefinition()
	yearly.Recurrence = catalog.AllowanceAnnual
	periods, err = Periods(PeriodInput{Definition: yearly, Assignment: assignment("a", "v1", utcDate(2026, time.January, 31), utcDate(2028, time.January, 31)), Anchor: utcDate(2026, time.January, 31), Through: utcDate(2027, time.January, 31)})
	if err != nil || len(periods) != 1 || !periods[0].End.Equal(utcDate(2027, time.January, 31)) {
		t.Fatalf("annual period = %#v, err %v", periods, err)
	}
}

func TestPeriodsUseAssignmentAnchorWhenExplicitAnchorOmitted(t *testing.T) {
	start := utcDate(2026, time.March, 30)
	periods, err := Periods(PeriodInput{Definition: monthlyDefinition(), Assignment: assignment("a", "v1", start, utcDate(2026, time.June, 30)), Through: utcDate(2026, time.May, 1)})
	if err != nil || len(periods) != 2 || !periods[1].Start.Equal(utcDate(2026, time.April, 30)) {
		t.Fatalf("assignment anchor periods = %#v, err %v", periods, err)
	}
}

func TestPeriodsPageResumesWithoutMaterializingHistory(t *testing.T) {
	start := utcDate(2020, time.January, 31)
	input := PeriodInput{Definition: monthlyDefinition(), Assignment: assignment("a", "v1", start, utcDate(2030, time.January, 31)), Anchor: start, Through: utcDate(2026, time.January, 31)}
	page, more, err := PeriodsPage(input, time.Time{}, 2)
	if err != nil || !more || len(page) != 2 {
		t.Fatalf("first page = %#v, more=%v, err=%v", page, more, err)
	}
	if !page[0].Start.Equal(start) || !page[1].Start.Equal(utcDate(2020, time.February, 29)) {
		t.Fatalf("first page dates = %#v", page)
	}
	next, more, err := PeriodsPage(input, page[1].Start, 2)
	if err != nil || !more || len(next) != 2 || !next[0].Start.Equal(utcDate(2020, time.March, 31)) {
		t.Fatalf("resumed page = %#v, more=%v, err=%v", next, more, err)
	}
}

func TestPeriodsPageSeedsFarHistoryCursor(t *testing.T) {
	anchor := utcDate(2000, time.January, 31)
	input := PeriodInput{Definition: monthlyDefinition(), Assignment: assignment("far", "v1", anchor, utcDate(2030, time.January, 31)), Anchor: anchor, Through: utcDate(2025, time.April, 1)}
	page, more, err := PeriodsPage(input, utcDate(2025, time.January, 31), 1)
	if err != nil || !more || len(page) != 1 || !page[0].Start.Equal(utcDate(2025, time.February, 28)) {
		t.Fatalf("far cursor page = %#v, more=%v, err=%v", page, more, err)
	}
}

func monthlyDefinition() catalog.AllowanceDefinition {
	return catalog.AllowanceDefinition{ID: "monthly", Unit: billing.Unit{Code: "credit", Scale: 1}, Amount: 10, Recurrence: catalog.AllowanceMonthly, Scope: catalog.AllowanceAccount, SpendScope: "AI_STANDARD"}
}

func assignment(id, plan string, start, end time.Time) catalog.PlanAssignment {
	return catalog.PlanAssignment{ID: id, PlanVersionID: plan, Quantity: 1, Effective: billing.Period{Start: start, End: end}, Source: catalog.SourceSubscription}
}

func utcDate(year int, month time.Month, day int) time.Time {
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}
