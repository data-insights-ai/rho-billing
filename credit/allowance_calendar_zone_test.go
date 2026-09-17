package credit

import (
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/catalog"
)

func TestPeriodsConvertOffsetBearingTimesToUTCWithoutNamedZonePolicy(t *testing.T) {
	first, err := time.Parse(time.RFC3339, "2026-10-25T02:30:00+02:00")
	if err != nil {
		t.Fatal(err)
	}
	second, err := time.Parse(time.RFC3339, "2026-10-25T02:30:00+01:00")
	if err != nil {
		t.Fatal(err)
	}
	if first.Format("15:04") != second.Format("15:04") {
		t.Fatal("fixture lost the repeated Brussels wall clock")
	}
	if billing.CanonicalTime(second).Sub(billing.CanonicalTime(first)) != time.Hour {
		t.Fatal("canonical clock collapsed distinct DST instants")
	}

	start := first
	end := start.AddDate(0, 3, 0)
	periods, err := Periods(PeriodInput{
		Definition: catalog.AllowanceDefinition{ID: "monthly", Unit: billing.Unit{Code: "credit", Scale: 1}, Amount: 10, Recurrence: catalog.AllowanceMonthly, Scope: catalog.AllowanceAccount, SpendScope: "AI_STANDARD"},
		Assignment: catalog.PlanAssignment{ID: "a", PlanVersionID: "v1", Quantity: 1, Effective: billing.Period{Start: start, End: end}, Source: catalog.SourceSubscription},
		Anchor:     start,
		Through:    start.AddDate(0, 1, 1),
	})
	if err != nil || len(periods) == 0 {
		t.Fatalf("periods=%#v err=%v", periods, err)
	}
	for i, period := range periods {
		if period.Start.Location() != time.UTC || period.End.Location() != time.UTC {
			t.Fatalf("period %d used named-zone location %s/%s", i, period.Start.Location(), period.End.Location())
		}
		if period.Start.Nanosecond()%1000 != 0 {
			t.Fatalf("period %d is not microsecond-canonical: %s", i, period.Start)
		}
	}
	if !periods[0].Start.Equal(billing.CanonicalTime(first)) {
		t.Fatalf("anchor %s was not the UTC instant", periods[0].Start)
	}
}
