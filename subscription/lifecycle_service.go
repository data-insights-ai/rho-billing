package subscription

import (
	"context"
	"errors"
	"slices"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/internal/checked"
)

func (s *Service) Activate(ctx context.Context, in ActivateInput) (Lifecycle, error) {
	if err := ctx.Err(); err != nil {
		return Lifecycle{}, err
	}
	at := billing.CanonicalTime(in.At)
	if at.IsZero() || !billing.ValidID(string(in.Account)) || !billing.ValidID(in.ID) || !billing.ValidID(in.Operation) || in.Quantity <= 0 || !in.Policies.valid() || len(in.Items) == 0 {
		return Lifecycle{}, billing.ErrInvalid
	}
	if in.Policies.Collection == CollectionNone && in.Policies.Access == AccessPaid {
		return Lifecycle{}, billing.ErrInvalid
	}
	items := slices.Clone(in.Items)
	life := Lifecycle{
		Account: in.Account, ID: in.ID, Ref: in.Ref, Revision: 1,
		DesiredQuantity: in.Quantity, QuantityRevision: 1, Items: items,
		Policies: in.Policies, Coverage: in.Coverage, QuoteFingerprint: in.QuoteFingerprint,
		CreatedAt: at, UpdatedAt: at, Collection: CollectionIdle,
	}
	if !in.Coverage.Start.IsZero() {
		life.AllowanceAnchor = billing.CanonicalTime(in.Coverage.Start)
		life.AllowanceDay = in.Coverage.Start.UTC().Day()
	} else {
		life.AllowanceAnchor = at
		life.AllowanceDay = at.UTC().Day()
	}
	life.Access = initialAccess(in.Trial, in.Policies)
	if in.Policies.Collection != CollectionNone {
		life.Collection = CollectionPending
	}
	if err := life.Validate(); err != nil {
		return Lifecycle{}, err
	}
	change := newChange(in.Account, in.ID, in.Operation, ChangeActivate, ChangeAccepted, in.Quantity, 1, items, at, in.Policies, at)
	var out Lifecycle
	err := s.repo.WithinAccount(ctx, in.Account, func(tx Tx) error {
		if replay, ok, err := replayChange(ctx, tx, in.ID, change); err != nil {
			return err
		} else if ok {
			out = replay
			return nil
		}
		if _, err := tx.Lifecycle(ctx, in.ID); !errors.Is(err, billing.ErrNotFound) {
			if err == nil {
				return billing.ErrConflict
			}
			return err
		}
		if err := tx.SaveLifecycle(ctx, life, 0); err != nil {
			return err
		}
		if err := tx.SaveChange(ctx, change); err != nil {
			return err
		}
		out = copyLifecycle(life)
		return nil
	})
	return out, err
}

func initialAccess(trial bool, p Policies) AccessState {
	if trial {
		return AccessTrial
	}
	if p.Access == AccessPaid {
		return AccessNone
	}
	return AccessActive
}

func (s *Service) SetDesiredQuantity(ctx context.Context, in QuantityInput) (Lifecycle, error) {
	if err := ctx.Err(); err != nil {
		return Lifecycle{}, err
	}
	at := billing.CanonicalTime(in.At)
	if at.IsZero() || !billing.ValidID(string(in.Account)) || !billing.ValidID(in.ID) || !billing.ValidID(in.Operation) || in.Quantity <= 0 || in.Revision <= 0 {
		return Lifecycle{}, billing.ErrInvalid
	}
	var out Lifecycle
	err := s.repo.WithinAccount(ctx, in.Account, func(tx Tx) error {
		life, err := tx.Lifecycle(ctx, in.ID)
		if err != nil {
			return err
		}
		change := newChange(in.Account, in.ID, in.Operation, ChangeQuantity, ChangePlanned, in.Quantity, in.Revision, nil, time.Time{}, life.Policies, at)
		if replay, ok, err := replayChange(ctx, tx, in.ID, change); err != nil {
			return err
		} else if ok {
			out = replay
			return nil
		}
		if in.Revision < life.QuantityRevision {
			return billing.ErrConflict
		}
		if in.Revision == life.QuantityRevision {
			if life.DesiredQuantity != in.Quantity {
				return billing.ErrConflict
			}
			out = copyLifecycle(life)
			return nil
		}
		next, err := checked.Add(life.Revision, 1)
		if err != nil {
			return err
		}
		if err := supersedePending(ctx, tx, life, at); err != nil {
			return err
		}
		life.Revision = next
		life.DesiredQuantity = in.Quantity
		life.QuantityRevision = in.Revision
		life.PendingOperation = in.Operation
		life.UpdatedAt = at
		if len(life.Items) == 1 {
			life.Items = slices.Clone(life.Items)
			life.Items[0].Quantity = in.Quantity
		}
		if err := tx.SaveLifecycle(ctx, life, next-1); err != nil {
			return err
		}
		if err := tx.SaveChange(ctx, change); err != nil {
			return err
		}
		out = copyLifecycle(life)
		return nil
	})
	return out, err
}

