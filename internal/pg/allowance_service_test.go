package pg

import (
	"errors"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/catalog"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/integration"
	"github.com/data-insights-ai/rho-billing/subscription"
)

func TestPostgresAllowanceServiceUsesAtomicSession(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("allowance-service")
	if err := store.CreateAccount(ctx, account, "allowance-service-subject"); err != nil {
		t.Fatal(err)
	}
	plan := allowancePlan()
	if err := store.PublishPlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, time.May, 1, 0, 0, 0, 0, time.UTC)
	ref := billing.Reference{Scope: billing.Scope{Provider: "sim", Merchant: "allowance-service-merchant", Environment: "sandbox"}, ID: "allowance-service-subscription"}
	assignment := catalog.PlanAssignment{ID: "allowance-service-assignment", PlanVersionID: plan.ID, Quantity: 1, Effective: billing.Period{Start: start, End: end}, Source: catalog.SourceSubscription}
	if _, err := subscription.New(store.Subscriptions()).Observe(ctx, subscription.Observation{Snapshot: subscription.Snapshot{Account: account, Ref: ref, Status: "active", Assignments: []catalog.PlanAssignment{assignment}}, EventID: "allowance-service-event", OccurredAt: start}); err != nil {
		t.Fatal(err)
	}
	schedule := credit.Schedule{Account: account, ID: "allowance-service-schedule", Subscription: ref, Assignment: assignment, Anchor: start, State: credit.ScheduleActive, StateEffectiveAt: start, SourceID: "allowance-service-event"}
	sentinel := errors.New("rollback allowance service transaction")
	err := store.Atomic(ctx, account, func(scope integration.Session) error {
		if _, err := credit.NewAllowances(scope.Allowances(), nil).PutSchedule(ctx, schedule, 0); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Atomic error = %v, want sentinel", err)
	}
	var schedules, history, lineage, successors int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM billing_allowance_schedules WHERE account_id=$1`, account).Scan(&schedules); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM billing_allowance_schedule_history WHERE account_id=$1`, account).Scan(&history); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM billing_allowance_lineage WHERE account_id=$1`, account).Scan(&lineage); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM billing_allowance_successors WHERE account_id=$1`, account).Scan(&successors); err != nil {
		t.Fatal(err)
	}
	if schedules != 0 || history != 0 || lineage != 0 || successors != 0 {
		t.Fatalf("rolled-back allowance effects remain: schedules=%d history=%d lineage=%d successors=%d", schedules, history, lineage, successors)
	}
	stamp := time.Date(2026, 2, 3, 4, 5, 6, 123456789, time.FixedZone("service-clock", 3600))
	if _, err := credit.NewAllowances(store.Allowances(), func() time.Time { return stamp }).PutSchedule(ctx, schedule, 0); err != nil {
		t.Fatal(err)
	}
	var recordedAt, createdAt time.Time
	if err := db.QueryRowContext(ctx, `SELECT s.created_at,h.recorded_at FROM billing_allowance_schedules s
 JOIN billing_allowance_schedule_history h USING(account_id,schedule_id,revision)
 WHERE s.account_id=$1 AND s.schedule_id=$2`, account, schedule.ID).Scan(&createdAt, &recordedAt); err != nil {
		t.Fatal(err)
	}
	if !createdAt.Equal(billing.CanonicalTime(stamp)) || !recordedAt.Equal(billing.CanonicalTime(stamp)) {
		t.Fatalf("service clock lost: created=%v recorded=%v", createdAt, recordedAt)
	}
}

func TestPostgresAllowanceServiceRejectsCrossAccountSchedule(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	for _, account := range []billing.AccountID{"allowance-service-a", "allowance-service-b"} {
		if err := store.CreateAccount(ctx, account, string(account)+"-subject"); err != nil {
			t.Fatal(err)
		}
	}
	plan := allowancePlan()
	if err := store.PublishPlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	schedule := credit.Schedule{
		Account: "allowance-service-b", ID: "allowance-service-cross-account", Subscription: billing.Reference{Scope: billing.Scope{Provider: "sim", Merchant: "allowance-service-merchant", Environment: "sandbox"}, ID: "allowance-service-cross-account-subscription"},
		Assignment: catalog.PlanAssignment{ID: "allowance-service-cross-account-assignment", PlanVersionID: plan.ID, Quantity: 1, Effective: billing.Period{Start: start, End: start.AddDate(0, 1, 0)}, Source: catalog.SourceSubscription},
		Anchor:     start, State: credit.ScheduleActive, StateEffectiveAt: start, SourceID: "allowance-service-cross-account-event",
	}
	err := store.Atomic(ctx, "allowance-service-a", func(scope integration.Session) error {
		_, err := credit.NewAllowances(scope.Allowances(), nil).PutSchedule(ctx, schedule, 0)
		return err
	})
	if !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("cross-account schedule error = %v, want not found", err)
	}
}
