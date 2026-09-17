package pg

import (
	"errors"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/catalog"
	"github.com/data-insights-ai/rho-billing/integration"
)

func TestPostgresEntitlementRevocationReplayAndHistoricalBoundary(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("revocation-boundary")
	if err := store.CreateAccount(ctx, account, string(account)); err != nil {
		t.Fatal(err)
	}
	plan := assignmentPlan("revocation-plan")
	if err := store.PublishPlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	service := catalog.NewEntitlement(store.Entitlements(), testTime)
	assignment := assignmentInput(account, "revocable-assignment", plan.ID, catalog.SourceManual)
	got, err := service.Assign(ctx, assignment)
	if err != nil {
		t.Fatal(err)
	}
	effective := testTime().Add(time.Hour)
	in := catalog.Revocation{Account: account, AssignmentID: got.Plan.ID, SourceRef: "revocation-source", Actor: "operator", Reason: "closed", EffectiveAt: effective}
	first, err := service.Revoke(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := service.Revoke(ctx, in)
	if err != nil || replay.CreatedAt != first.CreatedAt {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	in.Reason = "changed"
	if _, err := service.Revoke(ctx, in); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("changed replay=%v", err)
	}
	before, err := catalog.ResolveEntitlements(catalog.EntitlementInput{At: effective.Add(-time.Nanosecond), Assignments: []catalog.PlanAssignment{got.Plan}, Versions: []catalog.PlanVersion{plan}, Revocations: []catalog.Revocation{first}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := before.Get("feature"); !ok {
		t.Fatal("revocation applied before boundary")
	}
	after, err := catalog.ResolveEntitlements(catalog.EntitlementInput{At: effective, Assignments: []catalog.PlanAssignment{got.Plan}, Versions: []catalog.PlanVersion{plan}, Revocations: []catalog.Revocation{first}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := after.Get("feature"); ok {
		t.Fatal("revocation missing at boundary")
	}
}

func TestPostgresEntitlementRevocationRootRollback(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("revocation-rollback")
	if err := store.CreateAccount(ctx, account, string(account)); err != nil {
		t.Fatal(err)
	}
	plan := assignmentPlan("revocation-rollback-plan")
	if err := store.PublishPlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	service := catalog.NewEntitlement(store.Entitlements(), testTime)
	assignment := assignmentInput(account, "rollback-assignment", plan.ID, catalog.SourceManual)
	if _, err := service.Assign(ctx, assignment); err != nil {
		t.Fatal(err)
	}
	rev := catalog.Revocation{Account: account, AssignmentID: assignment.Plan.ID, SourceRef: "rollback-source", Actor: "operator", Reason: "rollback", EffectiveAt: testTime()}
	sentinel := errors.New("rollback")
	err := store.Atomic(ctx, account, func(v integration.Session) error {
		_, err := catalog.NewEntitlement(v.(*session).Entitlements(), testTime).Revoke(ctx, rev)
		if err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	if _, err := service.Revocation(ctx, account, rev.AssignmentID); !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("revocation after rollback=%v", err)
	}
}

func TestPostgresEntitlementRevocationDeferredCommitReturnsZero(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("revocation-commit-failure")
	if err := store.CreateAccount(ctx, account, string(account)); err != nil {
		t.Fatal(err)
	}
	plan := assignmentPlan("revocation-commit-plan")
	if err := store.PublishPlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	service := catalog.NewEntitlement(store.Entitlements(), testTime)
	assignment := assignmentInput(account, "commit-revocation-assignment", plan.ID, catalog.SourceManual)
	if _, err := service.Assign(ctx, assignment); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE FUNCTION fail_revocation_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'revocation commit failure'; END $$; CREATE CONSTRAINT TRIGGER fail_revocation_commit AFTER INSERT ON billing_entitlement_assignment_revocations DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION fail_revocation_commit()`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = db.ExecContext(ctx, `DROP TRIGGER IF EXISTS fail_revocation_commit ON billing_entitlement_assignment_revocations`)
		_, _ = db.ExecContext(ctx, `DROP FUNCTION IF EXISTS fail_revocation_commit()`)
	}()
	rev := catalog.Revocation{Account: account, AssignmentID: assignment.Plan.ID, SourceRef: "commit-source", Actor: "operator", Reason: "commit", EffectiveAt: testTime()}
	got, err := service.Revoke(ctx, rev)
	if err == nil || got != (catalog.Revocation{}) {
		t.Fatalf("failed commit result=%+v err=%v", got, err)
	}
	if _, err := service.Revocation(ctx, account, rev.AssignmentID); !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("persisted failed revocation=%v", err)
	}
}
