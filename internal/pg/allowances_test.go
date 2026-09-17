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

func allowancePlan() catalog.PlanVersion {
	return catalog.PlanVersion{
		ID: "allowance-plan-v1", PlanID: "allowance-plan", Version: 1,
		Allowances: []catalog.AllowanceDefinition{{
			ID: "monthly-credits", Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 10,
			Recurrence: catalog.AllowanceMonthly, Scope: catalog.AllowanceAccount, SpendScope: "AI_STANDARD",
		}},
	}
}

func TestEligibleForPeriodUsesNewestEffectiveEvidence(t *testing.T) {
	period := billing.Period{Start: time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)}
	paid := credit.Eligibility{Status: credit.EligibilityPaid, Evidence: []credit.EligibilityEvidence{{Kind: credit.EvidencePayment, Reference: "invoice", Covered: billing.Period{Start: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)}}}}
	events := []storedEligibility{
		{SourceID: "delinquent", EffectiveAt: time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC), Eligibility: credit.Eligibility{Status: credit.EligibilityDelinquent}},
		{SourceID: "paid", EffectiveAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Eligibility: paid},
	}
	if _, source, ok := eligibleForPeriod(events, period); ok || source != "delinquent" {
		t.Fatalf("delinquent evidence was bypassed: source=%q eligible=%v", source, ok)
	}
	period.Start = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	period.End = time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	if _, source, ok := eligibleForPeriod(events, period); !ok || source != "paid" {
		t.Fatalf("historical paid evidence was not selected: source=%q eligible=%v", source, ok)
	}
}

func TestPostgresAllowanceCatchupIsIdempotentAndAtomic(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	if err := store.CreateAccount(ctx, "allowance-acct", "allowance-subject"); err != nil {
		t.Fatal(err)
	}
	plan := allowancePlan()
	if err := store.PublishPlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC)
	ref := billing.Reference{Scope: billing.Scope{Provider: "sim", Merchant: "allowance-merchant", Environment: "sandbox"}, ID: "allowance-subscription"}
	assignment := catalog.PlanAssignment{ID: "assignment-a", PlanVersionID: plan.ID, Quantity: 1, Effective: billing.Period{Start: start, End: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)}, Source: catalog.SourceSubscription}
	if _, err := subscription.New(store.Subscriptions()).Observe(ctx, subscription.Observation{Snapshot: subscription.Snapshot{Account: "allowance-acct", Ref: ref, Status: "active", Assignments: []catalog.PlanAssignment{assignment}}, EventID: "subscription-event-a", OccurredAt: start}); err != nil {
		t.Fatal(err)
	}
	schedule := credit.Schedule{
		Account: "allowance-acct", ID: "schedule-a", Subscription: ref,
		Assignment: assignment,
		Anchor:     start, State: credit.ScheduleActive,
		StateEffectiveAt: start, SourceID: "subscription-event-a",
	}
	if _, err := credit.NewAllowances(store.Allowances(), nil).PutSchedule(ctx, schedule, 0); err != nil {
		t.Fatal(err)
	}
	coverage := billing.Period{Start: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)}
	evidence := credit.EligibilityObservation{
		Account: "allowance-acct", ScheduleID: schedule.ID, SourceID: "payment-a",
		EffectiveAt: coverage.Start, ObservedAt: time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC),
		Eligibility: credit.Eligibility{Status: credit.EligibilityPaid, Evidence: []credit.EligibilityEvidence{{Kind: credit.EvidencePayment, Reference: "invoice-a", Covered: coverage}}},
	}
	first, err := issueDueForTest(ctx, credit.NewAllowances(store.Allowances(), nil), allowanceIssueRequest{Account: schedule.Account, Now: time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC), Limit: 10, Evidence: []credit.EligibilityObservation{evidence}})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Issuances) != 2 || first.Issuances[0].Amount != 10 || first.Issuances[1].Amount != 10 {
		t.Fatalf("catch-up issuances = %#v", first.Issuances)
	}
	retry, err := issueDueForTest(ctx, credit.NewAllowances(store.Allowances(), nil), allowanceIssueRequest{Account: schedule.Account, Now: time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC), Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(retry.Issuances) != 0 {
		t.Fatalf("retry created issuances: %#v", retry.Issuances)
	}
	later, err := issueDueForTest(ctx, credit.NewAllowances(store.Allowances(), nil), allowanceIssueRequest{Account: schedule.Account, Now: time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC), Limit: 10})
	if err != nil || len(later.Issuances) != 1 || later.Issuances[0].Amount != 10 {
		t.Fatalf("later catch-up = %#v, err %v", later.Issuances, err)
	}
	engine := credit.New(store, func() time.Time { return time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC) })
	balance, err := engine.Balance(ctx, schedule.Account, "credits", "AI_STANDARD")
	if err != nil || balance.Available != 10 || balance.Expired != 20 {
		t.Fatalf("allowance balance = %#v, err %v", balance, err)
	}
	updated := schedule
	updated.State = credit.SchedulePaused
	updated.StateEffectiveAt = time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	updated.SourceID = "subscription-event-pause"
	updated, err = credit.NewAllowances(store.Allowances(), nil).PutSchedule(ctx, updated, 1)
	if err != nil || updated.Revision != 2 {
		t.Fatalf("pause CAS = %#v, err %v", updated, err)
	}
	if _, err := credit.NewAllowances(store.Allowances(), nil).PutSchedule(ctx, schedule, 1); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("stale schedule update = %v", err)
	}
}

