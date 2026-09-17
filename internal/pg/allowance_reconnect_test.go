package pg

import (
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/catalog"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/subscription"
)

func TestPostgresAllowanceCheckpointResumesAfterStoreReconnect(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("allowance-reconnect")
	if err := store.CreateAccount(ctx, account, "allowance-reconnect-subject"); err != nil {
		t.Fatal(err)
	}
	plan := allowancePlan()
	if err := store.PublishPlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, time.April, 1, 0, 0, 0, 0, time.UTC)
	ref := billing.Reference{Scope: billing.Scope{Provider: "sim", Merchant: "allowance-reconnect-merchant", Environment: "sandbox"}, ID: "allowance-reconnect-subscription"}
	assignment := catalog.PlanAssignment{ID: "allowance-reconnect-assignment", PlanVersionID: plan.ID, Quantity: 1, Effective: billing.Period{Start: start, End: end}, Source: catalog.SourceSubscription}
	if _, err := subscription.New(store.Subscriptions()).Observe(ctx, subscription.Observation{Snapshot: subscription.Snapshot{Account: account, Ref: ref, Status: "active", Assignments: []catalog.PlanAssignment{assignment}}, EventID: "allowance-reconnect-event", OccurredAt: start}); err != nil {
		t.Fatal(err)
	}
	schedule := credit.Schedule{Account: account, ID: "allowance-reconnect-schedule", Subscription: ref, Assignment: assignment, Anchor: start, State: credit.ScheduleActive, StateEffectiveAt: start, SourceID: "allowance-reconnect-event"}
	if _, err := credit.NewAllowances(store.Allowances(), nil).PutSchedule(ctx, schedule, 0); err != nil {
		t.Fatal(err)
	}
	evidence := acceptancePaid(account, schedule.ID, "allowance-reconnect-payment", start, start, start, end)
	first, err := credit.NewAllowances(store.Allowances(), nil).AdvanceCheckpoint(ctx, credit.CheckpointRequest{Account: account, ID: "reconnect-worker", Now: end, Limit: 10, MaxPeriods: 1, Evidence: []credit.EligibilityObservation{evidence}})
	if err != nil || len(first.Issuances) != 1 || !first.HasMore {
		t.Fatalf("first checkpoint=%+v error=%v", first, err)
	}

	recovered := credit.NewAllowances(New(secondQueueDB(t, db)).Allowances(), nil)
	status := first
	issued := len(first.Issuances)
	for i := 0; i < 32 && status.HasMore; i++ {
		status, err = recovered.AdvanceCheckpoint(ctx, credit.CheckpointRequest{Account: account, ID: "reconnect-worker", Now: end, Limit: 10, MaxPeriods: 1})
		if err != nil {
			t.Fatal(err)
		}
		issued += len(status.Issuances)
	}
	if issued != 3 || status.HasMore {
		t.Fatalf("issuances=%d hasMore=%v status=%+v, want 3 periods", issued, status.HasMore, status)
	}
	got, err := credit.New(New(secondQueueDB(t, db)), func() time.Time { return end }).Balance(ctx, account, "credits", "")
	if err != nil || got.Available+got.Expired != 30 || got.Expired != 30 {
		t.Fatalf("reconnect allowance balance=%+v err=%v, want 30 expired monthly grants", got, err)
	}
}