func (s *Service) RequestChange(ctx context.Context, in ChangeInput) (Lifecycle, error) {
	if err := ctx.Err(); err != nil {
		return Lifecycle{}, err
	}
	at := billing.CanonicalTime(in.At)
	if at.IsZero() || !billing.ValidID(string(in.Account)) || !billing.ValidID(in.ID) || !billing.ValidID(in.Operation) || !in.Kind.valid() || in.Kind == ChangeActivate || in.Kind == ChangeQuantity || in.Kind == ChangeCollect {
		return Lifecycle{}, billing.ErrInvalid
	}
	var out Lifecycle
	err := s.repo.WithinAccount(ctx, in.Account, func(tx Tx) error {
		life, err := tx.Lifecycle(ctx, in.ID)
		if err != nil {
			return err
		}
		policies := in.Policies
		if !policies.valid() {
			policies = life.Policies
		}
		effective := billing.CanonicalTime(in.EffectiveAt)
		if effective.IsZero() {
			effective = scheduledTime(life, policies, at)
		}
		items := slices.Clone(in.Items)
		if len(items) == 0 {
			items = slices.Clone(life.Items)
		}
		quantity := in.Quantity
		if quantity == 0 {
			quantity = life.DesiredQuantity
		}
		revision := in.QuantityRevision
		if revision == 0 {
			revision = life.QuantityRevision
		}
		change := newChange(in.Account, in.ID, in.Operation, in.Kind, ChangeAccepted, quantity, revision, items, effective, policies, at)
		if replay, ok, err := replayChange(ctx, tx, in.ID, change); err != nil {
			return err
		} else if ok {
			out = replay
			return nil
		}
		if revision < life.QuantityRevision {
			return billing.ErrConflict
		}
		next, err := checked.Add(life.Revision, 1)
		if err != nil {
			return err
		}
		pending := life.Change.Kind
		if err := supersedePending(ctx, tx, life, at); err != nil {
			return err
		}
		life.Revision = next
		life.UpdatedAt = at
		life.Change = ScheduledChange{Kind: string(in.Kind), EffectiveAt: effective}
		life.PendingOperation = in.Operation
		if in.QuoteFingerprint != "" {
			life.QuoteFingerprint = in.QuoteFingerprint
		}
		switch in.Kind {
		case ChangeUpgrade, ChangeDowngrade, ChangeAddOn:
			life.Items = items
			life.DesiredQuantity = quantity
			life.QuantityRevision = revision
			if policies.Allowance == AllowanceReissue {
				life.AllowanceAnchor = effective
				life.AllowanceDay = effective.UTC().Day()
			}
			life.Policies.Proration = policies.Proration
			life.Policies.Allowance = policies.Allowance
		case ChangeCancel:
			// A change scheduled for later must not take access away now: the
			// customer has paid through the current period. The schedule is
			// recorded, and the access transition follows the provider's own
			// observation of the change taking effect.
			if !effective.After(at) {
				life.Access = AccessCanceled
			}
		case ChangePause:
			if !effective.After(at) {
				life.Access = AccessPaused
			}
		case ChangeResume:
			// Resume both lifts an active pause and withdraws one that was only
			// scheduled; supersedePending above already retired the pending
			// pause command.
			if life.Access != AccessPaused && pending != string(ChangePause) {
				return billing.ErrState
			}
			if life.Access == AccessPaused && !effective.After(at) {
				life.Access = accessAfterResume(life.Policies, life.Collection)
			}
		}
		if err := life.Validate(); err != nil {
			return err
		}
		if err := tx.SaveLifecycle(ctx, life, next-1); err != nil {
			return err
		}
		if err := tx.SaveChange(ctx, change); err != nil {
			return err
		}
		out = copyLifecycle(life)
		return nil
	})
	return out, err
}

