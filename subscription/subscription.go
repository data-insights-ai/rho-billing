// Package subscription: provider payment status is never an implicit access or credit-grant policy.
package subscription

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/catalog"
	"github.com/data-insights-ai/rho-billing/internal/checked"
	"github.com/data-insights-ai/rho-billing/internal/identity"
)

var ErrReconcile = errors.New("subscription: equal-time conflict requires authoritative reconciliation")

type Item struct {
	ID, PlanVersion string
	Quantity        int64
	Period          billing.Period
}
type ScheduledChange struct {
	Kind        string
	EffectiveAt time.Time
}
type Snapshot struct {
	Account     billing.AccountID
	Ref         billing.Reference
	Status      string
	Items       []Item
	Change      ScheduledChange
	Assignments []catalog.PlanAssignment
	Revision    int64
	SourceTime  time.Time
	LastEvent   string
	// SourceFingerprint fences equal-time reconciliation and detects a changed
	// replay of the event that produced this snapshot.
	SourceFingerprint string
}

// Observation.SourceTime is the adapter's conservative observation fence
// (never the subscription creation timestamp or API response completion).
// Reconciliation requires a revision captured before the API read.
type Observation struct {
	Snapshot         Snapshot
	EventID          string
	OccurredAt       time.Time
	Reconcile        bool
	ExpectedRevision int64
}

type Event struct {
	Ref         billing.Reference
	EventID     string
	OccurredAt  time.Time
	Fingerprint string
	Result      Snapshot
}

type Repository interface {
	WithinAccount(context.Context, billing.AccountID, func(Tx) error) error
}

type Tx interface {
	Snapshot(context.Context, billing.Reference) (Snapshot, error)
	Event(context.Context, billing.Reference, string) (Event, bool, error)
	Owner(context.Context, billing.Reference) (billing.AccountID, bool, error)
	PublishedPlans(context.Context, []string) error
	SaveSnapshot(context.Context, Snapshot) error
	SaveEvent(context.Context, Event) error
	Lifecycle(context.Context, string) (Lifecycle, error)
	SaveLifecycle(context.Context, Lifecycle, int64) error
	Change(context.Context, string) (ChangeRecord, bool, error)
	SaveChange(context.Context, ChangeRecord) error
}

type Service struct{ repo Repository }

func New(repo Repository) *Service {
	if repo == nil {
		panic("subscription: nil repository")
	}
	return &Service{repo: repo}
}

func (s *Service) Observe(ctx context.Context, in Observation) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	var out Snapshot
	err := s.repo.WithinAccount(ctx, in.Snapshot.Account, func(tx Tx) error {
		if err := Validate(in.Snapshot); err != nil {
			return err
		}
		if !billing.ValidID(in.EventID) || in.OccurredAt.IsZero() {
			return billing.ErrInvalid
		}
		owner, found, err := tx.Owner(ctx, in.Snapshot.Ref)
		if err != nil {
			return err
		}
		if found && owner != in.Snapshot.Account {
			return billing.ErrConflict
		}
		fingerprint := ObservationFingerprint(in)
		oldEvent, found, err := tx.Event(ctx, in.Snapshot.Ref, in.EventID)
		if err != nil {
			return err
		}
		if found {
			if oldEvent.Fingerprint != fingerprint {
				return billing.ErrConflict
			}
			out = Copy(oldEvent.Result)
			return nil
		}
		old, err := tx.Snapshot(ctx, in.Snapshot.Ref)
		var current *Snapshot
		if err == nil {
			current = &old
		} else if !errors.Is(err, billing.ErrNotFound) {
			return err
		}
		updated, err := apply(current, in)
		if err != nil {
			return err
		}
		if current == nil || current.Revision != updated.Revision {
			if err := tx.PublishedPlans(ctx, snapshotPlans(updated)); err != nil {
				return err
			}
			if err := tx.SaveSnapshot(ctx, updated); err != nil {
				return err
			}
		}
		if err := tx.SaveEvent(ctx, Event{Ref: updated.Ref, EventID: in.EventID, OccurredAt: in.OccurredAt, Fingerprint: fingerprint, Result: updated}); err != nil {
			return err
		}
		out = Copy(updated)
		return nil
	})
	if err != nil {
		return Snapshot{}, err
	}
	return out, nil
}