func TestPostgresAllowanceRejectsPreviousScheduleFromAnotherProviderScope(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	if err := store.CreateAccount(ctx, "allowance-cross-provider", "allowance-cross-provider-subject"); err != nil {
		t.Fatal(err)
	}
	plan := allowancePlan()
	if err := store.PublishPlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	oldAssignment := catalog.PlanAssignment{ID: "cross-old-assignment", PlanVersionID: plan.ID, Quantity: 1, Effective: billing.Period{Start: start, End: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)}, Source: catalog.SourceSubscription}
	newAssignment := oldAssignment
	newAssignment.ID = "cross-new-assignment"
	newAssignment.Effective = billing.Period{Start: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)}
	refA := billing.Reference{Scope: billing.Scope{Provider: "sim-a", Merchant: "merchant", Environment: "sandbox"}, ID: "cross-sub-a"}
	refB := billing.Reference{Scope: billing.Scope{Provider: "sim-b", Merchant: "merchant", Environment: "sandbox"}, ID: "cross-sub-b"}
	for _, item := range []struct {
		ref        billing.Reference
		assignment catalog.PlanAssignment
		event      string
	}{
		{refA, oldAssignment, "cross-event-a"},
		{refB, newAssignment, "cross-event-b"},
	} {
		if _, err := subscription.New(store.Subscriptions()).Observe(ctx, subscription.Observation{Snapshot: subscription.Snapshot{Account: "allowance-cross-provider", Ref: item.ref, Status: "active", Assignments: []catalog.PlanAssignment{item.assignment}}, EventID: item.event, OccurredAt: item.assignment.Effective.Start}); err != nil {
			t.Fatal(err)
		}
	}
	oldSchedule := credit.Schedule{Account: "allowance-cross-provider", ID: "cross-schedule-a", Subscription: refA, Assignment: oldAssignment, Anchor: start, State: credit.ScheduleActive, StateEffectiveAt: start, SourceID: "cross-event-a"}
	if _, err := credit.NewAllowances(store.Allowances(), nil).PutSchedule(ctx, oldSchedule, 0); err != nil {
		t.Fatal(err)
	}
	newSchedule := credit.Schedule{Account: oldSchedule.Account, ID: "cross-schedule-b", Subscription: refB, Assignment: newAssignment, Anchor: start, State: credit.ScheduleActive, StateEffectiveAt: newAssignment.Effective.Start, PreviousScheduleID: oldSchedule.ID, SourceID: "cross-event-b"}
	if _, err := credit.NewAllowances(store.Allowances(), nil).PutSchedule(ctx, newSchedule, 0); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("cross-provider previous schedule error = %v, want conflict", err)
	}
}

