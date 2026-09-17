package purchase

import (
	"context"

	billing "github.com/data-insights-ai/rho-billing"
)

func cloneCollectionBinding(in CollectionBinding) CollectionBinding {
	in.Lines = collectionLines(in.Lines)
	return in
}

func (t *memoryTx) CollectionBinding(ctx context.Context, scope billing.Scope, transactionID string) (CollectionBinding, error) {
	if err := ctx.Err(); err != nil {
		return CollectionBinding{}, err
	}
	if err := t.lifecycleReady(); err != nil {
		return CollectionBinding{}, err
	}
	if !scope.Valid() || !billing.ValidID(transactionID) {
		return CollectionBinding{}, billing.ErrInvalid
	}
	binding, ok := t.lifecycle.collections[lifecycleCollectionKey{scope: scope, transactionID: transactionID}]
	if !ok {
		return CollectionBinding{}, billing.ErrNotFound
	}
	if binding.Account != t.account {
		return CollectionBinding{}, billing.ErrConflict
	}
	return cloneCollectionBinding(binding), nil
}

func (t *memoryTx) InsertCollectionBinding(ctx context.Context, binding CollectionBinding) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := t.lifecycleReady(); err != nil {
		return err
	}
	if err := binding.Validate(); err != nil {
		return err
	}
	if binding.Account != t.account {
		return billing.ErrConflict
	}
	intent, ok := t.lifecycle.intents[lifecycleIntentKey{account: binding.Account, id: binding.IntentID}]
	if !ok {
		return billing.ErrNotFound
	}
	if intent.Account != binding.Account || intent.Scope != binding.Scope || intent.QuoteFingerprint != binding.QuoteFingerprint {
		return billing.ErrConflict
	}
	quote, ok := t.state.quotes[intent.QuoteID]
	if !ok {
		return billing.ErrNotFound
	}
	if quote.Account != binding.Account || quote.Fingerprint() != binding.QuoteFingerprint || !collectionAllocationsMatchQuote(binding.Lines, quote.Lines) {
		return billing.ErrConflict
	}
	key := lifecycleCollectionKey{scope: binding.Scope, transactionID: binding.TransactionID}
	if old, ok := t.lifecycle.collections[key]; ok {
		if old.Account != binding.Account {
			return billing.ErrConflict
		}
		if old.Fingerprint() == binding.Fingerprint() {
			return nil
		}
		return billing.ErrConflict
	}
	t.lifecycle.collections[key] = cloneCollectionBinding(binding)
	return nil
}

var _ CollectionTx = (*memoryTx)(nil)