func snapshotPlans(s Snapshot) []string {
	plans := make([]string, 0, len(s.Items)+len(s.Assignments))
	seen := make(map[string]struct{}, cap(plans))
	for _, item := range s.Items {
		if _, exists := seen[item.PlanVersion]; !exists {
			seen[item.PlanVersion] = struct{}{}
			plans = append(plans, item.PlanVersion)
		}
	}
	for _, assignment := range s.Assignments {
		if _, exists := seen[assignment.PlanVersionID]; !exists {
			seen[assignment.PlanVersionID] = struct{}{}
			plans = append(plans, assignment.PlanVersionID)
		}
	}
	return plans
}

func (s *Service) Subscription(ctx context.Context, account billing.AccountID, ref billing.Reference) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	if !billing.ValidID(string(account)) || !ref.Valid() {
		return Snapshot{}, billing.ErrInvalid
	}
	var out Snapshot
	err := s.repo.WithinAccount(ctx, account, func(tx Tx) error {
		var err error
		out, err = tx.Snapshot(ctx, ref)
		return err
	})
	if err != nil {
		return Snapshot{}, err
	}
	if out.Account != account || out.Ref != ref {
		return Snapshot{}, billing.ErrConflict
	}
	return Copy(out), nil
}

type PlanResolver interface {
	Plan(context.Context, string) (catalog.PlanVersion, error)
}

type AccountResolver interface {
	Account(context.Context, billing.AccountID) (catalog.Account, error)
}

type MemoryConfig struct {
	Catalog  PlanResolver
	Accounts AccountResolver
}

func Copy(s Snapshot) Snapshot {
	s.Items = slices.Clone(s.Items)
	s.Assignments = slices.Clone(s.Assignments)
	return s
}

func CanonicalSnapshot(s Snapshot) Snapshot {
	s = Copy(s)
	s.SourceTime = billing.CanonicalTime(s.SourceTime)
	s.Change.EffectiveAt = billing.CanonicalTime(s.Change.EffectiveAt)
	for i := range s.Items {
		s.Items[i].Period.Start = billing.CanonicalTime(s.Items[i].Period.Start)
		s.Items[i].Period.End = billing.CanonicalTime(s.Items[i].Period.End)
	}
	for i := range s.Assignments {
		s.Assignments[i].Effective.Start = billing.CanonicalTime(s.Assignments[i].Effective.Start)
		s.Assignments[i].Effective.End = billing.CanonicalTime(s.Assignments[i].Effective.End)
	}
	return s
}

func Validate(s Snapshot) error {
	if !billing.ValidID(string(s.Account)) || !s.Ref.Valid() || !billing.ValidID(s.Status) {
		return billing.ErrInvalid
	}
	seenItems := map[string]bool{}
	for _, i := range s.Items {
		if !billing.ValidID(i.ID) || !billing.ValidID(i.PlanVersion) || i.Quantity <= 0 || !i.Period.Valid() || seenItems[i.ID] {
			return billing.ErrInvalid
		}
		seenItems[i.ID] = true
	}
	seenAssignments := map[string]struct{}{}
	for index, a := range s.Assignments {
		if !billing.ValidID(a.ID) || !billing.ValidID(a.PlanVersionID) || !a.Effective.Valid() || a.Perpetual || a.Quantity <= 0 || !validSource(a.Source) {
			return billing.ErrInvalid
		}
		if _, exists := seenAssignments[a.ID]; exists {
			// Reusing an assignment ID is valid only for adjacent historical
			// intervals; an overlap would make the identity ambiguous.
			for oldIndex, old := range s.Assignments {
				if oldIndex >= index {
					break
				}
				if old.ID == a.ID && old.Effective.Start.Before(a.Effective.End) && a.Effective.Start.Before(old.Effective.End) {
					return billing.ErrConflict
				}
			}
		}
		seenAssignments[a.ID] = struct{}{}
	}
	if (s.Change.Kind == "") != s.Change.EffectiveAt.IsZero() {
		return billing.ErrInvalid
	}
	return nil
}

func validSource(source catalog.AssignmentSource) bool {
	switch source {
	case catalog.SourceSubscription, catalog.SourceAddOn, catalog.SourceMigration, catalog.SourceManual:
		return true
	default:
		return false
	}
}