func TestPostgresAllowancePeriodCursorBoundsLongCatchup(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	if err := store.CreateAccount(ctx, "allowance-period-cursor", "allowance-period-cursor-subject"); err != nil {
		t.Fatal(err)
	}
	plan := allowancePlan()
	plan.Allowances = append(plan.Allowances, catalog.AllowanceDefinition{ID: "monthly-secondary", Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 5, Recurrence: catalog.AllowanceMonthly, Scope: catalog.AllowanceAccount, SpendScope: "AI_STANDARD"})
	if err := store.PublishPlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC)
	ref := billing.Reference{Scope: billing.Scope{Provider: "sim", Merchant: "cursor-merchant", Environment: "sandbox"}, ID: "cursor-sub"}
	assignment := catalog.PlanAssignment{ID: "cursor-assignment", PlanVersionID: plan.ID, Quantity: 1, Effective: billing.Period{Start: start, End: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)}, Source: catalog.SourceSubscription}
	if _, err := subscription.New(store.Subscriptions()).Observe(ctx, subscription.Observation{Snapshot: subscription.Snapshot{Account: "allowance-period-cursor", Ref: ref, Status: "active", Assignments: []catalog.PlanAssignment{assignment}}, EventID: "cursor-event", OccurredAt: start}); err != nil {
		t.Fatal(err)
	}
	schedule := credit.Schedule{Account: "allowance-period-cursor", ID: "cursor-schedule", Subscription: ref, Assignment: assignment, Anchor: start, State: credit.ScheduleActive, StateEffectiveAt: start, SourceID: "cursor-event"}
	if _, err := credit.NewAllowances(store.Allowances(), nil).PutSchedule(ctx, schedule, 0); err != nil {
		t.Fatal(err)
	}
	evidence := credit.EligibilityObservation{Account: schedule.Account, ScheduleID: schedule.ID, SourceID: "cursor-paid", EffectiveAt: start, ObservedAt: time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC), Eligibility: credit.Eligibility{Status: credit.EligibilityPaid, Evidence: []credit.EligibilityEvidence{{Kind: credit.EvidencePayment, Reference: "cursor-invoice", Covered: billing.Period{Start: start, End: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)}}}}}
	first, err := issueDueForTest(ctx, credit.NewAllowances(store.Allowances(), nil), allowanceIssueRequest{Account: schedule.Account, Now: evidence.ObservedAt, Limit: 10, MaxPeriods: 1, Evidence: []credit.EligibilityObservation{evidence}})
	if err != nil || len(first.Issuances) != 1 || !first.HasMore || first.Next != schedule.ID || first.NextDefinition != "monthly-credits" || !first.NextPeriod.Equal(start) {
		t.Fatalf("first bounded catch-up = %#v, err %v", first, err)
	}
	second, err := issueDueForTest(ctx, credit.NewAllowances(store.Allowances(), nil), allowanceIssueRequest{Account: schedule.Account, Now: evidence.ObservedAt, After: first.Next, AfterPeriod: first.NextPeriod, AfterDefinition: first.NextDefinition, Limit: 10, MaxPeriods: 1})
	if err != nil || len(second.Issuances) != 1 || !second.HasMore || !second.NextPeriod.Equal(time.Date(2026, 2, 28, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("second bounded catch-up = %#v, err %v", second, err)
	}
	third, err := issueDueForTest(ctx, credit.NewAllowances(store.Allowances(), nil), allowanceIssueRequest{Account: schedule.Account, Now: evidence.ObservedAt, After: second.Next, AfterPeriod: second.NextPeriod, AfterDefinition: second.NextDefinition, Limit: 10, MaxPeriods: 1})
	if err != nil || len(third.Issuances) != 1 || third.Issuances[0].DefinitionID != "monthly-secondary" {
		t.Fatalf("definition cursor skipped a shared-start allowance: %#v, err %v", third, err)
	}
}

