package subscription

import (
	"errors"
	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/catalog"
	"reflect"
	"testing"
	"time"
)

func TestObservationOrderAndReconcileCAS(t *testing.T) {
	at := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)
	in := Observation{Snapshot: Snapshot{Account: "acme", Ref: billing.Reference{Scope: billing.Scope{Provider: "sim", Merchant: "merchant", Environment: "sandbox"}, ID: "sub"}, Status: "active"}, EventID: "evt1", OccurredAt: at}
	s, err := apply(nil, in)
	if err != nil {
		t.Fatal(err)
	}
	in.EventID = "evt2"
	in.Snapshot.Status = "paused"
	if _, err = apply(&s, in); !errors.Is(err, ErrReconcile) {
		t.Fatal(err)
	}
	in.OccurredAt = at.Add(-time.Second)
	got, err := apply(&s, in)
	if err != nil || got.Status != "active" {
		t.Fatal(got, err)
	}
	in.Reconcile = true
	in.ExpectedRevision = 1
	in.OccurredAt = at.Add(time.Second)
	got, err = apply(&s, in)
	if err != nil || got.Status != "paused" || got.Revision != 2 {
		t.Fatal(got, err)
	}
	if _, err = apply(&got, Observation{Snapshot: s, EventID: "reconcile-raced", OccurredAt: at.Add(time.Minute), Reconcile: true, ExpectedRevision: 1}); !errors.Is(err, billing.ErrConflict) {
		t.Fatal(err)
	}
	if _, err = apply(&s, Observation{Snapshot: s, EventID: "evt1", OccurredAt: at, Reconcile: true, ExpectedRevision: 1}); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("same event with changed reconciliation command should conflict: %v", err)
	}
}

func TestValidateAssignmentIdentityAndSource(t *testing.T) {
	at := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)
	base := Snapshot{Account: "acme", Ref: billing.Reference{Scope: billing.Scope{Provider: "sim", Merchant: "merchant", Environment: "sandbox"}, ID: "sub"}, Status: "active"}
	invalidSource := base
	invalidSource.Assignments = []catalog.PlanAssignment{{ID: "assignment", PlanVersionID: "v1", Quantity: 1, Effective: billing.Period{Start: at, End: at.Add(time.Hour)}}}
	if err := Validate(invalidSource); !errors.Is(err, billing.ErrInvalid) {
		t.Fatalf("unspecified assignment source should be rejected: %v", err)
	}
	first := catalog.PlanAssignment{ID: "assignment-a", PlanVersionID: "v1", Quantity: 1, Source: catalog.SourceSubscription, Effective: billing.Period{Start: at, End: at.Add(time.Hour)}}
	second := first
	second.ID = "assignment-b"
	second.Effective = billing.Period{Start: at.Add(30 * time.Minute), End: at.Add(90 * time.Minute)}
	validOverlap := base
	validOverlap.Assignments = []catalog.PlanAssignment{first, second}
	if err := Validate(validOverlap); err != nil {
		t.Fatalf("different assignment IDs may overlap: %v", err)
	}
	duplicate := base
	duplicate.Assignments = []catalog.PlanAssignment{first, first}
	if err := Validate(duplicate); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("overlapping duplicate assignment should conflict: %v", err)
	}
	adjacent := base
	second.ID = first.ID
	second.Effective = billing.Period{Start: first.Effective.End, End: at.Add(2 * time.Hour)}
	adjacent.Assignments = []catalog.PlanAssignment{first, second}
	if err := Validate(adjacent); err != nil {
		t.Fatalf("adjacent historical intervals may reuse an assignment ID: %v", err)
	}
}

func TestProviderScopeAndAccountIsolation(t *testing.T) {
	service := New(NewMemoryRepository())
	ctx := t.Context()
	ref := billing.Reference{Scope: billing.Scope{Provider: "sim", Merchant: "merchant", Environment: "sandbox"}, ID: "sub"}
	in := Observation{Snapshot: Snapshot{Account: "a", Ref: ref, Status: "active"}, EventID: "e", OccurredAt: time.Now()}
	if _, err := service.Observe(ctx, in); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Subscription(ctx, "b", ref); !errors.Is(err, billing.ErrNotFound) {
		t.Fatal(err)
	}
	ref.Scope.Environment = "production"
	if _, err := service.Subscription(ctx, "a", ref); !errors.Is(err, billing.ErrNotFound) {
		t.Fatal(err)
	}
}

func TestMemoryHistoricalEventsOwnershipAndResolvers(t *testing.T) {
	ctx := t.Context()
	at := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)
	accounts := catalog.NewMemoryAccountRepository()
	for _, id := range []billing.AccountID{"a", "b"} {
		if err := accounts.CreateAccount(ctx, id, "subject-"+string(id)); err != nil {
			t.Fatal(err)
		}
	}
	plans := catalog.NewRegistry()
	if err := plans.PublishPlan(ctx, catalog.PlanVersion{ID: "v1", PlanID: "plan", Version: 1}); err != nil {
		t.Fatal(err)
	}
	service := New(NewMemoryRepository(MemoryConfig{Catalog: plans, Accounts: accounts}))
	ref := billing.Reference{Scope: billing.Scope{Provider: "sim", Merchant: "merchant", Environment: "sandbox"}, ID: "sub"}
	first := Observation{Snapshot: Snapshot{Account: "a", Ref: ref, Status: "active", Assignments: []catalog.PlanAssignment{{ID: "assignment", PlanVersionID: "v1", Quantity: 1, Source: catalog.SourceSubscription, Effective: billing.Period{Start: at, End: at.Add(time.Hour)}}}}, EventID: "event-old", OccurredAt: at}
	original, err := service.Observe(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	later := first
	later.EventID = "event-new"
	later.OccurredAt = at.Add(time.Minute)
	later.Snapshot.Status = "paused"
	if _, err := service.Observe(ctx, later); err != nil {
		t.Fatal(err)
	}
	replayed, err := service.Observe(ctx, first)
	if err != nil || !reflect.DeepEqual(replayed, original) {
		t.Fatalf("historical replay = %#v, err %v; want original %#v", replayed, err, original)
	}
	changed := first
	changed.Snapshot.Status = "cancelled"
	if _, err := service.Observe(ctx, changed); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("changed historical replay should conflict: %v", err)
	}
	other := first
	other.Snapshot.Account = "b"
	other.EventID = "event-other"
	if _, err := service.Observe(ctx, other); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("provider reference ownership should conflict: %v", err)
	}
	unknownPlan := first
	unknownPlan.EventID = "event-unknown-plan"
	unknownPlan.Snapshot.Assignments = append([]catalog.PlanAssignment(nil), first.Snapshot.Assignments...)
	unknownPlan.Snapshot.Assignments[0].PlanVersionID = "v2"
	if got, err := service.Observe(ctx, unknownPlan); err != nil || got.Status != "paused" || got.Revision != 2 {
		t.Fatalf("stale unknown plan should remain a no-op: got=%+v err=%v", got, err)
	}
	unknownAccount := first
	unknownAccount.EventID = "event-unknown-account"
	unknownAccount.Snapshot.Account = "missing"
	if _, err := service.Observe(ctx, unknownAccount); !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("unknown account should be rejected: %v", err)
	}
}
