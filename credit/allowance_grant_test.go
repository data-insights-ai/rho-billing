package credit

import (
	"errors"
	"math"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/catalog"
)

func TestPrepareGrantPreservesSelectedPeriodIdentityAndValidity(t *testing.T) {
	start := utcDate(2026, time.January, 31)
	period := billing.Period{Start: start, End: utcDate(2026, time.February, 28)}
	definition := monthlyDefinition()
	plan := catalog.PlanVersion{ID: "plan", PlanID: "pro", Version: 1, Allowances: []catalog.AllowanceDefinition{definition}}
	schedule := Schedule{Account: "acct", Assignment: assignment("assignment", plan.ID, start, utcDate(2026, time.July, 31))}
	schedule.Assignment.Quantity = 2
	eligible := paidEligibility(start, period.End)
	grant, err := prepareGrant(schedule, plan, definition, period, eligible)
	if err != nil {
		t.Fatal(err)
	}
	if grant.Amount != 20 || grant.Scope != "AI_STANDARD" || grant.Source != "allowance" || !grant.ValidFrom.Equal(start) || !grant.ExpiresAt.Equal(period.End) {
		t.Fatalf("grant=%+v", grant)
	}
	key := grantKey(schedule.Account, schedule.Assignment, plan, definition, period)
	if string(grant.Operation) != "allowance-op-"+key || grant.LotID != "allowance-lot-"+key || grant.SourceRef != key {
		t.Fatalf("grant identity=%+v", grant)
	}
	again, err := prepareGrant(schedule, plan, definition, period, eligible)
	if err != nil || again != grant {
		t.Fatalf("repeated grant=%+v err=%v", again, err)
	}
	definition.Validity = 24 * time.Hour
	limited, err := prepareGrant(schedule, plan, definition, period, eligible)
	if err != nil || !limited.ExpiresAt.Equal(start.Add(24*time.Hour)) {
		t.Fatalf("limited expiry=%+v err=%v", limited, err)
	}
}

func TestPrepareGrantRejectsUnsupportedScopeCoverageAndOverflow(t *testing.T) {
	start := utcDate(2026, time.January, 31)
	period := billing.Period{Start: start, End: utcDate(2026, time.February, 28)}
	definition := monthlyDefinition()
	plan := catalog.PlanVersion{ID: "plan", PlanID: "pro", Version: 1, Allowances: []catalog.AllowanceDefinition{definition}}
	schedule := Schedule{Account: "acct", Assignment: assignment("assignment", plan.ID, start, period.End)}
	eligible := paidEligibility(start, period.End)
	member := definition
	member.Scope = catalog.AllowanceMember
	if _, err := prepareGrant(schedule, plan, member, period, eligible); !errors.Is(err, ErrMemberScope) {
		t.Fatalf("member scope=%v", err)
	}
	if _, err := prepareGrant(schedule, plan, definition, period, Eligibility{Status: EligibilityDelinquent}); !errors.Is(err, ErrIneligible) {
		t.Fatalf("unpaid coverage=%v", err)
	}
	schedule.Assignment.Quantity = math.MaxInt64
	if _, err := prepareGrant(schedule, plan, definition, period, eligible); !errors.Is(err, billing.ErrOverflow) {
		t.Fatalf("overflow=%v", err)
	}
}
