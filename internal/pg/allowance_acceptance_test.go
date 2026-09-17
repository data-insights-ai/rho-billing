package pg

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/catalog"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/subscription"
)

type allowanceAcceptanceFixture struct {
	store    *Store
	account  billing.AccountID
	plan     catalog.PlanVersion
	schedule credit.Schedule
	start    time.Time
	end      time.Time
}

func newAllowanceAcceptanceFixture(t *testing.T, name string) allowanceAcceptanceFixture {
	t.Helper()
	store, _ := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("acceptance-" + name)
	plan := catalog.PlanVersion{ID: "acceptance-plan-" + name, PlanID: "acceptance-" + name, Version: 1, Allowances: []catalog.AllowanceDefinition{{ID: "monthly", Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 10, Recurrence: catalog.AllowanceMonthly, Scope: catalog.AllowanceAccount, SpendScope: "AI_STANDARD"}}}
	start := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	if name == "upgrade-cycle" {
		start = time.Date(2026, time.January, 31, 0, 0, 0, 0, time.UTC)
	}
	end := time.Date(2026, time.May, 1, 0, 0, 0, 0, time.UTC)
	ref := billing.Reference{Scope: billing.Scope{Provider: "acceptance-provider", Merchant: "acceptance-merchant-" + name, Environment: "sandbox"}, ID: "acceptance-sub-" + name}
	assignment := catalog.PlanAssignment{ID: "acceptance-assignment-" + name, PlanVersionID: plan.ID, Quantity: 1, Effective: billing.Period{Start: start, End: end}, Source: catalog.SourceSubscription}
	if err := store.CreateAccount(ctx, account, "subject-"+name); err != nil {
		t.Fatal(err)
	}
	if err := store.PublishPlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if _, err := subscription.New(store.Subscriptions()).Observe(ctx, subscription.Observation{Snapshot: subscription.Snapshot{Account: account, Ref: ref, Status: "active", Assignments: []catalog.PlanAssignment{assignment}}, EventID: "acceptance-event-" + name, OccurredAt: start}); err != nil {
		t.Fatal(err)
	}
	schedule := credit.Schedule{Account: account, ID: "acceptance-schedule-" + name, Subscription: ref, Assignment: assignment, Anchor: start, State: credit.ScheduleActive, StateEffectiveAt: start, SourceID: "acceptance-event-" + name}
	if _, err := credit.NewAllowances(store.Allowances(), nil).PutSchedule(ctx, schedule, 0); err != nil {
		t.Fatal(err)
	}
	return allowanceAcceptanceFixture{store: store, account: account, plan: plan, schedule: schedule, start: start, end: end}
}

func acceptancePaid(account billing.AccountID, scheduleID, source string, effective, observed, start, end time.Time) credit.EligibilityObservation {
	return credit.EligibilityObservation{Account: account, ScheduleID: scheduleID, SourceID: source, EffectiveAt: effective, ObservedAt: observed, Eligibility: credit.Eligibility{Status: credit.EligibilityPaid, Evidence: []credit.EligibilityEvidence{{Kind: credit.EvidencePayment, Reference: source + "-invoice", Covered: billing.Period{Start: start, End: end}}}}}
}

func acceptanceTrial(account billing.AccountID, scheduleID, source string, effective, observed, start, end time.Time) credit.EligibilityObservation {
	return credit.EligibilityObservation{Account: account, ScheduleID: scheduleID, SourceID: source, EffectiveAt: effective, ObservedAt: observed, Eligibility: credit.Eligibility{Status: credit.EligibilityTrial, Evidence: []credit.EligibilityEvidence{{Kind: credit.EvidenceTrial, Reference: source + "-trial", Policy: "acceptance-trial-policy", Covered: billing.Period{Start: start, End: end}}}}}
}