// Identity fingerprints provider state only, excluding receipt/revision metadata.
func Identity(s Snapshot) string {
	items := slices.Clone(s.Items)
	slices.SortFunc(items, func(a, b Item) int {
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	as := slices.Clone(s.Assignments)
	slices.SortFunc(as, func(a, b catalog.PlanAssignment) int {
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return a.Effective.Start.Compare(b.Effective.Start)
	})
	f := []string{string(s.Account), s.Ref.Scope.Provider, s.Ref.Scope.Merchant, s.Ref.Scope.Environment, s.Ref.ID, s.Status, s.Change.Kind, identity.Instant(s.Change.EffectiveAt)}
	for _, i := range items {
		f = append(f, i.ID, i.PlanVersion, strconv.FormatInt(i.Quantity, 10), identity.Instant(i.Period.Start), identity.Instant(i.Period.End))
	}
	for _, a := range as {
		f = append(f, a.ID, a.PlanVersionID, strconv.FormatInt(a.Quantity, 10), identity.Instant(a.Effective.Start), identity.Instant(a.Effective.End), strconv.FormatInt(int64(a.Source), 10))
	}
	return identity.Fingerprint(f...)
}

// ObservationFingerprint is retained independently of the current projection
// so an old event ID cannot be replayed with a changed timestamp or snapshot.
func ObservationFingerprint(in Observation) string {
	in.Snapshot = CanonicalSnapshot(in.Snapshot)
	in.OccurredAt = billing.CanonicalTime(in.OccurredAt)
	return identity.Fingerprint("subscription-event", string(in.Snapshot.Account), in.Snapshot.Ref.Scope.Provider, in.Snapshot.Ref.Scope.Merchant, in.Snapshot.Ref.Scope.Environment, in.Snapshot.Ref.ID, in.EventID, identity.Instant(in.OccurredAt), strconv.FormatBool(in.Reconcile), strconv.FormatInt(in.ExpectedRevision, 10), Identity(in.Snapshot))
}

// apply: stale observations return the existing snapshot; equal-time
// disagreements are explicit and never resolved by arbitrary event-ID sorting.
func apply(current *Snapshot, in Observation) (Snapshot, error) {
	in.Snapshot = CanonicalSnapshot(in.Snapshot)
	in.OccurredAt = billing.CanonicalTime(in.OccurredAt)
	if err := Validate(in.Snapshot); err != nil {
		return Snapshot{}, err
	}
	if !billing.ValidID(in.EventID) || in.OccurredAt.IsZero() {
		return Snapshot{}, billing.ErrInvalid
	}
	var revision int64
	if current != nil {
		if current.Account != in.Snapshot.Account || current.Ref != in.Snapshot.Ref {
			return Snapshot{}, billing.ErrConflict
		}
		revision = current.Revision
		if current.LastEvent == in.EventID {
			if Identity(*current) != Identity(in.Snapshot) || (!current.SourceTime.IsZero() && !current.SourceTime.Equal(in.OccurredAt)) || (current.SourceFingerprint != "" && current.SourceFingerprint != ObservationFingerprint(in)) {
				return Snapshot{}, billing.ErrConflict
			}
			return Copy(*current), nil
		}
		if in.Reconcile {
			if in.ExpectedRevision != revision {
				return Snapshot{}, billing.ErrConflict
			}
			if in.OccurredAt.Before(current.SourceTime) {
				return Snapshot{}, billing.ErrConflict
			}
		} else {
			if in.OccurredAt.Before(current.SourceTime) {
				return Copy(*current), nil
			}
			if in.OccurredAt.Equal(current.SourceTime) {
				if Identity(*current) != Identity(in.Snapshot) {
					return Snapshot{}, ErrReconcile
				}
				return Copy(*current), nil
			}
		}
	} else if in.Reconcile && in.ExpectedRevision != 0 {
		return Snapshot{}, billing.ErrConflict
	}
	next, err := checked.Add(revision, 1)
	if err != nil {
		return Snapshot{}, err
	}
	out := Copy(in.Snapshot)
	out.Revision = next
	out.SourceTime = in.OccurredAt
	out.LastEvent = in.EventID
	out.SourceFingerprint = ObservationFingerprint(in)
	return out, nil
}
