package pg

import (
	"errors"
	"fmt"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/catalog"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/integration"
	"github.com/data-insights-ai/rho-billing/subscription"
)

func TestPostgresAllowanceIssuanceServiceUsesOneOuterTransaction(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("allowance-issuance-service")
	if err := store.CreateAccount(ctx, account, "allowance-issuance-service-subject"); err != nil {
		t.Fatal(err)
	}
	plan := allowancePlan()
	if err := store.PublishPlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, time.February, 1, 0, 0, 0, 0, time.UTC)
	ref := billing.Reference{Scope: billing.Scope{Provider: "sim", Merchant: "allowance-issuance-service-merchant", Environment: "sandbox"}, ID: "allowance-issuance-service-subscription"}
	assignment := catalog.PlanAssignment{ID: "allowance-issuance-service-assignment", PlanVersionID: plan.ID, Quantity: 1, Effective: billing.Period{Start: start, End: end}, Source: catalog.SourceSubscription}
	if _, err := subscription.New(store.Subscriptions()).Observe(ctx, subscription.Observation{Snapshot: subscription.Snapshot{Account: account, Ref: ref, Status: "active", Assignments: []catalog.PlanAssignment{assignment}}, EventID: "allowance-issuance-service-event", OccurredAt: start}); err != nil {
		t.Fatal(err)
	}
	schedule := credit.Schedule{Account: account, ID: "allowance-issuance-service-schedule", Subscription: ref, Assignment: assignment, Anchor: start, State: credit.ScheduleActive, StateEffectiveAt: start, SourceID: "allowance-issuance-service-event"}
	if _, err := credit.NewAllowances(store.Allowances(), nil).PutSchedule(ctx, schedule, 0); err != nil {
		t.Fatal(err)
	}
	evidence := acceptancePaid(account, schedule.ID, "allowance-issuance-service-payment", start, start, start, end)
	request := allowanceIssueRequest{Account: account, Now: end, Limit: 10, Evidence: []credit.EligibilityObservation{evidence}}
	sentinel := errors.New("rollback allowance issuance service transaction")
	err := store.Atomic(ctx, account, func(scope integration.Session) error {
		service := credit.NewAllowances(scope.Allowances(), nil)
		if err := service.RecordEligibility(ctx, evidence); err != nil {
			return err
		}
		if _, err := issueDueForTest(ctx, service, allowanceIssueRequest{Account: account, Now: end, Limit: 10}); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Atomic error = %v, want sentinel", err)
	}
	var evidenceRows, journalRows, lotRows, issuanceRows, highWaterRows int
	if err := db.QueryRowContext(ctx, `SELECT
 (SELECT count(*) FROM billing_allowance_eligibility_events WHERE account_id=$1)+
 (SELECT count(*) FROM billing_allowance_eligibility_evidence WHERE account_id=$1),
 (SELECT count(*) FROM billing_journal WHERE account_id=$1),
 (SELECT count(*) FROM billing_lots WHERE account_id=$1),
 (SELECT count(*) FROM billing_allowance_issuances WHERE account_id=$1),
 (SELECT count(*) FROM billing_allowance_high_water WHERE account_id=$1)`, account).Scan(&evidenceRows, &journalRows, &lotRows, &issuanceRows, &highWaterRows); err != nil {
		t.Fatal(err)
	}
	if evidenceRows != 0 || journalRows != 0 || lotRows != 0 || issuanceRows != 0 || highWaterRows != 0 {
		t.Fatalf("rolled-back issuance effects remain: evidence=%d journal=%d lots=%d issuances=%d high_water=%d", evidenceRows, journalRows, lotRows, issuanceRows, highWaterRows)
	}

	root := credit.NewAllowances(store.Allowances(), nil)
	result, err := issueDueForTest(ctx, root, request)
	if err != nil || len(result.Issuances) != 1 || result.Issuances[0].Amount != 10 {
		t.Fatalf("retry issuance = %#v, err=%v", result, err)
	}
	replay, err := issueDueForTest(ctx, root, allowanceIssueRequest{Account: account, Now: end, Limit: 10})
	if err != nil || len(replay.Issuances) != 0 {
		t.Fatalf("replay issuance = %#v, err=%v", replay, err)
	}
	if err := db.QueryRowContext(ctx, `SELECT
 (SELECT count(*) FROM billing_allowance_eligibility_events WHERE account_id=$1)+
 (SELECT count(*) FROM billing_allowance_eligibility_evidence WHERE account_id=$1),
 (SELECT count(*) FROM billing_journal WHERE account_id=$1),
 (SELECT count(*) FROM billing_lots WHERE account_id=$1),
 (SELECT count(*) FROM billing_allowance_issuances WHERE account_id=$1),
 (SELECT count(*) FROM billing_allowance_high_water WHERE account_id=$1)`, account).Scan(&evidenceRows, &journalRows, &lotRows, &issuanceRows, &highWaterRows); err != nil {
		t.Fatal(err)
	}
	if evidenceRows != 2 || journalRows != 2 || lotRows != 1 || issuanceRows != 1 || highWaterRows != 1 {
		t.Fatalf("committed issuance effects = evidence=%d journal=%d lots=%d issuances=%d high_water=%d", evidenceRows, journalRows, lotRows, issuanceRows, highWaterRows)
	}
}