func TestPostgresAllowanceUncoveredPeriodBecomesEligibleExactlyOnce(t *testing.T) {
	f := newAllowanceAcceptanceFixture(t, "late-payment")
	ctx := t.Context()
	janEnd := time.Date(2026, time.February, 1, 0, 0, 0, 0, time.UTC)
	febEnd := time.Date(2026, time.March, 1, 0, 0, 0, 0, time.UTC)
	first := acceptancePaid(f.account, f.schedule.ID, "late-payment-jan", f.start, janEnd, f.start, janEnd)
	result, err := issueDueForTest(ctx, credit.NewAllowances(f.store.Allowances(), nil), allowanceIssueRequest{Account: f.account, Now: janEnd.Add(-time.Microsecond), Limit: 10, Evidence: []credit.EligibilityObservation{first}})
	if err != nil || len(result.Issuances) != 1 || !result.Issuances[0].Period.Start.Equal(f.start) {
		t.Fatalf("initial covered period = %#v, err=%v", result, err)
	}
	later := acceptancePaid(f.account, f.schedule.ID, "late-payment-feb", janEnd, febEnd, janEnd, febEnd)
	result, err = issueDueForTest(ctx, credit.NewAllowances(f.store.Allowances(), nil), allowanceIssueRequest{Account: f.account, Now: febEnd, Limit: 10, Evidence: []credit.EligibilityObservation{later}})
	if err != nil || len(result.Issuances) != 1 || !result.Issuances[0].Period.Start.Equal(janEnd) {
		t.Fatalf("later covered period = %#v, err=%v", result, err)
	}
	replay, err := issueDueForTest(ctx, credit.NewAllowances(f.store.Allowances(), nil), allowanceIssueRequest{Account: f.account, Now: febEnd, Limit: 10})
	if err != nil || len(replay.Issuances) != 0 {
		t.Fatalf("late payment replay = %#v, err=%v", replay, err)
	}
}

func TestPostgresAllowanceTrialThenPaidDoesNotDoubleGrant(t *testing.T) {
	f := newAllowanceAcceptanceFixture(t, "trial-paid")
	ctx := t.Context()
	janEnd := time.Date(2026, time.February, 1, 0, 0, 0, 0, time.UTC)
	trial := acceptanceTrial(f.account, f.schedule.ID, "trial-paid-trial", f.start, f.start.Add(time.Hour), f.start, janEnd)
	result, err := issueDueForTest(ctx, credit.NewAllowances(f.store.Allowances(), nil), allowanceIssueRequest{Account: f.account, Now: janEnd.Add(-time.Microsecond), Limit: 10, Evidence: []credit.EligibilityObservation{trial}})
	if err != nil || len(result.Issuances) != 1 {
		t.Fatalf("trial issuance = %#v, err=%v", result, err)
	}
	paid := acceptancePaid(f.account, f.schedule.ID, "trial-paid-payment", f.start, janEnd.Add(time.Hour), f.start, janEnd)
	result, err = issueDueForTest(ctx, credit.NewAllowances(f.store.Allowances(), nil), allowanceIssueRequest{Account: f.account, Now: janEnd, Limit: 10, Evidence: []credit.EligibilityObservation{paid}})
	if err != nil || len(result.Issuances) != 0 {
		t.Fatalf("trial to paid replay = %#v, err=%v", result, err)
	}
}

