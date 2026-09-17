package pg

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/catalog"
	"github.com/data-insights-ai/rho-billing/integration"
)

func TestAllowanceSuccessorTriggerMaintainsMinimumAndAccountScope(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	plan := catalog.PlanVersion{ID: "successor-trigger-plan", PlanID: "successor-trigger-plan", Version: 1}
	if err := store.PublishPlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	accounts := []billing.AccountID{"successor-trigger-a", "successor-trigger-b"}
	for _, account := range accounts {
		if err := store.CreateAccount(ctx, account, "subject-"+string(account)); err != nil {
			t.Fatal(err)
		}
		insertSuccessorSchedule(t, db, account, plan.ID, "root", "", "root-assignment", time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC))
	}
	start := time.Date(2026, 2, 10, 0, 0, 0, 0, time.UTC)
	later := start.Add(24 * time.Hour)
	earlier := start.Add(-24 * time.Hour)
	insertSuccessorSchedule(t, db, accounts[0], plan.ID, "later", "root", "assignment-later", later)
	insertSuccessorSchedule(t, db, accounts[0], plan.ID, "earlier", "root", "assignment-earlier", earlier)
	insertSuccessorSchedule(t, db, accounts[1], plan.ID, "same-id", "root", "assignment-other", start)

	var got time.Time
	if err := db.QueryRowContext(ctx, `SELECT effective_at FROM billing_allowance_successors WHERE account_id=$1 AND schedule_id='root'`, accounts[0]).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if !got.Equal(earlier) {
		t.Fatalf("account A successor effective_at=%s, want %s", got, earlier)
	}
	var other time.Time
	if err := db.QueryRowContext(ctx, `SELECT effective_at FROM billing_allowance_successors WHERE account_id=$1 AND schedule_id='root'`, accounts[1]).Scan(&other); err != nil {
		t.Fatal(err)
	}
	if !other.Equal(start) {
		t.Fatalf("account B successor effective_at=%s, want %s", other, start)
	}

	if err := store.Atomic(ctx, accounts[0], func(v integration.Session) error {
		got, err := v.(*session).allowanceSuccessorEffectiveAt(ctx, "later")
		if err != nil {
			return err
		}
		if !got.IsZero() {
			return fmt.Errorf("leaf successor effective_at=%s, want zero", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	aborted := errors.New("abort successor trigger")
	if err := store.Atomic(ctx, accounts[0], func(v integration.Session) error {
		insertSuccessorSchedule(t, v.(*session).tx, accounts[0], plan.ID, "aborted", "root", "assignment-aborted", earlier.Add(-time.Hour))
		return aborted
	}); !errors.Is(err, aborted) {
		t.Fatalf("aborted successor insert error=%v, want sentinel", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT effective_at FROM billing_allowance_successors WHERE account_id=$1 AND schedule_id='root'`, accounts[0]).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if !got.Equal(earlier) {
		t.Fatalf("rolled-back successor effective_at=%s, want %s", got, earlier)
	}
}

func insertSuccessorSchedule(t *testing.T, q querier, account billing.AccountID, planID, id, previous, assignmentID string, start time.Time) {
	t.Helper()
	assignment, err := json.Marshal(catalog.PlanAssignment{ID: assignmentID, PlanVersionID: planID, Quantity: 1, Effective: billing.Period{Start: start, End: start.AddDate(1, 0, 0)}, Source: catalog.SourceSubscription})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.ExecContext(t.Context(), `INSERT INTO billing_allowance_schedules
(account_id,schedule_id,subscription_id,subscription_provider,subscription_merchant,subscription_environment,assignment_id,plan_version_id,assignment,anchor,state,state_effective_at,revision,previous_schedule_id,adjustment_mode,source_id,created_at,updated_at)
VALUES($1,$2,'lineage-sub','provider','merchant','sandbox',$3,$4,$5,$6,'active',$6,1,NULLIF($7,''),'initial',$2,$6,$6)`, account, id, assignmentID, planID, assignment, start, previous); err != nil {
		t.Fatal(err)
	}
}
