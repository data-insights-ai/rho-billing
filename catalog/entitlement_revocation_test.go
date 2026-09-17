package catalog

import (
	"context"
	"errors"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
)

type revocationTestRepo struct {
	assignment Assignment
	revocation *Revocation
	plan       PlanVersion
}

func (r *revocationTestRepo) WithinAccount(_ context.Context, account billing.AccountID, fn func(EntitlementTx) error) error {
	if account != r.assignment.Account {
		return billing.ErrNotFound
	}
	return fn(revocationTestTx{r: r})
}

type revocationTestTx struct{ r *revocationTestRepo }

func (t revocationTestTx) Assignment(_ context.Context, id string) (Assignment, error) {
	if id != t.r.assignment.Plan.ID {
		return Assignment{}, billing.ErrNotFound
	}
	return t.r.assignment, nil
}
func (t revocationTestTx) InsertAssignment(context.Context, Assignment) error { return nil }
func (t revocationTestTx) Revocation(_ context.Context, id string) (Revocation, error) {
	if t.r.revocation == nil || t.r.revocation.AssignmentID != id {
		return Revocation{}, billing.ErrNotFound
	}
	return *t.r.revocation, nil
}
func (t revocationTestTx) InsertRevocation(_ context.Context, in Revocation) error {
	if t.r.revocation != nil {
		return billing.ErrConflict
	}
	v := in
	t.r.revocation = &v
	return nil
}
func (t revocationTestTx) Plan(_ context.Context, id string) (PlanVersion, error) {
	if id != t.r.plan.ID {
		return PlanVersion{}, billing.ErrNotFound
	}
	return t.r.plan, nil
}

func TestRevocationReplayConflictAndBoundary(t *testing.T) {
	start := billing.CanonicalTime(time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC))
	clock := start.Add(time.Hour)
	assignment := Assignment{Account: "acct", Plan: PlanAssignment{ID: "assignment", PlanVersionID: "plan", Quantity: 1, Effective: billing.Period{Start: start}, Perpetual: true, Source: SourceManual}, SourceRef: "source", Actor: "operator", Reason: "manual", CreatedAt: start}
	r := &revocationTestRepo{assignment: assignment, plan: PlanVersion{ID: "plan", PlanID: "family", Version: 1, Entitlements: []EntitlementDefinition{{Key: "feature", Kind: EntitlementFeature, Aggregation: AggregationOR, Enabled: true}}}}
	s := NewEntitlement(r, func() time.Time { return clock })
	in := Revocation{Account: "acct", AssignmentID: "assignment", SourceRef: "revoke", Actor: "operator", Reason: "closed", EffectiveAt: start.Add(30 * time.Minute)}
	first, err := s.Revoke(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := s.Revoke(t.Context(), in)
	if err != nil || replay.CreatedAt != first.CreatedAt {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	in.Reason = "changed"
	if _, err := s.Revoke(t.Context(), in); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("changed=%v", err)
	}
	before, err := ResolveEntitlements(EntitlementInput{At: start.Add(29 * time.Minute), Assignments: []PlanAssignment{assignment.Plan}, Versions: []PlanVersion{r.plan}, Revocations: []Revocation{first}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := before.Get("feature"); !ok {
		t.Fatal("revocation applied before boundary")
	}
	after, err := ResolveEntitlements(EntitlementInput{At: start.Add(30 * time.Minute), Assignments: []PlanAssignment{assignment.Plan}, Versions: []PlanVersion{r.plan}, Revocations: []Revocation{first}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := after.Get("feature"); ok {
		t.Fatal("revocation not applied at boundary")
	}
}

func TestRevocationRejectsAmbiguousHistoricalAssignmentID(t *testing.T) {
	start := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	first := PlanAssignment{ID: "reused", PlanVersionID: "plan", Quantity: 1, Effective: billing.Period{Start: start, End: start.Add(time.Hour)}, Source: SourceManual}
	second := first
	second.Effective = billing.Period{Start: start.Add(time.Hour), End: start.Add(2 * time.Hour)}
	revocation := Revocation{Account: "acct", AssignmentID: "reused", SourceRef: "refund", Actor: "operator", Reason: "refund", EffectiveAt: start.Add(time.Hour), CreatedAt: start.Add(time.Hour)}
	if _, err := ResolveEntitlements(EntitlementInput{At: start.Add(time.Hour), Assignments: []PlanAssignment{first, second}, Revocations: []Revocation{revocation}}); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("ambiguous assignment accepted: %v", err)
	}
}

// An assignment ID may legitimately repeat across adjacent, non-overlapping
// intervals. Without a way to name one, every revocation for such an ID was
// ambiguous, which made entitlement resolution fail for the whole account for
// as long as the revocation existed.
func TestRevocationTargetsOneIntervalOfAReusedAssignmentID(t *testing.T) {
	start := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	plan := PlanVersion{ID: "plan", PlanID: "family", Version: 1, Entitlements: []EntitlementDefinition{{Key: "feature", Kind: EntitlementFeature, Aggregation: AggregationOR, Enabled: true}}}
	first := PlanAssignment{ID: "reused", PlanVersionID: "plan", Quantity: 1, Effective: billing.Period{Start: start, End: start.Add(time.Hour)}, Source: SourceManual}
	second := first
	second.Effective = billing.Period{Start: start.Add(time.Hour), End: start.Add(2 * time.Hour)}
	revocation := Revocation{Account: "acct", AssignmentID: "reused", SourceRef: "refund", Actor: "operator", Reason: "refund", EffectiveAt: start.Add(time.Hour), CreatedAt: start.Add(time.Hour), AssignmentStart: second.Effective.Start}
	input := EntitlementInput{At: start.Add(90 * time.Minute), Assignments: []PlanAssignment{first, second}, Versions: []PlanVersion{plan}, Revocations: []Revocation{revocation}}
	snapshot, err := ResolveEntitlements(input)
	if err != nil {
		t.Fatalf("scoped revocation rejected: %v", err)
	}
	if _, ok := snapshot.Get("feature"); ok {
		t.Fatal("revocation did not apply to the named interval")
	}
	// The earlier interval is untouched by a revocation naming the later one.
	earlier := input
	earlier.At = start.Add(30 * time.Minute)
	active, err := ResolveEntitlements(earlier)
	if err != nil {
		t.Fatalf("earlier interval rejected: %v", err)
	}
	if _, ok := active.Get("feature"); !ok {
		t.Fatal("revocation of the later interval revoked the earlier one")
	}
}

func (t revocationTestTx) AssignmentsPage(_ context.Context, after string, limit int) ([]Assignment, error) {
	if limit < 1 {
		return nil, billing.ErrInvalid
	}
	if after != "" && after >= t.r.assignment.Plan.ID {
		return nil, nil
	}
	return []Assignment{t.r.assignment}, nil
}

func (t revocationTestTx) RevocationsPage(_ context.Context, after string, limit int) ([]Revocation, error) {
	if limit < 1 {
		return nil, billing.ErrInvalid
	}
	if t.r.revocation == nil || (after != "" && after >= t.r.revocation.AssignmentID) {
		return nil, nil
	}
	return []Revocation{*t.r.revocation}, nil
}