func TestPostgresAllowanceStateHistoryBeyondOneThousand(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	if err := store.CreateAccount(ctx, "allowance-history-bound", "allowance-history-bound-subject"); err != nil {
		t.Fatal(err)
	}
	plan := allowancePlan()
	if err := store.PublishPlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ref := billing.Reference{Scope: billing.Scope{Provider: "sim", Merchant: "history-merchant", Environment: "sandbox"}, ID: "history-sub"}
	assignment := catalog.PlanAssignment{ID: "history-assignment", PlanVersionID: plan.ID, Quantity: 1, Effective: billing.Period{Start: start, End: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)}, Source: catalog.SourceSubscription}
	if _, err := subscription.New(store.Subscriptions()).Observe(ctx, subscription.Observation{Snapshot: subscription.Snapshot{Account: "allowance-history-bound", Ref: ref, Status: "active", Assignments: []catalog.PlanAssignment{assignment}}, EventID: "history-event", OccurredAt: start}); err != nil {
		t.Fatal(err)
	}
	schedule := credit.Schedule{Account: "allowance-history-bound", ID: "history-schedule", Subscription: ref, Assignment: assignment, Anchor: start, State: credit.ScheduleActive, StateEffectiveAt: start, SourceID: "history-event"}
	if _, err := credit.NewAllowances(store.Allowances(), nil).PutSchedule(ctx, schedule, 0); err != nil {
		t.Fatal(err)
	}
	var snapshot []byte
	if err := store.db.QueryRowContext(ctx, `SELECT snapshot FROM billing_allowance_schedule_history WHERE account_id=$1 AND schedule_id=$2 AND revision=1`, schedule.Account, schedule.ID).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	for revision := int64(2); revision <= 1001; revision++ {
		if _, err := store.db.ExecContext(ctx, `INSERT INTO billing_allowance_schedule_history(account_id,schedule_id,revision,snapshot,source_id,recorded_at) VALUES($1,$2,$3,$4,$5,$6)`, schedule.Account, schedule.ID, revision, snapshot, "history-replay", start); err != nil {
			t.Fatal(err)
		}
	}
	out, err := issueDueForTest(ctx, credit.NewAllowances(store.Allowances(), nil), allowanceIssueRequest{Account: schedule.Account, Now: start, Limit: 10, Evidence: []credit.EligibilityObservation{acceptancePaid(schedule.Account, schedule.ID, "history-paid", start, start, start, assignment.Effective.End)}})
	if err != nil || len(out.Issuances) != 1 || out.Issuances[0].Amount != 10 {
		t.Fatalf("bounded schedule history issuance=%+v error=%v", out, err)
	}
}

