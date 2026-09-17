package pg

import (
	"errors"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/catalog"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/subscription"
)

func TestAllowanceScheduleRequiresMatchingAssignmentSource(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	createTestAccounts(t, store)
	plan := allowancePlan()
	if err := store.PublishPlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	account := billing.AccountID("acct-a")
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	period := billing.Period{Start: start, End: start.AddDate(1, 0, 0)}
	ref := billing.Reference{Scope: billing.Scope{Provider: "sim", Merchant: "merchant", Environment: "sandbox"}, ID: "source-subscription"}
	assignment := catalog.PlanAssignment{ID: "source-assignment", PlanVersionID: plan.ID, Quantity: 1, Effective: period, Source: catalog.SourceSubscription}
	if _, err := subscription.New(store.Subscriptions()).Observe(ctx, subscription.Observation{Snapshot: subscription.Snapshot{Account: account, Ref: ref, Status: "active", Assignments: []catalog.PlanAssignment{assignment}}, EventID: "source-event", OccurredAt: start}); err != nil {
		t.Fatal(err)
	}
	wrong := assignment
	wrong.Source = catalog.SourceManual
	schedule := credit.Schedule{Account: account, ID: "source-schedule", Subscription: ref, Assignment: wrong, Anchor: start, State: credit.ScheduleActive, StateEffectiveAt: start, SourceID: "source-event"}
	if _, err := credit.NewAllowances(store.Allowances(), nil).PutSchedule(ctx, schedule, 0); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("mismatched assignment source error = %v, want conflict", err)
	}
	schedule.Assignment = assignment
	schedule.Subscription = billing.Reference{ID: ref.ID}
	if _, err := credit.NewAllowances(store.Allowances(), nil).PutSchedule(ctx, schedule, 0); !errors.Is(err, billing.ErrInvalid) {
		t.Fatalf("bare subscription ID error = %v, want invalid", err)
	}
	schedule.Subscription = ref
	schedule.Subscription.Scope.Environment = "production"
	if _, err := credit.NewAllowances(store.Allowances(), nil).PutSchedule(ctx, schedule, 0); !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("wrong subscription scope error = %v, want not found", err)
	}
	var schedules, history, financialEffects int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM billing_allowance_schedules WHERE account_id=$1`, account).Scan(&schedules); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM billing_allowance_schedule_history WHERE account_id=$1`, account).Scan(&history); err != nil {
		t.Fatal(err)
	}
	if schedules != 0 || history != 0 {
		t.Fatalf("mismatched assignment left schedule effects: schedules=%d history=%d", schedules, history)
	}
	if err := db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM billing_journal WHERE account_id=$1)+(SELECT count(*) FROM billing_allowance_issuances WHERE account_id=$1)`, account).Scan(&financialEffects); err != nil {
		t.Fatal(err)
	}
	if financialEffects != 0 {
		t.Fatalf("mismatched assignment left financial effects: %d", financialEffects)
	}
}

func TestAllowanceScopeDoesNotShareGrantOrHighWater(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	createTestAccounts(t, store)
	plan := allowancePlan()
	if err := store.PublishPlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	period := billing.Period{Start: start, End: start.AddDate(1, 0, 0)}
	assignment := catalog.PlanAssignment{ID: "same-assignment", PlanVersionID: plan.ID, Quantity: 1, Effective: period, Source: catalog.SourceSubscription}
	var evidence []credit.EligibilityObservation
	for _, env := range []string{"sandbox", "production"} {
		ref := billing.Reference{Scope: billing.Scope{Provider: "sim", Merchant: "merchant", Environment: env}, ID: "same-subscription"}
		if _, err := subscription.New(store.Subscriptions()).Observe(ctx, subscription.Observation{Snapshot: subscription.Snapshot{Account: "acct-a", Ref: ref, Status: "active", Assignments: []catalog.PlanAssignment{assignment}}, EventID: "same-event", OccurredAt: start}); err != nil {
			t.Fatal(err)
		}
		schedule := credit.Schedule{Account: "acct-a", ID: env, Subscription: ref, Assignment: assignment, Anchor: start, State: credit.ScheduleActive, StateEffectiveAt: start, SourceID: "same-event"}
		if _, err := credit.NewAllowances(store.Allowances(), nil).PutSchedule(ctx, schedule, 0); err != nil {
			t.Fatal(err)
		}
		evidence = append(evidence, credit.EligibilityObservation{Account: "acct-a", ScheduleID: env, SourceID: "coverage-" + env, EffectiveAt: start, ObservedAt: start, Eligibility: credit.Eligibility{Status: credit.EligibilityPaid, Evidence: []credit.EligibilityEvidence{{Kind: credit.EvidencePayment, Reference: "paid-" + env, Covered: period}}}})
	}
	out, err := issueDueForTest(ctx, credit.NewAllowances(store.Allowances(), nil), allowanceIssueRequest{Account: "acct-a", Now: start, Limit: 10, Evidence: evidence})
	if err != nil || len(out.Issuances) != 2 {
		t.Fatalf("scoped issuance %+v %v", out, err)
	}
	if out.Issuances[0].GrantKey == out.Issuances[1].GrantKey {
		t.Fatal("grant identities crossed environments")
	}
	engine := credit.New(store, func() time.Time { return start })
	b, err := engine.Balance(ctx, "acct-a", "credits", "")
	if err != nil || b.Available != 20 {
		t.Fatalf("scope capped an unrelated allowance %+v %v", b, err)
	}
	if _, err := engine.VerifyLedger(ctx, "acct-a"); err != nil {
		t.Fatal(err)
	}
}

func TestScheduleFutureStateCannotAuthorizeEarlierPeriod(t *testing.T) {
	start := testTime()
	future := credit.Schedule{ID: "future", State: credit.ScheduleActive, StateEffectiveAt: start.Add(time.Hour), Revision: 1}
	if state := scheduleStateAt([]credit.Schedule{future}, future, start); state.State == credit.ScheduleActive {
		t.Fatal("future active state authorized early grant")
	}
	if state := scheduleStateAt([]credit.Schedule{future}, future, future.StateEffectiveAt); state.State != credit.ScheduleActive {
		t.Fatal("exact effective boundary not active")
	}
}