func scheduledTime(life Lifecycle, policies Policies, at time.Time) time.Time {
	if policies.Proration == ProrationNextPeriod {
		if life.Coverage.Valid() {
			return billing.CanonicalTime(life.Coverage.End)
		}
		if len(life.Items) > 0 && life.Items[0].Period.Valid() {
			return billing.CanonicalTime(life.Items[0].Period.End)
		}
	}
	return at
}

func accessAfterResume(p Policies, collection CollectionState) AccessState {
	if p.Access == AccessPaid && collection != CollectionPaid {
		return AccessNone
	}
	return AccessActive
}

func (s *Service) RecordCollection(ctx context.Context, in CollectionInput) (Lifecycle, error) {
	if err := ctx.Err(); err != nil {
		return Lifecycle{}, err
	}
	at := billing.CanonicalTime(in.At)
	if at.IsZero() || !billing.ValidID(string(in.Account)) || !billing.ValidID(in.ID) || !billing.ValidID(in.Operation) || !in.State.valid() {
		return Lifecycle{}, billing.ErrInvalid
	}
	var out Lifecycle
	err := s.repo.WithinAccount(ctx, in.Account, func(tx Tx) error {
		life, err := tx.Lifecycle(ctx, in.ID)
		if err != nil {
			return err
		}
		change := newChange(in.Account, in.ID, in.Operation, ChangeCollect, ChangeAccepted, life.DesiredQuantity, life.QuantityRevision, nil, at, life.Policies, at)
		change.Collection = in.State
		change.Fingerprint = change.fingerprint()
		if replay, ok, err := replayChange(ctx, tx, in.ID, change); err != nil {
			return err
		} else if ok {
			out = replay
			return nil
		}
		if life.Policies.Collection == CollectionNone {
			return billing.ErrState
		}
		next, err := checked.Add(life.Revision, 1)
		if err != nil {
			return err
		}
		life.Revision = next
		life.Collection = in.State
		life.UpdatedAt = at
		life.Access = applyCollectionAccess(life.Access, life.Policies, in.State)
		if err := tx.SaveLifecycle(ctx, life, next-1); err != nil {
			return err
		}
		if err := tx.SaveChange(ctx, change); err != nil {
			return err
		}
		out = copyLifecycle(life)
		return nil
	})
	return out, err
}

func applyCollectionAccess(access AccessState, p Policies, collection CollectionState) AccessState {
	if access == AccessCanceled || access == AccessPaused {
		return access
	}
	switch collection {
	case CollectionPaid:
		if p.Access == AccessPaid || p.Access == AccessImmediate || p.Access == AccessGrace {
			return AccessActive
		}
		return access
	case CollectionFailed, CollectionPastDue:
		if p.Access == AccessGrace {
			return AccessInGrace
		}
		if p.Access == AccessPaid {
			return AccessNone
		}
		return access
	default:
		return access
	}
}

func (s *Service) Confirm(ctx context.Context, account billing.AccountID, id string, snapshot Snapshot) (Lifecycle, error) {
	if err := ctx.Err(); err != nil {
		return Lifecycle{}, err
	}
	if !billing.ValidID(string(account)) || !billing.ValidID(id) {
		return Lifecycle{}, billing.ErrInvalid
	}
	if err := Validate(snapshot); err != nil {
		return Lifecycle{}, err
	}
	// A snapshot without the adapter's observation fence has no defensible
	// confirmation time; inventing one would reorder reconciliation.
	if snapshot.SourceTime.IsZero() {
		return Lifecycle{}, billing.ErrInvalid
	}
	var out Lifecycle
	err := s.repo.WithinAccount(ctx, account, func(tx Tx) error {
		life, err := tx.Lifecycle(ctx, id)
		if err != nil {
			return err
		}
		if life.Ref.ID != "" && life.Ref != snapshot.Ref {
			return billing.ErrConflict
		}
		// The observation fence must also order confirmations. A redelivered or
		// reordered snapshot would otherwise regress ConfirmedQuantity, move
		// UpdatedAt backward, and — if the stale value happened to equal the
		// desired one — clear an in-flight change. Observe applies the same rule.
		fence := billing.CanonicalTime(snapshot.SourceTime)
		if !life.UpdatedAt.IsZero() && fence.Before(life.UpdatedAt) {
			out = copyLifecycle(life)
			return nil
		}
		next, err := checked.Add(life.Revision, 1)
		if err != nil {
			return err
		}
		life.Revision = next
		life.Ref = snapshot.Ref
		life.ConfirmedQuantity = confirmedQuantity(snapshot)
		life.UpdatedAt = fence
		// The provider's snapshot carries the billing period of the plan
		// the subscription holds now. Adopting it keeps the items and the
		// coverage in step with a plan change.
		//
		// Without this both keep the window of the first purchase for
		// ever: an organization that started on a monthly plan and moved
		// to a yearly one reads as though its year ends in four weeks,
		// and a change scheduled for "next period" lands on a date that
		// passed months ago. Activation is the only other writer, and it
		// refuses a subscription that already exists.
		//
		// Validate has already required a usable period on every item, so
		// there is nothing to guard against here: either the provider sent
		// items or it sent none. The provider reports one billing period
		// for the subscription and every item carries it, so the first is
		// the term.
		if len(snapshot.Items) > 0 {
			life.Items = slices.Clone(snapshot.Items)
			life.Coverage = snapshot.Items[0].Period
		}
		if life.ConfirmedQuantity == life.DesiredQuantity {
			life.Change = ScheduledChange{}
			life.PendingOperation = ""
		}
		if err := tx.SaveLifecycle(ctx, life, next-1); err != nil {
			return err
		}
		out = copyLifecycle(life)
		return nil
	})
	return out, err
}