func TestPostgresAllowancePauseAndCancelRetainHistoryAndSuppressFuturePeriods(t *testing.T) {
	for _, state := range []credit.ScheduleState{credit.SchedulePaused, credit.ScheduleCanceled} {
		t.Run(string(state), func(t *testing.T) {
			f := newAllowanceAcceptanceFixture(t, "state-"+string(state))
			ctx := t.Context()
			boundary := time.Date(2026, time.February, 1, 0, 0, 0, 0, time.UTC)
			coverage := acceptancePaid(f.account, f.schedule.ID, "state-payment-"+string(state), f.start, f.start, f.start, f.end)
			before, err := issueDueForTest(ctx, credit.NewAllowances(f.store.Allowances(), nil), allowanceIssueRequest{Account: f.account, Now: boundary.Add(-time.Microsecond), Limit: 10, Evidence: []credit.EligibilityObservation{coverage}})
			if err != nil || len(before.Issuances) != 1 {
				t.Fatalf("pre-boundary issuance = %#v, err=%v", before, err)
			}
			updated := f.schedule
			updated.State = state
			updated.StateEffectiveAt = boundary
			updated.SourceID = "state-event-" + string(state)
			if _, err := credit.NewAllowances(f.store.Allowances(), nil).PutSchedule(ctx, updated, 1); err != nil {
				t.Fatal(err)
			}
			after, err := issueDueForTest(ctx, credit.NewAllowances(f.store.Allowances(), nil), allowanceIssueRequest{Account: f.account, Now: time.Date(2026, time.March, 1, 0, 0, 0, 0, time.UTC), Limit: 10})
			if err != nil || len(after.Issuances) != 0 {
				t.Fatalf("post-boundary %s issuance = %#v, err=%v", state, after, err)
			}
			var count int
			if err := f.store.db.QueryRowContext(ctx, `SELECT count(*) FROM billing_allowance_issuances WHERE account_id=$1 AND schedule_id=$2`, f.account, f.schedule.ID).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 1 {
				t.Fatalf("%s retained %d historical issuances, want 1", state, count)
			}
		})
	}
}

func TestPostgresAllowanceEqualTimeEligibilityConflictIsOrderIndependent(t *testing.T) {
	for _, firstStatus := range []credit.EligibilityStatus{credit.EligibilityPaid, credit.EligibilityDelinquent} {
		t.Run(timeStatusName(firstStatus), func(t *testing.T) {
			f := newAllowanceAcceptanceFixture(t, "equal-time-"+timeStatusName(firstStatus))
			at := f.start
			first := acceptancePaid(f.account, f.schedule.ID, "equal-first", at, at, f.start, f.end)
			second := credit.EligibilityObservation{Account: f.account, ScheduleID: f.schedule.ID, SourceID: "equal-second", EffectiveAt: at, ObservedAt: at, Eligibility: credit.Eligibility{Status: credit.EligibilityDelinquent}}
			if firstStatus == credit.EligibilityDelinquent {
				first, second = second, first
			}
			if err := credit.NewAllowances(f.store.Allowances(), nil).RecordEligibility(t.Context(), first); err != nil {
				t.Fatal(err)
			}
			if err := credit.NewAllowances(f.store.Allowances(), nil).RecordEligibility(t.Context(), second); !errors.Is(err, billing.ErrConflict) {
				t.Fatalf("equal-time conflict after %v = %v, want conflict", firstStatus, err)
			}
		})
	}
}

func TestPostgresAllowanceEligibilityHistoryBeyondOneThousand(t *testing.T) {
	f := newAllowanceAcceptanceFixture(t, "eligibility-history-bound")
	ctx := t.Context()
	raw, err := json.Marshal(acceptancePaid(f.account, f.schedule.ID, "history-paid", f.start, f.start, f.start, f.end).Eligibility)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i <= 1000; i++ {
		at := f.start.Add(-time.Duration(i) * time.Microsecond)
		if _, err := f.store.db.ExecContext(ctx, `INSERT INTO billing_allowance_eligibility_events(account_id,schedule_id,source_id,effective_at,observed_at,status,eligibility,fingerprint) VALUES($1,$2,$3,$4,$4,'paid',$5,$6)`, f.account, f.schedule.ID, "eligibility-history-"+time.Duration(i).String(), at, raw, "eligibility-fingerprint-"+time.Duration(i).String()); err != nil {
			t.Fatal(err)
		}
	}
	out, err := issueDueForTest(ctx, credit.NewAllowances(f.store.Allowances(), nil), allowanceIssueRequest{Account: f.account, Now: f.start, Limit: 10})
	if err != nil || len(out.Issuances) != 1 || out.Issuances[0].Amount != 10 {
		t.Fatalf("bounded eligibility history issuance=%+v error=%v", out, err)
	}
}

func timeStatusName(status credit.EligibilityStatus) string {
	if status == credit.EligibilityPaid {
		return "paid-first"
	}
	return "delinquent-first"
}

