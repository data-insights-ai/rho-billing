package purchase

import (
	"cmp"
	"context"
	"math"
	"slices"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
)

type CollectionLine struct {
	ProviderLineID, ProviderPriceID string
	Quantity                        int64
	Allocations                     []CollectionAllocation
}

type CollectionAllocation struct {
	QuoteLineID string
	Quantity    int64
}

// CollectionInput does not assert payment or grant benefits; never infer the
// association from an amount alone.
type CollectionInput struct {
	Account                                               billing.AccountID
	Scope                                                 billing.Scope
	TransactionID, IntentID, QuoteFingerprint, CustomerID string
	Lines                                                 []CollectionLine
	Actor, Reason, EvidenceReference                      string
}

type CollectionBinding struct {
	CollectionInput
	CreatedAt time.Time
}

type CollectionTx interface {
	CollectionBinding(context.Context, billing.Scope, string) (CollectionBinding, error)
	InsertCollectionBinding(context.Context, CollectionBinding) error
}

func (in CollectionInput) Validate() error {
	if !billing.ValidID(string(in.Account)) || !in.Scope.Valid() || !billing.ValidID(in.TransactionID) || !billing.ValidID(in.IntentID) || !validDigest(in.QuoteFingerprint) || !billing.ValidID(in.Actor) || !billing.ValidID(in.Reason) || !billing.ValidID(in.EvidenceReference) || len(in.Lines) == 0 || len(in.Lines) > 100 {
		return billing.ErrInvalid
	}
	if in.CustomerID != "" && !billing.ValidID(in.CustomerID) {
		return billing.ErrInvalid
	}
	providerIDs, quoteIDs := make(map[string]bool, len(in.Lines)), make(map[string]bool, len(in.Lines))
	for _, line := range in.Lines {
		if !billing.ValidID(line.ProviderLineID) || (line.ProviderPriceID != "" && !billing.ValidID(line.ProviderPriceID)) || line.Quantity <= 0 || len(line.Allocations) == 0 || len(line.Allocations) > 100 {
			return billing.ErrInvalid
		}
		if providerIDs[line.ProviderLineID] {
			return billing.ErrConflict
		}
		providerIDs[line.ProviderLineID] = true
		var total int64
		for _, allocation := range line.Allocations {
			if !billing.ValidID(allocation.QuoteLineID) || allocation.Quantity <= 0 {
				return billing.ErrInvalid
			}
			if quoteIDs[allocation.QuoteLineID] {
				return billing.ErrConflict
			}
			quoteIDs[allocation.QuoteLineID] = true
			if total > math.MaxInt64-allocation.Quantity {
				return billing.ErrOverflow
			}
			total += allocation.Quantity
		}
		if total != line.Quantity || len(quoteIDs) > 100 {
			return billing.ErrConflict
		}
	}
	return nil
}

func (b CollectionBinding) Validate() error {
	if err := b.CollectionInput.Validate(); err != nil {
		return err
	}
	if b.CreatedAt.IsZero() {
		return billing.ErrInvalid
	}
	if _, err := b.CreatedAt.MarshalJSON(); err != nil {
		return billing.ErrInvalid
	}
	return nil
}

func collectionLines(lines []CollectionLine) []CollectionLine {
	out := slices.Clone(lines)
	for i := range out {
		out[i].Allocations = slices.Clone(out[i].Allocations)
		slices.SortFunc(out[i].Allocations, func(a, b CollectionAllocation) int { return cmp.Compare(a.QuoteLineID, b.QuoteLineID) })
	}
	slices.SortFunc(out, func(a, b CollectionLine) int { return cmp.Compare(a.ProviderLineID, b.ProviderLineID) })
	return out
}

func (in CollectionInput) Fingerprint() string {
	in.Lines = collectionLines(in.Lines)
	return digest(in)
}

func (b CollectionBinding) Fingerprint() string {
	b.Lines = collectionLines(b.Lines)
	b.CreatedAt = billing.CanonicalTime(b.CreatedAt)
	return digest(b)
}
