package pg

import (
	"sync"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/catalog"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/subscription"
)

func TestConcurrentAllowanceIssuersCommitOnePeriod(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	if err := store.CreateAccount(ctx, "allowance-race", "allowance-race-subject"); err != nil {
		t.Fatal(err)
	}
	plan := catalog.PlanVersion{ID: "allowance-race-plan", PlanID: "allowance-race", Version: 1, Allowances: []catalog.AllowanceDefinition{{ID: "monthly", Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 10, Recurrence: catalog.AllowanceMonthly, Scope: catalog.AllowanceAccount, SpendScope: "AI_STANDARD"}}}
	if err := store.PublishPlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	ref := billing.Reference{Scope: billing.Scope{Provider: "sim", Merchant: "allowance-race-merchant", Environment: "sandbox"}, ID: "allowance-race-subscription"}
	assignment := catalog.PlanAssignment{ID: "allowance-race-assignment", PlanVersionID: plan.ID, Quantity: 1, Effective: billing.Period{Start: start, End: end}, Source: catalog.SourceSubscription}
	if _, err := subscription.New(store.Subscriptions()).Observe(ctx, subscription.Observation{Snapshot: subscription.Snapshot{Account: "allowance-race", Ref: ref, Status: "active", Assignments: []catalog.PlanAssignment{assignment}}, EventID: "allowance-race-subscription", OccurredAt: start}); err != nil {
		t.Fatal(err)
	}
	schedule := credit.Schedule{Account: "allowance-race", ID: "allowance-race-schedule", Subscription: ref, Assignment: assignment, Anchor: start, State: credit.ScheduleActive, StateEffectiveAt: start, SourceID: "allowance-race-source"}
	if _, err := credit.NewAllowances(store.Allowances(), nil).PutSchedule(ctx, schedule, 0); err != nil {
		t.Fatal(err)
	}
	evidence := credit.EligibilityObservation{Account: "allowance-race", ScheduleID: schedule.ID, SourceID: "allowance-race-payment", EffectiveAt: start, ObservedAt: start, Eligibility: credit.Eligibility{Status: credit.EligibilityPaid, Evidence: []credit.EligibilityEvidence{{Kind: credit.EvidencePayment, Reference: "allowance-race-invoice", Covered: billing.Period{Start: start, End: end}}}}}
	second := New(secondQueueDB(t, db))
	request := allowanceIssueRequest{Account: "allowance-race", Now: start, Limit: 10, MaxIssuances: 1, Evidence: []credit.EligibilityObservation{evidence}}
	startGate := make(chan struct{})
	results := make(chan allowanceIssueResult, 2)
	errorsOut := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Go(func() {
		<-startGate
		result, err := issueDueForTest(ctx, credit.NewAllowances(store.Allowances(), nil), request)
		results <- result
		errorsOut <- err
	})
	wg.Go(func() {
		<-startGate
		result, err := issueDueForTest(ctx, credit.NewAllowances(second.Allowances(), nil), request)
		results <- result
		errorsOut <- err
	})
	close(startGate)
	wg.Wait()
	close(results)
	close(errorsOut)
	var issued int
	for err := range errorsOut {
		if err != nil {
			t.Fatal(err)
		}
	}
	for result := range results {
		issued += len(result.Issuances)
	}
	if issued != 1 {
		t.Fatalf("concurrent issuers committed %d grants", issued)
	}
	balance, err := credit.New(store, func() time.Time { return start }).Balance(ctx, "allowance-race", "credits", "AI_STANDARD")
	if err != nil || balance.Available != 10 {
		t.Fatalf("concurrent allowance balance=%+v err=%v", balance, err)
	}
	if _, err := credit.New(store, func() time.Time { return start }).VerifyLedger(ctx, "allowance-race"); err != nil {
		t.Fatal(err)
	}
}
