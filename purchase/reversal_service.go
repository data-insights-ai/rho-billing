package purchase

import (
	"context"

	billing "github.com/data-insights-ai/rho-billing"
)

func (s *Service) Adjustment(ctx context.Context, account billing.AccountID, id string) (AdjustmentRecord, error) {
	if s == nil || s.repo == nil || !billing.ValidID(string(account)) || !billing.ValidID(id) {
		return AdjustmentRecord{}, billing.ErrInvalid
	}
	var out AdjustmentRecord
	err := s.repo.WithinAccount(ctx, account, func(tx Tx) error {
		var err error
		out, err = tx.Adjustment(ctx, id)
		if err != nil {
			return err
		}
		if out.Validate() != nil || out.Input.Account != account || out.Input.ID != id {
			return billing.ErrState
		}
		return nil
	})
	if err != nil {
		return AdjustmentRecord{}, err
	}
	return cloneAdjustmentRecord(out), nil
}

// Reversals: hosts must deduplicate the reversal and tombstone Original.ID
// atomically with undoing the resource; acknowledging alone cannot undo it.
func (s *Service) Reversals(ctx context.Context, account billing.AccountID, intentID string) ([]Reversal, error) {
	if s == nil || s.repo == nil || !billing.ValidID(string(account)) || !billing.ValidID(intentID) {
		return nil, billing.ErrInvalid
	}
	var out []Reversal
	err := s.repo.WithinAccount(ctx, account, func(tx Tx) error {
		rows, err := tx.Reversals(ctx, intentID)
		if err != nil {
			return err
		}
		if len(rows) > maxQuoteEffects {
			return billing.ErrState
		}
		seen := make(map[string]bool, len(rows))
		for _, row := range rows {
			if row.Validate() != nil || row.Account != account || row.IntentID != intentID || seen[row.ID] {
				return billing.ErrState
			}
			seen[row.ID] = true
			out = append(out, cloneReversal(row))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Service) AcknowledgeReversal(ctx context.Context, ack Acknowledgment) (Reversal, error) {
	if s == nil || s.repo == nil || s.now == nil {
		return Reversal{}, billing.ErrInvalid
	}
	ack.AppliedAt = billing.CanonicalTime(ack.AppliedAt)
	if err := ack.Validate(); err != nil {
		return Reversal{}, err
	}
	now := billing.CanonicalTime(s.now())
	if now.IsZero() {
		return Reversal{}, billing.ErrInvalid
	}
	var out Reversal
	err := s.repo.WithinAccount(ctx, ack.Account, func(tx Tx) error {
		old, err := tx.Reversal(ctx, ack.EffectID)
		if err != nil {
			return err
		}
		if old.Validate() != nil || old.Account != ack.Account || old.ID != ack.EffectID || old.Fingerprint() != ack.Fingerprint {
			return billing.ErrConflict
		}
		if old.State == FulfillmentComplete {
			if old.HostReference != ack.HostReference || !old.AppliedAt.Equal(ack.AppliedAt) {
				return billing.ErrConflict
			}
			out = old
			return nil
		}
		if old.State != FulfillmentPending || ack.AppliedAt.Before(old.EffectiveAt) || ack.AppliedAt.After(now) || now.Before(old.CreatedAt) {
			return billing.ErrInvalid
		}
		old.State = FulfillmentComplete
		old.HostReference = ack.HostReference
		old.AppliedAt = ack.AppliedAt
		old.AcknowledgedAt = now
		if err := tx.SaveReversal(ctx, old, ack.Fingerprint); err != nil {
			return err
		}
		out = old
		return nil
	})
	if err != nil {
		return Reversal{}, err
	}
	return cloneReversal(out), nil
}
