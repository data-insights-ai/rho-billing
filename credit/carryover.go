package credit

import (
	"context"
	"strconv"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/internal/identity"
)

type CarryoverInput struct {
	Account       billing.AccountID
	Operation     billing.OperationID
	FromLotID     string
	ToLotID       string
	PolicyVersion string
	Cap           int64
	ExpiresAt     time.Time
}

func (e *Engine) Carryover(ctx context.Context, in CarryoverInput) (Result, error) {
	in.ExpiresAt = billing.CanonicalTime(in.ExpiresAt)
	ref := identity.Fingerprint("rollover", in.PolicyVersion, in.FromLotID)
	fp := identity.Fingerprint("carryover", in.FromLotID, in.ToLotID, in.PolicyVersion, strconv.FormatInt(in.Cap, 10), identity.Instant(in.ExpiresAt))
	return e.run(ctx, in.Account, in.Operation, fp, func(s *state) (Result, error) {
		if !billing.ValidID(in.FromLotID) || !billing.ValidID(in.ToLotID) || !billing.ValidID(in.PolicyVersion) || in.Cap <= 0 || in.FromLotID == in.ToLotID {
			return Result{}, billing.ErrInvalid
		}
		if !in.ExpiresAt.IsZero() && !in.ExpiresAt.After(s.now) {
			return Result{}, billing.ErrInvalid
		}
		from, ok, err := s.lot(in.FromLotID)
		if err != nil {
			return Result{}, err
		}
		if !ok {
			return Result{}, billing.ErrNotFound
		}
		if !from.RevokedAt.IsZero() {
			return Result{}, billing.ErrState
		}
		if from.Available <= 0 {
			if expired(from, s.now) {
				return Result{}, billing.ErrExpired
			}
			return Result{}, billing.ErrInsufficient
		}
		if _, ok, err := s.lot(in.ToLotID); err != nil {
			return Result{}, err
		} else if ok {
			return Result{}, billing.ErrConflict
		}
		if _, ok, err := s.sourceLookup(from.Unit.Code, "rollover", ref); err != nil {
			return Result{}, err
		} else if ok {
			return Result{}, billing.ErrConflict
		}
		carry := from.Available
		if carry > in.Cap {
			carry = in.Cap
		}
		if err := s.change(from.ID, "carryover", "", in.PolicyVersion, -from.Available, 0, 0, from.Available, 0, s.now); err != nil {
			return Result{}, err
		}
		to := Lot{
			ID: in.ToLotID, Unit: from.Unit, Scope: from.Scope, Source: "rollover", SourceRef: ref,
			ValidFrom: s.now, ExpiresAt: in.ExpiresAt, GrantedAt: s.now, Initial: carry, Available: carry,
		}
		s.saveLot(to)
		s.entries = append(s.entries, Entry{OperationID: s.op, LotID: to.ID, Kind: "grant", Reason: in.PolicyVersion, RecordedAt: s.now, EffectiveAt: to.ValidFrom, Available: to.Initial})
		out, err := s.result(to.Unit.Code, to.Scope)
		out.LotID = to.ID
		return out, err
	})
}