func confirmedQuantity(snapshot Snapshot) int64 {
	var total int64
	for _, item := range snapshot.Items {
		total += item.Quantity
	}
	return total
}

func (s *Service) PreviewChange(in ChangeInput, current Lifecycle) (Preview, error) {
	if !in.Kind.valid() || !current.Policies.valid() {
		return Preview{}, billing.ErrInvalid
	}
	policies := in.Policies
	if !policies.valid() {
		policies = current.Policies
	}
	at := billing.CanonicalTime(in.At)
	effective := billing.CanonicalTime(in.EffectiveAt)
	if effective.IsZero() {
		effective = scheduledTime(current, policies, at)
	}
	quantity := in.Quantity
	if quantity == 0 {
		quantity = current.DesiredQuantity
	}
	return Preview{Kind: in.Kind, EffectiveAt: effective, Policies: policies, Quantity: quantity}, nil
}

func (s *Service) Lifecycle(ctx context.Context, account billing.AccountID, id string) (Lifecycle, error) {
	if err := ctx.Err(); err != nil {
		return Lifecycle{}, err
	}
	if !billing.ValidID(string(account)) || !billing.ValidID(id) {
		return Lifecycle{}, billing.ErrInvalid
	}
	var out Lifecycle
	err := s.repo.WithinAccount(ctx, account, func(tx Tx) error {
		got, err := tx.Lifecycle(ctx, id)
		if err != nil {
			return err
		}
		out = copyLifecycle(got)
		return nil
	})
	return out, err
}

func newChange(account billing.AccountID, lifecycleID, operation string, kind ChangeKind, state ChangeState, quantity, revision int64, items []Item, effective time.Time, policies Policies, at time.Time) ChangeRecord {
	c := ChangeRecord{Account: account, LifecycleID: lifecycleID, Operation: operation, Kind: kind, State: state, Quantity: quantity, Revision: revision, Items: slices.Clone(items), EffectiveAt: billing.CanonicalTime(effective), Policies: policies, CreatedAt: at}
	c.Fingerprint = c.fingerprint()
	return c
}

func replayChange(ctx context.Context, tx Tx, lifecycleID string, want ChangeRecord) (Lifecycle, bool, error) {
	existing, found, err := tx.Change(ctx, want.Operation)
	if err != nil || !found {
		return Lifecycle{}, false, err
	}
	if existing.Fingerprint != want.Fingerprint {
		return Lifecycle{}, false, billing.ErrConflict
	}
	life, err := tx.Lifecycle(ctx, lifecycleID)
	if err != nil {
		return Lifecycle{}, false, err
	}
	return copyLifecycle(life), true, nil
}

func supersedePending(ctx context.Context, tx Tx, life Lifecycle, at time.Time) error {
	if life.PendingOperation == "" {
		return nil
	}
	existing, found, err := tx.Change(ctx, life.PendingOperation)
	if err != nil || !found {
		return err
	}
	if existing.State != ChangePlanned {
		return nil
	}
	existing.State = ChangeSuperseded
	return tx.SaveChange(ctx, existing)
}

func copyLifecycle(in Lifecycle) Lifecycle {
	in.Items = slices.Clone(in.Items)
	return in
}