func TestPostgresAllowanceUpgradeDoesNotSubtractAnUnissuedGrant(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	if err := store.CreateAccount(ctx, "upgrade-acct", "upgrade-subject"); err != nil {
		t.Fatal(err)
	}
	oldPlan := allowancePlan()
	newPlan := oldPlan
	newPlan.Allowances = append([]catalog.AllowanceDefinition(nil), oldPlan.Allowances...)
	newPlan.ID = "allowance-plan-v2"
	newPlan.Version = 2
	newPlan.Allowances[0].Amount = 20
	if err := store.PublishPlan(ctx, oldPlan); err != nil {
		t.Fatal(err)
	}
	if err := store.PublishPlan(ctx, newPlan); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC)
	ref := billing.Reference{Scope: billing.Scope{Provider: "sim", Merchant: "upgrade-merchant", Environment: "sandbox"}, ID: "upgrade-subscription"}
	oldAssignment := catalog.PlanAssignment{ID: "upgrade-assignment-old", PlanVersionID: oldPlan.ID, Quantity: 1, Effective: billing.Period{Start: start, End: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)}, Source: catalog.SourceSubscription}
	newAssignment := catalog.PlanAssignment{ID: "upgrade-assignment-new", PlanVersionID: newPlan.ID, Quantity: 1, Effective: billing.Period{Start: time.Date(2026, 2, 28, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)}, Source: catalog.SourceSubscription}
	if _, err := subscription.New(store.Subscriptions()).Observe(ctx, subscription.Observation{Snapshot: subscription.Snapshot{Account: "upgrade-acct", Ref: ref, Status: "active", Assignments: []catalog.PlanAssignment{oldAssignment}}, EventID: "upgrade-subscription-old", OccurredAt: start}); err != nil {
		t.Fatal(err)
	}
	oldSchedule := credit.Schedule{Account: "upgrade-acct", ID: "upgrade-old", Subscription: ref, Assignment: oldAssignment, Anchor: start, State: credit.ScheduleActive, StateEffectiveAt: start, SourceID: "upgrade-source-old"}
	if _, err := credit.NewAllowances(store.Allowances(), nil).PutSchedule(ctx, oldSchedule, 0); err != nil {
		t.Fatal(err)
	}
	paused := oldSchedule
	paused.State = credit.SchedulePaused
	paused.StateEffectiveAt = time.Date(2026, 2, 28, 0, 0, 0, 0, time.UTC)
	paused.SourceID = "upgrade-source-pause"
	if _, err := credit.NewAllowances(store.Allowances(), nil).PutSchedule(ctx, paused, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := subscription.New(store.Subscriptions()).Observe(ctx, subscription.Observation{Snapshot: subscription.Snapshot{Account: "upgrade-acct", Ref: ref, Status: "active", Assignments: []catalog.PlanAssignment{oldAssignment, newAssignment}}, EventID: "upgrade-subscription-new", OccurredAt: time.Date(2026, 2, 28, 0, 0, 0, 0, time.UTC)}); err != nil {
		t.Fatal(err)
	}
	newSchedule := credit.Schedule{Account: "upgrade-acct", ID: "upgrade-new", Subscription: oldSchedule.Subscription, Assignment: newAssignment, State: credit.ScheduleActive, StateEffectiveAt: time.Date(2026, 2, 28, 0, 0, 0, 0, time.UTC), PreviousScheduleID: oldSchedule.ID, SourceID: "upgrade-source-new"}
	if _, err := credit.NewAllowances(store.Allowances(), nil).PutSchedule(ctx, newSchedule, 0); err != nil {
		t.Fatal(err)
	}
	coverage := billing.Period{Start: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)}
	evidence := credit.EligibilityObservation{Account: "upgrade-acct", ScheduleID: newSchedule.ID, SourceID: "upgrade-payment", EffectiveAt: coverage.Start, ObservedAt: time.Date(2026, 2, 28, 0, 0, 0, 0, time.UTC), Eligibility: credit.Eligibility{Status: credit.EligibilityPaid, Evidence: []credit.EligibilityEvidence{{Kind: credit.EvidencePayment, Reference: "upgrade-invoice", Covered: coverage}}}}
	result, err := issueDueForTest(ctx, credit.NewAllowances(store.Allowances(), nil), allowanceIssueRequest{Account: "upgrade-acct", Now: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), Limit: 10, Evidence: []credit.EligibilityObservation{evidence}})
	if err != nil {
		t.Fatal(err)
	}
	var newGrant *credit.Issuance
	for i := range result.Issuances {
		if result.Issuances[i].ScheduleID == newSchedule.ID {
			newGrant = &result.Issuances[i]
		}
	}
	if newGrant == nil || newGrant.Amount != 20 {
		t.Fatalf("upgrade grant = %#v, all issuances = %#v", newGrant, result.Issuances)
	}
}

