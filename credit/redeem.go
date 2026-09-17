package credit

import (
	"context"
	"strconv"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/internal/identity"
)

type RedeemInput struct {
	Account       billing.AccountID
	Operation     billing.OperationID
	PromotionID   string
	RedemptionKey string
	Amount        int64
	Ceiling       int64
	Eligible      bool
	Member        string
	Unit          billing.Unit
	ValidFrom     time.Time
	ExpiresAt     time.Time
}

func (e *Engine) Redeem(ctx context.Context, in RedeemInput) (Result, error) {
	in.ValidFrom = billing.CanonicalTime(in.ValidFrom)
	in.ExpiresAt = billing.CanonicalTime(in.ExpiresAt)
	lotID := identity.Fingerprint("redeem-lot", in.PromotionID, in.RedemptionKey)
	fp := identity.Fingerprint("redeem", in.PromotionID, in.RedemptionKey, strconv.FormatInt(in.Amount, 10), strconv.FormatInt(in.Ceiling, 10), strconv.FormatBool(in.Eligible), in.Member, in.Unit.Code, strconv.FormatInt(in.Unit.Scale, 10), identity.Instant(in.ValidFrom), identity.Instant(in.ExpiresAt))
	return e.run(ctx, in.Account, in.Operation, fp, func(s *state) (Result, error) {
		if !billing.ValidID(in.PromotionID) || !billing.ValidID(in.RedemptionKey) || !in.Unit.Valid() || in.Amount <= 0 || in.Ceiling <= 0 || in.ValidFrom.IsZero() || (!in.ExpiresAt.IsZero() && !in.ExpiresAt.After(in.ValidFrom)) {
			return Result{}, billing.ErrInvalid
		}
		if in.Member != "" && !billing.ValidID(in.Member) {
			return Result{}, billing.ErrInvalid
		}
		if !in.Eligible {
			return Result{}, billing.ErrInvalid
		}
		if in.Amount > in.Ceiling {
			return Result{}, billing.ErrLimit
		}
		if _, ok, err := s.lot(lotID); err != nil {
			return Result{}, err
		} else if ok {
			return Result{}, billing.ErrConflict
		}
		if _, ok, err := s.sourceLookup(in.Unit.Code, in.PromotionID, in.RedemptionKey); err != nil {
			return Result{}, err
		} else if ok {
			return Result{}, billing.ErrConflict
		}
		if s.ensureUnit != nil {
			if err := s.ensureUnit(in.Unit); err != nil {
				return Result{}, err
			}
		}
		l := Lot{
			ID: lotID, Unit: in.Unit, Scope: in.Member, Source: in.PromotionID, SourceRef: in.RedemptionKey,
			ValidFrom: in.ValidFrom.UTC(), ExpiresAt: in.ExpiresAt.UTC(), GrantedAt: s.now, Initial: in.Amount, Available: in.Amount,
		}
		s.saveLot(l)
		s.entries = append(s.entries, Entry{OperationID: s.op, LotID: l.ID, Kind: "grant", Reason: "promotion", RecordedAt: s.now, EffectiveAt: l.ValidFrom, Available: l.Initial})
		out, err := s.result(l.Unit.Code, l.Scope)
		out.LotID = l.ID
		return out, err
	})
}
