package pg

import (
	"errors"
	"slices"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/catalog"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/integration"
	"github.com/data-insights-ai/rho-billing/subscription"
)

func TestPostgresAllowanceLineagePreservesRootAndTransitionAnchors(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("lineage-runtime")
	if err := store.CreateAccount(ctx, account, "lineage-runtime-subject"); err != nil {
		t.Fatal(err)
	}
	planV1 := catalog.PlanVersion{ID: "lineage-runtime-v1", PlanID: "lineage-runtime", Version: 1, Allowances: []catalog.AllowanceDefinition{{ID: "monthly", Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 10, Recurrence: catalog.AllowanceMonthly, Scope: catalog.AllowanceAccount, SpendScope: "AI_STANDARD"}}}
	planV2 := planV1
	planV2.Allowances = slices.Clone(planV1.Allowances)
	planV2.ID, planV2.Version = "lineage-runtime-v2", 2
	planV2.Allowances[0].Amount = 20
	for _, plan := range []catalog.PlanVersion{planV1, planV2} {
		if err := store.PublishPlan(ctx, plan); err != nil {
			t.Fatal(err)
		}
	}
	ref := billing.Reference{Scope: billing.Scope{Provider: "lineage-provider", Merchant: "lineage-merchant", Environment: "sandbox"}, ID: "lineage-sub"}
	rootAnchor := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	childAnchor := time.Date(2026, time.January, 15, 0, 0, 0, 0, time.UTC)
	grandchildStart := time.Date(2026, time.February, 15, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, time.April, 1, 0, 0, 0, 0, time.UTC)
	root := catalog.PlanAssignment{ID: "lineage-root-assignment", PlanVersionID: planV1.ID, Quantity: 1, Effective: billing.Period{Start: rootAnchor, End: end}, Source: catalog.SourceSubscription}
	child := catalog.PlanAssignment{ID: "lineage-child-assignment", PlanVersionID: planV2.ID, Quantity: 1, Effective: billing.Period{Start: childAnchor, End: end}, Source: catalog.SourceSubscription}
	grandchild := catalog.PlanAssignment{ID: "lineage-grandchild-assignment", PlanVersionID: planV2.ID, Quantity: 1, Effective: billing.Period{Start: grandchildStart, End: end}, Source: catalog.SourceSubscription}
	observe := func(event string, occurred time.Time, assignments ...catalog.PlanAssignment) {
		t.Helper()
		if _, err := subscription.New(store.Subscriptions()).Observe(ctx, subscription.Observation{Snapshot: subscription.Snapshot{Account: account, Ref: ref, Status: "active", Assignments: assignments}, EventID: event, OccurredAt: occurred}); err != nil {
			t.Fatal(err)
		}
	}
	observe("lineage-root-event", rootAnchor, root)
	rootSchedule := credit.Schedule{Account: account, ID: "lineage-root-schedule", Subscription: ref, Assignment: root, Anchor: rootAnchor, State: credit.ScheduleActive, StateEffectiveAt: rootAnchor, SourceID: "lineage-root-event"}
	if _, err := credit.NewAllowances(store.Allowances(), nil).PutSchedule(ctx, rootSchedule, 0); err != nil {
		t.Fatal(err)
	}
	observe("lineage-child-event", childAnchor, root, child)
	childSchedule := credit.Schedule{Account: account, ID: "lineage-child-schedule", Subscription: rootSchedule.Subscription, Assignment: child, Anchor: childAnchor, State: credit.ScheduleActive, StateEffectiveAt: childAnchor, PreviousScheduleID: rootSchedule.ID, Adjustment: credit.AdjustmentDelta, SourceID: "lineage-child-event"}
	if _, err := credit.NewAllowances(store.Allowances(), nil).PutSchedule(ctx, childSchedule, 0); err != nil {
		t.Fatal(err)
	}
	observe("lineage-grandchild-event", grandchildStart, root, child, grandchild)
	grandchildSchedule := credit.Schedule{Account: account, ID: "lineage-grandchild-schedule", Subscription: rootSchedule.Subscription, Assignment: grandchild, State: credit.ScheduleActive, StateEffectiveAt: grandchildStart, PreviousScheduleID: childSchedule.ID, SourceID: "lineage-grandchild-event"}
	if _, err := credit.NewAllowances(store.Allowances(), nil).PutSchedule(ctx, grandchildSchedule, 0); err != nil {
		t.Fatal(err)
	}

	rows, err := db.QueryContext(ctx, `SELECT schedule_id,root_assignment_id,root_anchor,transition_anchor FROM billing_allowance_lineage WHERE account_id=$1 ORDER BY schedule_id`, account)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := make(map[string]struct {
		assignment       string
		root, transition time.Time
	})
	for rows.Next() {
		var id, assignment string
		var root, transition time.Time
		if err := rows.Scan(&id, &assignment, &root, &transition); err != nil {
			t.Fatal(err)
		}
		got[id] = struct {
			assignment       string
			root, transition time.Time
		}{assignment, root, transition}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{rootSchedule.ID, childSchedule.ID, grandchildSchedule.ID} {
		if _, ok := got[id]; !ok {
			t.Fatalf("missing lineage row for %s", id)
		}
	}
	for _, id := range []string{childSchedule.ID, grandchildSchedule.ID} {
		lineage := got[id]
		if lineage.assignment != root.ID || !lineage.root.Equal(rootAnchor) || !lineage.transition.Equal(childAnchor) {
			t.Fatalf("%s lineage = %+v, want root assignment %q, root %s, transition %s", id, lineage, root.ID, rootAnchor, childAnchor)
		}
	}
	if err := store.Atomic(ctx, account, func(v integration.Session) error {
		return v.Allowances().WithinAccount(ctx, account, func(tx credit.AllowanceTx) error {
			lineage, err := tx.Lineage(ctx, grandchildSchedule.ID)
			if err != nil {
				return err
			}
			if lineage.RootAssignment != root.ID || !lineage.RootAnchor.Equal(rootAnchor) || !lineage.TransitionAnchor.Equal(childAnchor) {
				t.Fatalf("lineage = %+v, want root assignment %q, root %s, transition %s", lineage, root.ID, rootAnchor, childAnchor)
			}
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresAllowanceLineageRejectsIdentityChangesAndAllowsStateChanges(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("lineage-identity")
	if err := store.CreateAccount(ctx, account, "lineage-identity-subject"); err != nil {
		t.Fatal(err)
	}
	plan := catalog.PlanVersion{ID: "lineage-identity-plan", PlanID: "lineage-identity-plan", Version: 1, Allowances: []catalog.AllowanceDefinition{{ID: "monthly", Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 10, Recurrence: catalog.AllowanceMonthly, Scope: catalog.AllowanceAccount, SpendScope: "AI_STANDARD"}}}
	if err := store.PublishPlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, time.April, 1, 0, 0, 0, 0, time.UTC)
	ref := billing.Reference{Scope: billing.Scope{Provider: "lineage-provider", Merchant: "identity-merchant", Environment: "sandbox"}, ID: "identity-sub"}
	assignment := catalog.PlanAssignment{ID: "identity-assignment", PlanVersionID: plan.ID, Quantity: 1, Effective: billing.Period{Start: start, End: end}, Source: catalog.SourceSubscription}
	if _, err := subscription.New(store.Subscriptions()).Observe(ctx, subscription.Observation{Snapshot: subscription.Snapshot{Account: account, Ref: ref, Status: "active", Assignments: []catalog.PlanAssignment{assignment}}, EventID: "identity-event", OccurredAt: start}); err != nil {
		t.Fatal(err)
	}
	schedule := credit.Schedule{Account: account, ID: "identity-schedule", Subscription: ref, Assignment: assignment, Anchor: start, State: credit.ScheduleActive, StateEffectiveAt: start, SourceID: "identity-event"}
	if _, err := credit.NewAllowances(store.Allowances(), nil).PutSchedule(ctx, schedule, 0); err != nil {
		t.Fatal(err)
	}
	var beforeHistory, beforeLineage int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM billing_allowance_schedule_history WHERE account_id=$1 AND schedule_id=$2`, account, schedule.ID).Scan(&beforeHistory); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM billing_allowance_lineage WHERE account_id=$1 AND schedule_id=$2`, account, schedule.ID).Scan(&beforeLineage); err != nil {
		t.Fatal(err)
	}
	changedAnchor := schedule
	changedAnchor.Anchor = start.Add(time.Hour)
	if _, err := credit.NewAllowances(store.Allowances(), nil).PutSchedule(ctx, changedAnchor, 1); !errors.Is(err, credit.ErrAdjustment) {
		t.Fatalf("changing anchor error = %v, want ErrAdjustment", err)
	}
	changedPrevious := schedule
	changedPrevious.PreviousScheduleID = "identity-other-schedule"
	if _, err := credit.NewAllowances(store.Allowances(), nil).PutSchedule(ctx, changedPrevious, 1); !errors.Is(err, credit.ErrAdjustment) {
		t.Fatalf("changing previous error = %v, want ErrAdjustment", err)
	}
	var afterHistory, afterLineage int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM billing_allowance_schedule_history WHERE account_id=$1 AND schedule_id=$2`, account, schedule.ID).Scan(&afterHistory); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM billing_allowance_lineage WHERE account_id=$1 AND schedule_id=$2`, account, schedule.ID).Scan(&afterLineage); err != nil {
		t.Fatal(err)
	}
	if afterHistory != beforeHistory || afterLineage != beforeLineage {
		t.Fatalf("rejected identity changes mutated history/lineage: before=%d/%d after=%d/%d", beforeHistory, beforeLineage, afterHistory, afterLineage)
	}
	paused := schedule
	paused.State, paused.StateEffectiveAt, paused.SourceID = credit.SchedulePaused, start.AddDate(0, 1, 0), "identity-pause"
	paused, err := credit.NewAllowances(store.Allowances(), nil).PutSchedule(ctx, paused, 1)
	if err != nil {
		t.Fatal(err)
	}
	resumed := paused
	resumed.State, resumed.StateEffectiveAt, resumed.SourceID = credit.ScheduleActive, start.AddDate(0, 2, 0), "identity-resume"
	if _, err := credit.NewAllowances(store.Allowances(), nil).PutSchedule(ctx, resumed, paused.Revision); err != nil {
		t.Fatal(err)
	}
	var rootAssignment string
	var root, transition time.Time
	if err := db.QueryRowContext(ctx, `SELECT root_assignment_id,root_anchor,transition_anchor FROM billing_allowance_lineage WHERE account_id=$1 AND schedule_id=$2`, account, schedule.ID).Scan(&rootAssignment, &root, &transition); err != nil {
		t.Fatal(err)
	}
	if rootAssignment != assignment.ID || !root.Equal(start) || !transition.Equal(start) {
		t.Fatalf("state changes altered lineage metadata: assignment=%q root=%s transition=%s", rootAssignment, root, transition)
	}
}

func TestPostgresAllowanceLineageRejectsNewSelfCycle(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("lineage-self-cycle")
	if err := store.CreateAccount(ctx, account, "lineage-self-cycle-subject"); err != nil {
		t.Fatal(err)
	}
	plan := catalog.PlanVersion{ID: "lineage-cycle-plan", PlanID: "lineage-cycle-plan", Version: 1, Allowances: []catalog.AllowanceDefinition{{ID: "monthly", Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 10, Recurrence: catalog.AllowanceMonthly, Scope: catalog.AllowanceAccount, SpendScope: "AI_STANDARD"}}}
	if err := store.PublishPlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	ref := billing.Reference{Scope: billing.Scope{Provider: "lineage-provider", Merchant: "cycle-merchant", Environment: "sandbox"}, ID: "cycle-sub"}
	assignment := catalog.PlanAssignment{ID: "cycle-assignment", PlanVersionID: plan.ID, Quantity: 1, Effective: billing.Period{Start: start, End: start.AddDate(0, 3, 0)}, Source: catalog.SourceSubscription}
	if _, err := subscription.New(store.Subscriptions()).Observe(ctx, subscription.Observation{Snapshot: subscription.Snapshot{Account: account, Ref: ref, Status: "active", Assignments: []catalog.PlanAssignment{assignment}}, EventID: "cycle-event", OccurredAt: start}); err != nil {
		t.Fatal(err)
	}
	schedule := credit.Schedule{Account: account, ID: "cycle-schedule", Subscription: ref, Assignment: assignment, Anchor: start, State: credit.ScheduleActive, StateEffectiveAt: start, PreviousScheduleID: "cycle-schedule", SourceID: "cycle-event"}
	if _, err := credit.NewAllowances(store.Allowances(), nil).PutSchedule(ctx, schedule, 0); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("self-cycle error = %v, want conflict", err)
	}
	var schedules, lineage int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM billing_allowance_schedules WHERE account_id=$1`, account).Scan(&schedules); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM billing_allowance_lineage WHERE account_id=$1`, account).Scan(&lineage); err != nil {
		t.Fatal(err)
	}
	if schedules != 0 || lineage != 0 {
		t.Fatalf("self-cycle partially committed schedules=%d lineage=%d", schedules, lineage)
	}
}