func TestPostgresAllowanceUpgradeDowngradeUpgradeKeepsPeriodHighWater(t *testing.T) {
	f := newAllowanceAcceptanceFixture(t, "upgrade-cycle")
	ctx := t.Context()
	upPlan := f.plan
	upPlan.ID = "acceptance-plan-upgrade-cycle-up"
	upPlan.PlanID = "acceptance-upgrade-cycle-up"
	upPlan.Version = 2
	upPlan.Allowances = append([]catalog.AllowanceDefinition(nil), f.plan.Allowances...)
	upPlan.Allowances[0].Amount = 20
	downPlan := f.plan
	downPlan.ID = "acceptance-plan-upgrade-cycle-down"
	downPlan.PlanID = "acceptance-upgrade-cycle-down"
	downPlan.Version = 3
	if err := f.store.PublishPlan(ctx, upPlan); err != nil {
		t.Fatal(err)
	}
	if err := f.store.PublishPlan(ctx, downPlan); err != nil {
		t.Fatal(err)
	}
	midCycle := time.Date(2026, time.February, 10, 0, 0, 0, 0, time.UTC)
	boundary := time.Date(2026, time.February, 28, 0, 0, 0, 0, time.UTC)
	secondUpgrade := time.Date(2026, time.March, 10, 0, 0, 0, 0, time.UTC)
	assignUp := f.schedule.Assignment
	assignUp.ID = "acceptance-assignment-upgrade-cycle-up"
	assignUp.PlanVersionID = upPlan.ID
	assignUp.Effective.Start = midCycle
	assignDown := f.schedule.Assignment
	assignDown.ID = "acceptance-assignment-upgrade-cycle-down"
	assignDown.PlanVersionID = downPlan.ID
	assignDown.Effective.Start = boundary
	assignUpAgain := assignUp
	assignUpAgain.ID = "acceptance-assignment-upgrade-cycle-up-again"
	assignUpAgain.Effective.Start = secondUpgrade
	ref := f.schedule.Subscription
	if _, err := subscription.New(f.store.Subscriptions()).Observe(ctx, subscription.Observation{Snapshot: subscription.Snapshot{Account: f.account, Ref: ref, Status: "active", Assignments: []catalog.PlanAssignment{f.schedule.Assignment, assignUp, assignDown, assignUpAgain}}, EventID: "acceptance-upgrade-cycle-assignments", OccurredAt: midCycle}); err != nil {
		t.Fatal(err)
	}
	oldEvidence := acceptancePaid(f.account, f.schedule.ID, "upgrade-cycle-old-payment", f.start, f.start, f.start, f.end)
	initial, err := issueDueForTest(ctx, credit.NewAllowances(f.store.Allowances(), nil), allowanceIssueRequest{Account: f.account, Now: time.Date(2026, time.February, 1, 0, 0, 0, 0, time.UTC), Limit: 20, Evidence: []credit.EligibilityObservation{oldEvidence}})
	if err != nil || len(initial.Issuances) != 1 || initial.Issuances[0].Amount != 10 {
		t.Fatalf("initial cycle grant = %#v, err=%v", initial, err)
	}
	upSchedule := credit.Schedule{Account: f.account, ID: "acceptance-schedule-upgrade-cycle-up", Subscription: f.schedule.Subscription, Assignment: assignUp, State: credit.ScheduleActive, StateEffectiveAt: midCycle, PreviousScheduleID: f.schedule.ID, Adjustment: credit.AdjustmentDelta, SourceID: "acceptance-upgrade-cycle-up"}
	if _, err := credit.NewAllowances(f.store.Allowances(), nil).PutSchedule(ctx, upSchedule, 0); err != nil {
		t.Fatal(err)
	}
	upEvidence := oldEvidence
	upEvidence.ScheduleID = upSchedule.ID
	upEvidence.SourceID = "upgrade-cycle-up-payment"
	upResult, err := issueDueForTest(ctx, credit.NewAllowances(f.store.Allowances(), nil), allowanceIssueRequest{Account: f.account, Now: time.Date(2026, time.February, 15, 0, 0, 0, 0, time.UTC), Limit: 20, Evidence: []credit.EligibilityObservation{upEvidence}})
	if err != nil || len(upResult.Issuances) != 1 || upResult.Issuances[0].Amount != 10 {
		t.Fatalf("first upgrade delta = %#v, err=%v", upResult, err)
	}
	downSchedule := credit.Schedule{Account: f.account, ID: "acceptance-schedule-upgrade-cycle-down", Subscription: f.schedule.Subscription, Assignment: assignDown, State: credit.ScheduleActive, StateEffectiveAt: boundary, PreviousScheduleID: upSchedule.ID, SourceID: "acceptance-upgrade-cycle-down"}
	if _, err := credit.NewAllowances(f.store.Allowances(), nil).PutSchedule(ctx, downSchedule, 0); err != nil {
		t.Fatal(err)
	}
	downEvidence := upEvidence
	downEvidence.ScheduleID = downSchedule.ID
	downEvidence.SourceID = "upgrade-cycle-down-payment"
	downResult, err := issueDueForTest(ctx, credit.NewAllowances(f.store.Allowances(), nil), allowanceIssueRequest{Account: f.account, Now: time.Date(2026, time.March, 1, 0, 0, 0, 0, time.UTC), Limit: 20, Evidence: []credit.EligibilityObservation{downEvidence}})
	if err != nil {
		t.Fatalf("downgrade boundary issuance = %#v, err=%v", downResult, err)
	}
	var downGrant *credit.Issuance
	for i := range downResult.Issuances {
		if downResult.Issuances[i].ScheduleID == downSchedule.ID {
			downGrant = &downResult.Issuances[i]
		}
	}
	if downGrant == nil || downGrant.Amount != 10 || !downGrant.Period.Start.Equal(boundary) {
		t.Fatalf("downgrade boundary grant = %#v, all issuances=%#v", downGrant, downResult.Issuances)
	}
	upAgain := credit.Schedule{Account: f.account, ID: "acceptance-schedule-upgrade-cycle-up-again", Subscription: f.schedule.Subscription, Assignment: assignUpAgain, State: credit.ScheduleActive, StateEffectiveAt: secondUpgrade, PreviousScheduleID: downSchedule.ID, Adjustment: credit.AdjustmentDelta, SourceID: "acceptance-upgrade-cycle-up-again"}
	if _, err := credit.NewAllowances(f.store.Allowances(), nil).PutSchedule(ctx, upAgain, 0); err != nil {
		t.Fatal(err)
	}
	upAgainEvidence := downEvidence
	upAgainEvidence.ScheduleID = upAgain.ID
	upAgainEvidence.SourceID = "upgrade-cycle-up-again-payment"
	upAgainResult, err := issueDueForTest(ctx, credit.NewAllowances(f.store.Allowances(), nil), allowanceIssueRequest{Account: f.account, Now: time.Date(2026, time.March, 15, 0, 0, 0, 0, time.UTC), Limit: 20, Evidence: []credit.EligibilityObservation{upAgainEvidence}})
	if err != nil {
		t.Fatalf("second upgrade issuance = %#v, err=%v", upAgainResult, err)
	}
	var upAgainGrant *credit.Issuance
	for i := range upAgainResult.Issuances {
		if upAgainResult.Issuances[i].ScheduleID == upAgain.ID {
			upAgainGrant = &upAgainResult.Issuances[i]
		}
	}
	if upAgainGrant == nil || upAgainGrant.Amount != 10 || !upAgainGrant.Period.Start.Equal(boundary) {
		t.Fatalf("second upgrade delta = %#v, all issuances=%#v", upAgainGrant, upAgainResult.Issuances)
	}
	replay, err := issueDueForTest(ctx, credit.NewAllowances(f.store.Allowances(), nil), allowanceIssueRequest{Account: f.account, Now: time.Date(2026, time.March, 15, 0, 0, 0, 0, time.UTC), Limit: 20})
	if err != nil || len(replay.Issuances) != 0 {
		t.Fatalf("second upgrade replay = %#v, err=%v", replay, err)
	}
}
