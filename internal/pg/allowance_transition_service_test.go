package pg

import (
	"errors"
	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/catalog"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/subscription"
	"slices"
	"testing"
	"time"
)

func TestAllowanceTransitionRequiresEachRecurrenceBoundary(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("mixed-recurrence")
	if err := store.CreateAccount(ctx, account, "mixed-recurrence"); err != nil {
		t.Fatal(err)
	}
	old := allowancePlan()
	old.Allowances = append(old.Allowances, catalog.AllowanceDefinition{ID: "annual", Unit: old.Allowances[0].Unit, Amount: 100, Recurrence: catalog.AllowanceAnnual, Scope: catalog.AllowanceAccount, SpendScope: "AI_STANDARD"})
	next := old
	next.ID = "mixed-lower"
	next.Version = 2
	next.Allowances = slices.Clone(old.Allowances)
	next.Allowances[1].Amount = 50
	for _, plan := range []catalog.PlanVersion{old, next} {
		if err := store.PublishPlan(ctx, plan); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ref := billing.Reference{Scope: billing.Scope{Provider: "sim", Merchant: "mixed", Environment: "sandbox"}, ID: "subscription"}
	prior := catalog.PlanAssignment{ID: "prior", PlanVersionID: old.ID, Quantity: 1, Effective: billing.Period{Start: start, End: start.AddDate(2, 0, 0)}, Source: catalog.SourceSubscription}
	successor := prior
	successor.ID = "successor"
	successor.PlanVersionID = next.ID
	successor.Effective.Start = start.AddDate(0, 1, 0)
	if _, err := subscription.New(store.Subscriptions()).Observe(ctx, subscription.Observation{Snapshot: subscription.Snapshot{Account: account, Ref: ref, Status: "active", Assignments: []catalog.PlanAssignment{prior, successor}}, EventID: "observed", OccurredAt: start}); err != nil {
		t.Fatal(err)
	}
	service := credit.NewAllowances(store.Allowances(), nil)
	root := credit.Schedule{Account: account, ID: "root", Subscription: ref, Assignment: prior, Anchor: start, State: credit.ScheduleActive, StateEffectiveAt: start, SourceID: "observed"}
	if _, err := service.PutSchedule(ctx, root, 0); err != nil {
		t.Fatal(err)
	}
	child := root
	child.ID = "child"
	child.PreviousScheduleID = root.ID
	child.Assignment = successor
	child.StateEffectiveAt = successor.Effective.Start
	for _, mode := range []credit.AdjustmentMode{0, credit.AdjustmentDelta} {
		child.Adjustment = mode
		if _, err := service.PutSchedule(ctx, child, 0); !errors.Is(err, credit.ErrAdjustment) {
			t.Fatalf("mid-annual downgrade mode %v: %v", mode, err)
		}
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM billing_allowance_schedules WHERE account_id=$1`, account).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("rejected change persisted %d schedules", count)
	}
	successor.Effective.Start = start.AddDate(1, 0, 0)
	if _, err := subscription.New(store.Subscriptions()).Observe(ctx, subscription.Observation{Snapshot: subscription.Snapshot{Account: account, Ref: ref, Status: "active", Assignments: []catalog.PlanAssignment{prior, successor}}, EventID: "rescheduled", OccurredAt: start.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	child.Assignment = successor
	child.StateEffectiveAt = successor.Effective.Start
	child.Adjustment = 0
	if _, err := service.PutSchedule(ctx, child, 0); err != nil {
		t.Fatalf("common monthly and annual boundary: %v", err)
	}
}
