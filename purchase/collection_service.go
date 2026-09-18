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

// RekeyCollectionLines replaces the provider line ids of a bound transaction.
//
// Some providers re-issue line ids whenever they recompute a transaction (an
// address arrives, tax is applied) while the lines themselves, price, quantity
// and what they pay for, stay what the host bound. The paid transaction is then
// the authoritative evidence for the ids that refunds and adjustments will
// reference later. This accepts exactly that: the same multiset of line
// contents under new ids. Any change to price, quantity or allocation is a
// different purchase and is refused as a conflict. Replaying the current ids
// is a no-op.
func (s *Service) RekeyCollectionLines(ctx context.Context, in RekeyInput) (CollectionBinding, error) {
	if s == nil || s.repo == nil || s.now == nil {
		return CollectionBinding{}, billing.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return CollectionBinding{}, err
	}
	if err := in.Validate(); err != nil {
		return CollectionBinding{}, err
	}
	var out CollectionBinding
	err := s.repo.WithinAccount(ctx, in.Account, func(tx Tx) error {
		old, err := tx.CollectionBinding(ctx, in.Scope, in.TransactionID)
		if err != nil {
			return err
		}
		if old.Validate() != nil || old.Account != in.Account {
			return billing.ErrState
		}
		if !sameLineContents(old.Lines, in.Lines) {
			return billing.ErrConflict
		}
		next := cloneCollectionBinding(old)
		next.Lines = collectionLines(in.Lines)
		if next.Fingerprint() == old.Fingerprint() {
			out = next
			return nil
		}
		if err := tx.ReplaceCollectionBinding(ctx, next); err != nil {
			return err
		}
		out = next
		return nil
	})
	if err != nil {
		return CollectionBinding{}, err
	}
	return out, nil
}

// sameLineContents reports whether two line sets pay for the same things in
// the same quantities, ignoring provider line ids.
func sameLineContents(a, b []CollectionLine) bool {
	if len(a) != len(b) {
		return false
	}
	counts := make(map[string]int, len(a))
	for _, line := range a {
		counts[lineContent(line)]++
	}
	for _, line := range b {
		key := lineContent(line)
		if counts[key] == 0 {
			return false
		}
		counts[key]--
	}
	return true
}
