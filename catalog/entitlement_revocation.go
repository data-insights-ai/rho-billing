package catalog

import (
	"context"
	"errors"
	"strings"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/internal/identity"
)

type Revocation struct {
	Account                                billing.AccountID
	AssignmentID, SourceRef, Actor, Reason string
	EffectiveAt, CreatedAt                 time.Time
	// AssignmentStart selects which interval of a reused assignment ID this
	// revocation targets. An assignment ID may legitimately repeat across
	// adjacent, non-overlapping intervals, and without this the revocation is
	// ambiguous and rejected. It may be left zero when the ID occurs once.
	AssignmentStart time.Time
}

func (r Revocation) Validate() error {
	if !billing.ValidID(string(r.Account)) || !billing.ValidID(r.AssignmentID) || !billing.ValidID(r.SourceRef) || !billing.ValidID(r.Actor) || strings.TrimSpace(r.Reason) == "" || len(r.Reason) > 2000 || r.EffectiveAt.IsZero() || r.CreatedAt.IsZero() {
		return billing.ErrInvalid
	}
	if _, err := r.EffectiveAt.MarshalJSON(); err != nil {
		return billing.ErrInvalid
	}
	if _, err := r.CreatedAt.MarshalJSON(); err != nil {
		return billing.ErrInvalid
	}
	return nil
}

// Fingerprint excludes receipt time so exact retries retain the original receipt.
func (r Revocation) Fingerprint() string {
	return identity.Fingerprint("entitlement-revocation", string(r.Account), r.AssignmentID, r.SourceRef, r.Actor, r.Reason, identity.Instant(r.EffectiveAt))
}

func (s *EntitlementService) Revoke(ctx context.Context, in Revocation) (Revocation, error) {
	if err := ctx.Err(); err != nil {
		return Revocation{}, err
	}
	if s == nil || s.repo == nil || s.now == nil {
		return Revocation{}, billing.ErrInvalid
	}
	in.EffectiveAt = billing.CanonicalTime(in.EffectiveAt)
	in.CreatedAt = billing.CanonicalTime(s.now())
	if in.CreatedAt.IsZero() {
		return Revocation{}, billing.ErrInvalid
	}
	if err := in.Validate(); err != nil {
		return Revocation{}, err
	}
	var out Revocation
	err := s.repo.WithinAccount(ctx, in.Account, func(tx EntitlementTx) error {
		assignment, err := tx.Assignment(ctx, in.AssignmentID)
		if err != nil {
			return err
		}
		if assignment.Account != in.Account || assignment.Plan.ID != in.AssignmentID {
			return billing.ErrState
		}
		if err := assignment.Validate(); err != nil {
			return billing.ErrState
		}
		if in.EffectiveAt.Before(assignment.Plan.Effective.Start) {
			return billing.ErrInvalid
		}
		old, err := tx.Revocation(ctx, in.AssignmentID)
		if err == nil {
			if old.Validate() != nil {
				return billing.ErrState
			}
			if old.Account != in.Account || old.AssignmentID != in.AssignmentID || old.Fingerprint() != in.Fingerprint() {
				return billing.ErrConflict
			}
			out = old
			return nil
		}
		if !errors.Is(err, billing.ErrNotFound) {
			return err
		}
		if err := tx.InsertRevocation(ctx, in); err != nil {
			return err
		}
		out = in
		return nil
	})
	if err != nil {
		return Revocation{}, err
	}
	return out, nil
}

func (s *EntitlementService) Revocation(ctx context.Context, account billing.AccountID, assignmentID string) (Revocation, error) {
	if err := ctx.Err(); err != nil {
		return Revocation{}, err
	}
	if s == nil || s.repo == nil || !billing.ValidID(string(account)) || !billing.ValidID(assignmentID) {
		return Revocation{}, billing.ErrInvalid
	}
	var out Revocation
	err := s.repo.WithinAccount(ctx, account, func(tx EntitlementTx) error {
		value, err := tx.Revocation(ctx, assignmentID)
		if err != nil {
			return err
		}
		if err := value.Validate(); err != nil || value.Account != account || value.AssignmentID != assignmentID {
			return billing.ErrState
		}
		out = value
		return nil
	})
	if err != nil {
		return Revocation{}, err
	}
	return out, nil
}
