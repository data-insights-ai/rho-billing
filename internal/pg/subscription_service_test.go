package pg

import (
	"errors"
	"reflect"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/catalog"
	"github.com/data-insights-ai/rho-billing/integration"
	"github.com/data-insights-ai/rho-billing/subscription"
)

func TestSubscriptionServiceBoundRollbackAndReplay(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	if err := store.CreateAccount(ctx, "subscription-service", "subscription-service-subject"); err != nil {
		t.Fatal(err)
	}
	plan := testPlan("subscription-service-plan", 1, 10, 100)
	if err := store.PublishPlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	start := testTime()
	ref := billing.Reference{Scope: billing.Scope{Provider: "test", Merchant: "subscription-service", Environment: "sandbox"}, ID: "subscription"}
	in := subscription.Observation{
		Snapshot: subscription.Snapshot{
			Account: "subscription-service", Ref: ref, Status: "active",
			Assignments: []catalog.PlanAssignment{{ID: "assignment", PlanVersionID: plan.ID, Quantity: 1, Source: catalog.SourceSubscription, Effective: billing.Period{Start: start, End: start.Add(time.Hour)}}},
		},
		EventID: "subscription-service-event", OccurredAt: start,
	}
	sentinel := errors.New("rollback subscription")
	err := store.Atomic(ctx, in.Snapshot.Account, func(scope integration.Session) error {
		if _, err := subscription.New(scope.Subscriptions()).Observe(ctx, in); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("bound observation error=%v, want sentinel", err)
	}
	if _, err := subscription.New(store.Subscriptions()).Subscription(ctx, in.Snapshot.Account, ref); !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("rollback retained subscription: %v", err)
	}
	var history, events int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM billing_subscription_history WHERE account_id=$1`, string(in.Snapshot.Account)).Scan(&history); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM billing_subscription_events WHERE account_id=$1`, string(in.Snapshot.Account)).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if history != 0 || events != 0 {
		t.Fatalf("rollback retained history=%d events=%d", history, events)
	}

	created, err := subscription.New(store.Subscriptions()).Observe(ctx, in)
	if err != nil || created.Revision != 1 {
		t.Fatalf("root observation=%+v err=%v", created, err)
	}
	replayed, err := subscription.New(store.Subscriptions()).Observe(ctx, in)
	if err != nil || !reflect.DeepEqual(replayed, created) {
		t.Fatalf("replay=%+v err=%v, want %+v", replayed, err, created)
	}
}