func TestPostgresMidCycleUpgradeUsesIssuedEntitlementAndKeepsOriginalPeriod(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	if err := store.CreateAccount(ctx, "midcycle-acct", "midcycle-subject"); err != nil {
		t.Fatal(err)
	}
	oldPlan := allowancePlan()
	newPlan := oldPlan
	newPlan.ID = "midcycle-plan-v2"
	newPlan.Version = 2
	newPlan.Allowances = append([]catalog.AllowanceDefinition(nil), oldPlan.Allowances...)
	newPlan.Allowances[0].Amount = 20
	downgradePlan := oldPlan
	downgradePlan.ID = "midcycle-plan-v3"
	downgradePlan.Version = 3
	if err := store.PublishPlan(ctx, oldPlan); err != nil {
		t.Fatal(err)
	}
	if err := store.PublishPlan(ctx, newPlan); err != nil {
		t.Fatal(err)
	}
	if err := store.PublishPlan(ctx, downgradePlan); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC)
	midCycle := time.Date(2026, 2, 10, 0, 0, 0, 0, time.UTC)
	boundary := time.Date(2026, 2, 28, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	ref := billing.Reference{Scope: billing.Scope{Provider: "sim", Merchant: "midcycle-merchant", Environment: "sandbox"}, ID: "midcycle-subscription"}
	oldAssignment := catalog.PlanAssignment{ID: "midcycle-assignment-old", PlanVersionID: oldPlan.ID, Quantity: 1, Effective: billing.Period{Start: start, End: end}, Source: catalog.SourceSubscription}
	newAssignment := catalog.PlanAssignment{ID: "midcycle-assignment-new", PlanVersionID: newPlan.ID, Quantity: 1, Effective: billing.Period{Start: midCycle, End: end}, Source: catalog.SourceSubscription}
	downgradeAssignment := catalog.PlanAssignment{ID: "midcycle-assignment-downgrade", PlanVersionID: downgradePlan.ID, Quantity: 1, Effective: billing.Period{Start: boundary, End: end}, Source: catalog.SourceSubscription}
	if _, err := subscription.New(store.Subscriptions()).Observe(ctx, subscription.Observation{Snapshot: subscription.Snapshot{Account: "midcycle-acct", Ref: ref, Status: "active", Assignments: []catalog.PlanAssignment{oldAssignment, newAssignment, downgradeAssignment}}, EventID: "midcycle-subscription", OccurredAt: start}); err != nil {
		t.Fatal(err)
	}
	oldSchedule := credit.Schedule{Account: "midcycle-acct", ID: "midcycle-old", Subscription: ref, Assignment: oldAssignment, Anchor: start, State: credit.ScheduleActive, StateEffectiveAt: start, SourceID: "midcycle-source-old"}
	if _, err := credit.NewAllowances(store.Allowances(), nil).PutSchedule(ctx, oldSchedule, 0); err != nil {
		t.Fatal(err)
	}
	coverage := billing.Period{Start: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)}
	oldEvidence := credit.EligibilityObservation{Account: "midcycle-acct", ScheduleID: oldSchedule.ID, SourceID: "midcycle-payment-old", EffectiveAt: coverage.Start, ObservedAt: start, Eligibility: credit.Eligibility{Status: credit.EligibilityPaid, Evidence: []credit.EligibilityEvidence{{Kind: credit.EvidencePayment, Reference: "midcycle-invoice", Covered: coverage}}}}
	initial, err := issueDueForTest(ctx, credit.NewAllowances(store.Allowances(), nil), allowanceIssueRequest{Account: "midcycle-acct", Now: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), Limit: 10, Evidence: []credit.EligibilityObservation{oldEvidence}})
	if err != nil || len(initial.Issuances) != 1 || initial.Issuances[0].Amount != 10 {
		t.Fatalf("initial allowance = %#v, err %v", initial.Issuances, err)
	}
	engine := credit.New(store, func() time.Time { return midCycle })
	if _, err := engine.Reserve(ctx, credit.ReserveInput{Account: "midcycle-acct", Operation: "midcycle-reserve", ReservationID: "midcycle-reservation", Actor: "midcycle-user", Unit: "credits", Scope: "AI_STANDARD", Amount: 7, Deadline: midCycle.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Settle(ctx, credit.SettleInput{Account: "midcycle-acct", Operation: "midcycle-settle", ReservationID: "midcycle-reservation", Actual: 7, Evidence: credit.Evidence{UsageID: "midcycle-usage", RatingVersion: "midcycle-rating", Metrics: []credit.Metric{{Name: "actions", Quantity: 7}}}}); err != nil {
		t.Fatal(err)
	}
	newSchedule := credit.Schedule{Account: "midcycle-acct", ID: "midcycle-new", Subscription: ref, Assignment: newAssignment, State: credit.ScheduleActive, StateEffectiveAt: midCycle, PreviousScheduleID: oldSchedule.ID, Adjustment: credit.AdjustmentDelta, SourceID: "midcycle-source-new"}
	if _, err := credit.NewAllowances(store.Allowances(), nil).PutSchedule(ctx, newSchedule, 0); err != nil {
		t.Fatal(err)
	}
	newEvidence := oldEvidence
	newEvidence.ScheduleID = newSchedule.ID
	newEvidence.SourceID = "midcycle-payment-new"
	result, err := issueDueForTest(ctx, credit.NewAllowances(store.Allowances(), nil), allowanceIssueRequest{Account: "midcycle-acct", Now: time.Date(2026, 2, 15, 0, 0, 0, 0, time.UTC), Limit: 10, Evidence: []credit.EligibilityObservation{newEvidence}})
	if err != nil || len(result.Issuances) != 1 || result.Issuances[0].Amount != 10 || !result.Issuances[0].Period.Start.Equal(start) || !result.Issuances[0].Period.End.Equal(boundary) {
		t.Fatalf("mid-cycle delta = %#v, err %v", result.Issuances, err)
	}
	balance, err := engine.Balance(ctx, "midcycle-acct", "credits", "AI_STANDARD")
	if err != nil || balance.Available != 13 {
		t.Fatalf("mid-cycle available balance = %#v, err %v", balance, err)
	}
	replay, err := issueDueForTest(ctx, credit.NewAllowances(store.Allowances(), nil), allowanceIssueRequest{Account: "midcycle-acct", Now: time.Date(2026, 2, 15, 0, 0, 0, 0, time.UTC), Limit: 10})
	if err != nil || len(replay.Issuances) != 0 {
		t.Fatalf("mid-cycle replay = %#v, err %v", replay.Issuances, err)
	}
	downgradeSchedule := credit.Schedule{Account: "midcycle-acct", ID: "midcycle-downgrade", Subscription: ref, Assignment: downgradeAssignment, State: credit.ScheduleActive, StateEffectiveAt: boundary, PreviousScheduleID: newSchedule.ID, SourceID: "midcycle-source-downgrade"}
	if _, err := credit.NewAllowances(store.Allowances(), nil).PutSchedule(ctx, downgradeSchedule, 0); err != nil {
		t.Fatal(err)
	}
	downgradeEvidence := newEvidence
	downgradeEvidence.ScheduleID = downgradeSchedule.ID
	downgradeEvidence.SourceID = "midcycle-payment-downgrade"
	downgrade, err := issueDueForTest(ctx, credit.NewAllowances(store.Allowances(), nil), allowanceIssueRequest{Account: "midcycle-acct", Now: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), Limit: 10, Evidence: []credit.EligibilityObservation{downgradeEvidence}})
	if err != nil {
		t.Fatal(err)
	}
	var foundDowngrade *credit.Issuance
	for i := range downgrade.Issuances {
		if downgrade.Issuances[i].ScheduleID == downgradeSchedule.ID {
			foundDowngrade = &downgrade.Issuances[i]
		}
	}
	if foundDowngrade == nil || foundDowngrade.Amount != 10 || !foundDowngrade.Period.Start.Equal(boundary) || !foundDowngrade.Period.End.Equal(time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("boundary downgrade = %#v, all issuances = %#v", foundDowngrade, downgrade.Issuances)
	}
}
