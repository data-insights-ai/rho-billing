package pg

import (
	"errors"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/catalog"
	"github.com/data-insights-ai/rho-billing/integration"
)

func assignmentPlan(id string) catalog.PlanVersion {
	return catalog.PlanVersion{
		ID: id, PlanID: id + "-family", Version: 1,
		Entitlements: []catalog.EntitlementDefinition{{
			Key: "feature", Kind: catalog.EntitlementFeature,
			Aggregation: catalog.AggregationOR, Enabled: true,
		}},
	}
}

func assignmentInput(account billing.AccountID, id, planID string, source catalog.AssignmentSource) catalog.Assignment {
	return catalog.Assignment{
		Account: account,
		Plan: catalog.PlanAssignment{
			ID: id, PlanVersionID: planID, Quantity: 1, Source: source,
			Effective: billing.Period{Start: testTime()}, Perpetual: true,
		},
		SourceRef: id + "-source", Actor: "operator", Reason: "independent assignment",
	}
}

func TestPostgresIndependentAssignmentSourcesResolveWithoutPayment(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("assignment-sources")
	if err := store.CreateAccount(ctx, account, "assignment-sources"); err != nil {
		t.Fatal(err)
	}
	plan := assignmentPlan("assignment-plan")
	if err := store.PublishPlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	service := catalog.NewEntitlement(store.Entitlements(), testTime)
	for _, source := range []catalog.AssignmentSource{catalog.SourceFree, catalog.SourceManual, catalog.SourcePurchase} {
		in := assignmentInput(account, "assignment-"+sourceName(source), plan.ID, source)
		got, err := service.Assign(ctx, in)
		if err != nil {
			t.Fatalf("assign source %d: %v", source, err)
		}
		if got.CreatedAt.IsZero() || !got.Plan.Perpetual {
			t.Fatalf("assignment source %d missing durable receipt: %+v", source, got)
		}
		stored, err := service.Assignment(ctx, account, in.Plan.ID)
		if err != nil {
			t.Fatalf("read source %d: %v", source, err)
		}
		versions := []catalog.PlanVersion{plan}
		resolved, err := catalog.ResolveEntitlements(catalog.EntitlementInput{At: testTime().Add(100 * 365 * 24 * time.Hour), Assignments: []catalog.PlanAssignment{stored.Plan}, Versions: versions})
		if err != nil {
			t.Fatalf("resolve source %d: %v", source, err)
		}
		if _, ok := resolved.Get("feature"); !ok {
			t.Fatalf("source %d did not resolve feature: %+v", source, resolved)
		}
	}
}

func TestPostgresIndependentAssignmentReplayConflictAndScope(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	plan := assignmentPlan("assignment-replay-plan")
	if err := store.PublishPlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	for _, account := range []billing.AccountID{"assignment-a", "assignment-b"} {
		if err := store.CreateAccount(ctx, account, string(account)); err != nil {
			t.Fatal(err)
		}
	}
	service := catalog.NewEntitlement(store.Entitlements(), testTime)
	in := assignmentInput("assignment-a", "same-assignment", plan.ID, catalog.SourceManual)
	first, err := service.Assign(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := service.Assign(ctx, in)
	if err != nil || !replay.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("replay=%+v err=%v first=%+v", replay, err, first)
	}
	changed := in
	changed.Reason = "changed reason"
	if _, err := service.Assign(ctx, changed); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("changed assignment error=%v", err)
	}
	if _, err := service.Assignment(ctx, "assignment-b", in.Plan.ID); !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("cross-account assignment error=%v", err)
	}
}

func TestPostgresIndependentAssignmentUnknownPlanAndHostRollback(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("assignment-rollback")
	if err := store.CreateAccount(ctx, account, string(account)); err != nil {
		t.Fatal(err)
	}
	service := catalog.NewEntitlement(store.Entitlements(), testTime)
	unknown := assignmentInput(account, "unknown-plan-assignment", "missing-plan", catalog.SourceFree)
	if _, err := service.Assign(ctx, unknown); !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("unknown plan error=%v", err)
	}
	plan := assignmentPlan("rollback-plan")
	if err := store.PublishPlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	in := assignmentInput(account, "rollback-assignment", plan.ID, catalog.SourceManual)
	sentinel := errors.New("host rollback")
	err := store.Atomic(ctx, account, func(v integration.Session) error {
		if _, err := catalog.NewEntitlement(v.(*session).Entitlements(), testTime).Assign(ctx, in); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("host rollback error=%v", err)
	}
	if _, err := service.Assignment(ctx, account, in.Plan.ID); !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("rolled-back assignment read=%v", err)
	}
}

func TestPostgresIndependentAssignmentFailedCommitReturnsZero(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("assignment-commit-failure")
	if err := store.CreateAccount(ctx, account, string(account)); err != nil {
		t.Fatal(err)
	}
	plan := assignmentPlan("commit-failure-plan")
	if err := store.PublishPlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE FUNCTION fail_assignment_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'assignment commit failure'; END $$; CREATE CONSTRAINT TRIGGER fail_assignment_commit AFTER INSERT ON billing_entitlement_assignments DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION fail_assignment_commit()`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = db.ExecContext(ctx, `DROP TRIGGER IF EXISTS fail_assignment_commit ON billing_entitlement_assignments`)
		_, _ = db.ExecContext(ctx, `DROP FUNCTION IF EXISTS fail_assignment_commit()`)
	}()
	in := assignmentInput(account, "commit-failure-assignment", plan.ID, catalog.SourceFree)
	got, err := catalog.NewEntitlement(store.Entitlements(), testTime).Assign(ctx, in)
	if err == nil || got != (catalog.Assignment{}) {
		t.Fatalf("failed commit assignment=%+v err=%v", got, err)
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM billing_entitlement_assignments WHERE account_id=$1`, account).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed commit persisted count=%d err=%v", count, err)
	}
}

func TestPostgresIndependentAssignmentRejectsRelationalPlanTampering(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("assignment-relational-tamper")
	if err := store.CreateAccount(ctx, account, string(account)); err != nil {
		t.Fatal(err)
	}
	first := assignmentPlan("assignment-relational-first")
	second := assignmentPlan("assignment-relational-second")
	if err := store.PublishPlan(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := store.PublishPlan(ctx, second); err != nil {
		t.Fatal(err)
	}
	service := catalog.NewEntitlement(store.Entitlements(), testTime)
	in := assignmentInput(account, "relational-tamper", first.ID, catalog.SourceManual)
	if _, err := service.Assign(ctx, in); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE billing_entitlement_assignments SET plan_version_id=$3 WHERE account_id=$1 AND assignment_id=$2`, account, in.Plan.ID, second.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Assignment(ctx, account, in.Plan.ID); err == nil {
		t.Fatal("relational plan mismatch was accepted")
	}
}

func sourceName(source catalog.AssignmentSource) string {
	switch source {
	case catalog.SourceFree:
		return "free"
	case catalog.SourceManual:
		return "manual"
	case catalog.SourcePurchase:
		return "purchase"
	default:
		return "unknown"
	}
}