func TestPostgresAllowanceCheckpointPersistsProgressAndReplay(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("allowance-checkpoint")
	if err := store.CreateAccount(ctx, account, "allowance-checkpoint-subject"); err != nil {
		t.Fatal(err)
	}
	plan := allowancePlan()
	if err := store.PublishPlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, time.April, 1, 0, 0, 0, 0, time.UTC)
	ref := billing.Reference{Scope: billing.Scope{Provider: "sim", Merchant: "allowance-checkpoint-merchant", Environment: "sandbox"}, ID: "allowance-checkpoint-subscription"}
	assignment := catalog.PlanAssignment{ID: "allowance-checkpoint-assignment", PlanVersionID: plan.ID, Quantity: 1, Effective: billing.Period{Start: start, End: end}, Source: catalog.SourceSubscription}
	if _, err := subscription.New(store.Subscriptions()).Observe(ctx, subscription.Observation{Snapshot: subscription.Snapshot{Account: account, Ref: ref, Status: "active", Assignments: []catalog.PlanAssignment{assignment}}, EventID: "allowance-checkpoint-event", OccurredAt: start}); err != nil {
		t.Fatal(err)
	}
	schedule := credit.Schedule{Account: account, ID: "allowance-checkpoint-schedule", Subscription: ref, Assignment: assignment, Anchor: start, State: credit.ScheduleActive, StateEffectiveAt: start, SourceID: "allowance-checkpoint-event"}
	if _, err := credit.NewAllowances(store.Allowances(), nil).PutSchedule(ctx, schedule, 0); err != nil {
		t.Fatal(err)
	}
	evidence := acceptancePaid(account, schedule.ID, "allowance-checkpoint-payment", start, start, start, end)
	service := credit.NewAllowances(store.Allowances(), nil)
	first, err := service.AdvanceCheckpoint(ctx, credit.CheckpointRequest{Account: account, ID: "worker-a", Now: end, Limit: 10, MaxPeriods: 1, Evidence: []credit.EligibilityObservation{evidence}})
	if err != nil || len(first.Issuances) != 1 || first.Checkpoint.Revision != 1 || !first.HasMore || !first.Checkpoint.PassActive {
		t.Fatalf("first checkpoint=%+v error=%v", first, err)
	}
	second, err := service.AdvanceCheckpoint(ctx, credit.CheckpointRequest{Account: account, ID: "worker-a", Now: end, Limit: 10, MaxPeriods: 1})
	if err != nil || len(second.Issuances) != 1 || second.Checkpoint.Revision == first.Checkpoint.Revision || !second.Checkpoint.PassActive {
		t.Fatalf("replay checkpoint=%+v error=%v", second, err)
	}
	if _, err := service.AdvanceCheckpoint(ctx, credit.CheckpointRequest{Account: account, ID: "worker-a", Revision: first.Checkpoint.Revision, Now: end, Limit: 10}); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("stale checkpoint revision error=%v, want conflict", err)
	}
	var revisions int
	if err := db.QueryRowContext(ctx, `SELECT revision FROM billing_allowance_checkpoints WHERE account_id=$1 AND checkpoint_id=$2`, account, "worker-a").Scan(&revisions); err != nil || int64(revisions) != second.Checkpoint.Revision {
		t.Fatalf("stored checkpoint revision=%d error=%v", revisions, err)
	}
	for i := 0; i < 300; i++ {
		late := evidence
		late.SourceID = fmt.Sprintf("checkpoint-journal-%03d", i)
		late.ObservedAt = start.Add(time.Duration(i+1) * time.Microsecond)
		if err := service.RecordEligibility(ctx, late); err != nil {
			t.Fatal(err)
		}
	}
	sameEffective := evidence
	sameEffective.SourceID = "checkpoint-same-effective-late"
	sameEffective.ObservedAt = end.Add(time.Hour)
	if err := service.RecordEligibility(ctx, sameEffective); err != nil {
		t.Fatal(err)
	}
	// Insert an ID that sorts before the active cursor while the pass is live.
	earlier := schedule
	earlier.ID = "allowance-checkpoint-earlier"
	earlier.SourceID = "allowance-checkpoint-earlier-event"
	earlier.Subscription.ID = "allowance-checkpoint-earlier-subscription"
	earlier.Subscription.Scope.Merchant = "allowance-checkpoint-earlier-merchant"
	earlier.Assignment.ID = "allowance-checkpoint-earlier-assignment"
	if _, err := subscription.New(store.Subscriptions()).Observe(ctx, subscription.Observation{Snapshot: subscription.Snapshot{Account: account, Ref: earlier.Subscription, Status: "active", Assignments: []catalog.PlanAssignment{earlier.Assignment}}, EventID: earlier.SourceID, OccurredAt: start}); err != nil {
		t.Fatal(err)
	}
	if _, err := credit.NewAllowances(store.Allowances(), nil).PutSchedule(ctx, earlier, 0); err != nil {
		t.Fatal(err)
	}
	third, err := service.AdvanceCheckpoint(ctx, credit.CheckpointRequest{Account: account, ID: "worker-a", Now: end, Limit: 10, MaxPeriods: 1})
	if err != nil {
		t.Fatalf("bounded dirty drain result=%+v error=%v", third, err)
	}
	var processed, maximum int64
	if err := db.QueryRowContext(ctx, `SELECT processed_change_id,(SELECT max(change_id) FROM billing_allowance_change_journal WHERE account_id=$1) FROM billing_allowance_checkpoints WHERE account_id=$1 AND checkpoint_id=$2`, account, "worker-a").Scan(&processed, &maximum); err != nil {
		t.Fatal(err)
	}
	if processed >= maximum {
		t.Fatalf("journal drain consumed %d of %d in one bounded page", processed, maximum)
	}
	if _, err := service.AdvanceCheckpoint(ctx, credit.CheckpointRequest{Account: account, ID: "worker-a", Now: end, Limit: 10}); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("checkpoint callback failed")
	if err := store.Atomic(ctx, account, func(scope integration.Session) error {
		if _, err := credit.NewAllowances(scope.Allowances(), nil).AdvanceCheckpoint(ctx, credit.CheckpointRequest{Account: account, ID: "worker-rollback", Now: end, Limit: 10}); err != nil {
			return err
		}
		return sentinel
	}); !errors.Is(err, sentinel) {
		t.Fatalf("checkpoint rollback error=%v, want sentinel", err)
	}
	var rollbackRows int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM billing_allowance_checkpoints WHERE account_id=$1 AND checkpoint_id=$2`, account, "worker-rollback").Scan(&rollbackRows); err != nil || rollbackRows != 0 {
		t.Fatalf("rolled-back checkpoint rows=%d error=%v", rollbackRows, err)
	}
}
