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

func TestPostgresFreeTrialAndInvoiceIsNotPaid(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	if err := store.CreateAccount(ctx, "sub-life-free", "sub-life-free"); err != nil {
		t.Fatal(err)
	}
	svc := subscription.New(store.Subscriptions())
	start := testTime()
	end := start.AddDate(0, 1, 0)
	free, err := svc.Activate(ctx, subscription.ActivateInput{
		Account: "sub-life-free", ID: "life-free", Operation: "op-free", Quantity: 1, Trial: true,
		Items:    []subscription.Item{{ID: "seat", PlanVersion: "subscription-service-plan", Quantity: 1, Period: billing.Period{Start: start, End: end}}},
		Policies: subscription.Policies{Access: subscription.AccessImmediate, Collection: subscription.CollectionNone, Proration: subscription.ProrationNone, Allowance: subscription.AllowanceKeepPeriod},
		Coverage: billing.Period{Start: start, End: end}, At: start,
	})
	if err != nil {
		t.Fatal(err)
	}
	if free.Access != subscription.AccessTrial || free.Collection != subscription.CollectionIdle {
		t.Fatalf("free=%+v", free)
	}
	if err := store.CreateAccount(ctx, "sub-life-paid", "sub-life-paid"); err != nil {
		t.Fatal(err)
	}
	paid := subscription.New(store.Subscriptions())
	life, err := paid.Activate(ctx, subscription.ActivateInput{
		Account: "sub-life-paid", ID: "life-paid", Operation: "op-paid", Quantity: 1,
		Items:    []subscription.Item{{ID: "seat", PlanVersion: "subscription-service-plan", Quantity: 1, Period: billing.Period{Start: start, End: end}}},
		Policies: subscription.Policies{Access: subscription.AccessPaid, Collection: subscription.CollectionManual, Proration: subscription.ProrationNone, Allowance: subscription.AllowanceKeepPeriod},
		Coverage: billing.Period{Start: start, End: end}, At: start,
	})
	if err != nil || life.Access != subscription.AccessNone {
		t.Fatalf("paid activate=%+v err=%v", life, err)
	}
	invoiced, err := paid.RecordCollection(ctx, subscription.CollectionInput{Account: "sub-life-paid", ID: "life-paid", Operation: "op-invoice", State: subscription.CollectionInvoiced, At: start.Add(time.Minute)})
	if err != nil || invoiced.Access != subscription.AccessNone || invoiced.Collection != subscription.CollectionInvoiced {
		t.Fatalf("invoice=%+v err=%v", invoiced, err)
	}
	got, err := paid.Lifecycle(ctx, "sub-life-paid", "life-paid")
	if err != nil || got.Collection != subscription.CollectionInvoiced {
		t.Fatalf("reload=%+v err=%v", got, err)
	}
}

