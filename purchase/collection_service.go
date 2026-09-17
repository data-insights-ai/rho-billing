package purchase

import (
	"context"
	"errors"

	billing "github.com/data-insights-ai/rho-billing"
)

// BindCollection performs no provider I/O; collection time is checked by ApplyPayment.
func (s *Service) BindCollection(ctx context.Context, in CollectionInput) (CollectionBinding, error) {
	if s == nil || s.repo == nil || s.now == nil {
		return CollectionBinding{}, billing.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return CollectionBinding{}, err
	}
	if err := in.Validate(); err != nil {
		return CollectionBinding{}, err
	}
	now := billing.CanonicalTime(s.now())
	if now.IsZero() {
		return CollectionBinding{}, billing.ErrInvalid
	}
	in.Lines = collectionLines(in.Lines)
	var out CollectionBinding
	err := s.repo.WithinAccount(ctx, in.Account, func(tx Tx) error {
		if old, err := tx.CollectionBinding(ctx, in.Scope, in.TransactionID); err == nil {
			if old.Validate() != nil || old.CollectionInput.Fingerprint() != in.Fingerprint() {
				return billing.ErrConflict
			}
			out = cloneCollectionBinding(old)
			return nil
		} else if !errors.Is(err, billing.ErrNotFound) {
			return err
		}
		intent, err := tx.Intent(ctx, in.IntentID)
		if err != nil {
			return err
		}
		if intent.Validate() != nil || intent.Account != in.Account || intent.ID != in.IntentID {
			return billing.ErrState
		}
		if intent.Scope != in.Scope || intent.QuoteFingerprint != in.QuoteFingerprint {
			return billing.ErrConflict
		}
		if now.Before(intent.CreatedAt) {
			return billing.ErrState
		}
		if intent.Payment == PaymentPaid && intent.TransactionID != in.TransactionID {
			return billing.ErrConflict
		}
		quote, err := tx.Quote(ctx, intent.QuoteID)
		if err != nil {
			return err
		}
		if quote.Validate() != nil || quote.Account != in.Account || quote.ID != intent.QuoteID || quote.Fingerprint() != in.QuoteFingerprint {
			return billing.ErrState
		}
		if !collectionAllocationsMatchQuote(in.Lines, quote.Lines) {
			return billing.ErrConflict
		}
		out = CollectionBinding{CollectionInput: in, CreatedAt: now}
		return tx.InsertCollectionBinding(ctx, out)
	})
	if err != nil {
		return CollectionBinding{}, err
	}
	return out, nil
}

func (s *Service) CollectionBinding(ctx context.Context, account billing.AccountID, scope billing.Scope, transactionID string) (CollectionBinding, error) {
	if s == nil || s.repo == nil || !billing.ValidID(string(account)) || !scope.Valid() || !billing.ValidID(transactionID) {
		return CollectionBinding{}, billing.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return CollectionBinding{}, err
	}
	var out CollectionBinding
	err := s.repo.WithinAccount(ctx, account, func(tx Tx) error {
		value, err := tx.CollectionBinding(ctx, scope, transactionID)
		if err != nil {
			return err
		}
		if value.Validate() != nil || value.Account != account || value.Scope != scope || value.TransactionID != transactionID {
			return billing.ErrState
		}
		out = cloneCollectionBinding(value)
		return nil
	})
	if err != nil {
		return CollectionBinding{}, err
	}
	return out, nil
}

func collectionAllocationsMatchQuote(lines []CollectionLine, quoteLines []QuoteLine) bool {
	quantities := make(map[string]int64, len(quoteLines))
	for _, line := range quoteLines {
		quantities[line.ID] = line.Quantity
	}
	matched := 0
	for _, line := range lines {
		for _, allocation := range line.Allocations {
			quantity, ok := quantities[allocation.QuoteLineID]
			if !ok || quantity != allocation.Quantity {
				return false
			}
			delete(quantities, allocation.QuoteLineID)
			matched++
		}
	}
	return matched == len(quoteLines) && len(quantities) == 0
}