func TestPostgresQuantityRevisionAndLateProviderConfirm(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	if err := store.CreateAccount(ctx, "sub-life-qty", "sub-life-qty"); err != nil {
		t.Fatal(err)
	}
	svc := subscription.New(store.Subscriptions())
	start := testTime()
	end := start.AddDate(0, 1, 0)
	ref := billing.Reference{Scope: billing.Scope{Provider: "test", Merchant: "subscription-service", Environment: "sandbox"}, ID: "sub-qty"}
	if _, err := svc.Activate(ctx, subscription.ActivateInput{
		Account: "sub-life-qty", ID: "life-qty", Operation: "op-qty-activate", Quantity: 2, Ref: ref,
		Items:    []subscription.Item{{ID: "seat", PlanVersion: "subscription-service-plan", Quantity: 2, Period: billing.Period{Start: start, End: end}}},
		Policies: subscription.Policies{Access: subscription.AccessImmediate, Collection: subscription.CollectionAutomatic, Proration: subscription.ProrationImmediate, Allowance: subscription.AllowanceKeepPeriod},
		Coverage: billing.Period{Start: start, End: end}, At: start,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetDesiredQuantity(ctx, subscription.QuantityInput{Account: "sub-life-qty", ID: "life-qty", Operation: "op-qty-2", Quantity: 5, Revision: 2, At: start.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetDesiredQuantity(ctx, subscription.QuantityInput{Account: "sub-life-qty", ID: "life-qty", Operation: "op-qty-old", Quantity: 3, Revision: 1, At: start.Add(2 * time.Minute)}); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("old revision=%v", err)
	}
	confirmed, err := svc.Confirm(ctx, "sub-life-qty", "life-qty", subscription.Snapshot{
		Account: "sub-life-qty", Ref: ref, Status: "active",
		Items:      []subscription.Item{{ID: "seat", PlanVersion: "subscription-service-plan", Quantity: 2, Period: billing.Period{Start: start, End: end}}},
		SourceTime: start.Add(3 * time.Minute),
	})
	if err != nil || confirmed.DesiredQuantity != 5 || confirmed.ConfirmedQuantity != 2 {
		t.Fatalf("confirm=%+v err=%v", confirmed, err)
	}
}

func TestPostgresPauseResumeAndChangeRollback(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	if err := store.CreateAccount(ctx, "sub-life-pause", "sub-life-pause"); err != nil {
		t.Fatal(err)
	}
	svc := subscription.New(store.Subscriptions())
	start := testTime()
	end := start.AddDate(1, 0, 0)
	if _, err := svc.Activate(ctx, subscription.ActivateInput{
		Account: "sub-life-pause", ID: "life-pause", Operation: "op-pause-activate", Quantity: 1,
		Items:    []subscription.Item{{ID: "seat", PlanVersion: "subscription-service-plan", Quantity: 1, Period: billing.Period{Start: start, End: end}}},
		Policies: subscription.Policies{Access: subscription.AccessImmediate, Collection: subscription.CollectionAutomatic, Proration: subscription.ProrationNextPeriod, Allowance: subscription.AllowanceKeepPeriod},
		Coverage: billing.Period{Start: start, End: end}, At: start,
	}); err != nil {
		t.Fatal(err)
	}
	// A pause scheduled for later records the schedule and leaves access alone
	// until it takes effect; only an immediate pause suspends access now.
	paused, err := svc.RequestChange(ctx, subscription.ChangeInput{Account: "sub-life-pause", ID: "life-pause", Operation: "op-pause", Kind: subscription.ChangePause, EffectiveAt: start.Add(24 * time.Hour), At: start.Add(time.Hour)})
	if err != nil || paused.Access != subscription.AccessActive || !paused.Change.EffectiveAt.Equal(start.Add(24*time.Hour)) {
		t.Fatalf("pause=%+v err=%v", paused, err)
	}
	replay, err := svc.RequestChange(ctx, subscription.ChangeInput{Account: "sub-life-pause", ID: "life-pause", Operation: "op-pause", Kind: subscription.ChangePause, EffectiveAt: start.Add(24 * time.Hour), At: start.Add(time.Hour)})
	if err != nil || replay.Access != paused.Access || replay.Revision != paused.Revision {
		t.Fatalf("pause replay=%+v err=%v", replay, err)
	}
	immediate, err := svc.RequestChange(ctx, subscription.ChangeInput{Account: "sub-life-pause", ID: "life-pause", Operation: "op-pause-now", Kind: subscription.ChangePause, EffectiveAt: start.Add(2 * time.Hour), At: start.Add(2 * time.Hour)})
	if err != nil || immediate.Access != subscription.AccessPaused {
		t.Fatalf("immediate pause=%+v err=%v", immediate, err)
	}
}

func TestPostgresAnnualCoverageIssuesMonthlyAllowancesOnce(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	if err := store.CreateAccount(ctx, "sub-life-annual", "sub-life-annual"); err != nil {
		t.Fatal(err)
	}
	svc := subscription.New(store.Subscriptions())
	start := time.Date(2028, 1, 31, 0, 0, 0, 0, time.UTC)
	end := time.Date(2029, 1, 31, 0, 0, 0, 0, time.UTC)
	life, err := svc.Activate(ctx, subscription.ActivateInput{
		Account: "sub-life-annual", ID: "life-annual", Operation: "op-annual", Quantity: 1,
		Items:    []subscription.Item{{ID: "seat", PlanVersion: "subscription-service-plan", Quantity: 1, Period: billing.Period{Start: start, End: end}}},
		Policies: subscription.Policies{Access: subscription.AccessImmediate, Collection: subscription.CollectionAutomatic, Proration: subscription.ProrationNone, Allowance: subscription.AllowanceKeepPeriod},
		Coverage: billing.Period{Start: start, End: end}, At: start,
	})
	if err != nil {
		t.Fatal(err)
	}
	def := catalog.AllowanceDefinition{ID: "monthly", Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 10, Recurrence: catalog.AllowanceMonthly, Scope: catalog.AllowanceAccount, SpendScope: "credits"}
	assignment := catalog.PlanAssignment{ID: life.ID, PlanVersionID: "subscription-service-plan", Quantity: life.DesiredQuantity, Effective: life.Coverage, Source: catalog.SourceSubscription}
	periods, err := credit.Periods(credit.PeriodInput{Definition: def, Assignment: assignment, Anchor: life.AllowanceAnchor, OriginalDay: life.AllowanceDay, Through: end})
	if err != nil || len(periods) != 12 {
		t.Fatalf("grants=%d err=%v periods=%v", len(periods), err, periods)
	}
	if !periods[0].Start.Equal(start) || !periods[1].Start.Equal(time.Date(2028, 2, 29, 0, 0, 0, 0, time.UTC)) || !periods[2].Start.Equal(time.Date(2028, 3, 31, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("leap/month-end=%v", periods[:3])
	}
	seen := map[time.Time]struct{}{}
	for _, period := range periods {
		if _, ok := seen[period.Start]; ok {
			t.Fatalf("duplicate %s", period.Start)
		}
		seen[period.Start] = struct{}{}
	}
}
