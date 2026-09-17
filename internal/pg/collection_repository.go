package pg

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/purchase"
)

const collectionBindingJSONLimit = 1 << 20

func (t *purchaseTx) CollectionBinding(ctx context.Context, scope billing.Scope, transactionID string) (purchase.CollectionBinding, error) {
	if !scope.Valid() || !billing.ValidID(transactionID) {
		return purchase.CollectionBinding{}, billing.ErrInvalid
	}
	var raw []byte
	var account, intentID, quoteFingerprint, fingerprint string
	err := t.session.tx.QueryRowContext(ctx, `
		SELECT account_id,intent_id,quote_fingerprint,binding,fingerprint
		FROM billing_purchase_collection_bindings
		WHERE provider=$1 AND merchant=$2 AND environment=$3 AND transaction_id=$4`,
		scope.Provider, scope.Merchant, scope.Environment, transactionID,
	).Scan(&account, &intentID, &quoteFingerprint, &raw, &fingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		return purchase.CollectionBinding{}, billing.ErrNotFound
	}
	if err != nil {
		return purchase.CollectionBinding{}, err
	}
	if billing.AccountID(account) != t.session.account {
		return purchase.CollectionBinding{}, billing.ErrConflict
	}
	if len(raw) > collectionBindingJSONLimit {
		return purchase.CollectionBinding{}, fmt.Errorf("purchase collection binding: %w", billing.ErrInvalid)
	}
	legacy, err := collectionBindingShape(raw)
	if err != nil {
		return purchase.CollectionBinding{}, fmt.Errorf("purchase collection binding shape: %w", err)
	}
	var out purchase.CollectionBinding
	if legacy {
		var old legacyCollectionBinding
		if err := json.Unmarshal(raw, &old); err != nil {
			return purchase.CollectionBinding{}, err
		}
		if old.Account != t.session.account || old.Scope != scope || old.TransactionID != transactionID || old.IntentID != intentID || old.QuoteFingerprint != quoteFingerprint || legacyCollectionFingerprint(old) != fingerprint {
			return purchase.CollectionBinding{}, fmt.Errorf("purchase legacy collection binding integrity: %w", billing.ErrConflict)
		}
		if err := old.validate(); err != nil {
			return purchase.CollectionBinding{}, fmt.Errorf("purchase legacy collection binding: %w", err)
		}
		out = old.current()
	} else {
		if err := json.Unmarshal(raw, &out); err != nil {
			return purchase.CollectionBinding{}, err
		}
		if out.Account != t.session.account || out.Scope != scope || out.TransactionID != transactionID || out.IntentID != intentID || out.QuoteFingerprint != quoteFingerprint || out.Fingerprint() != fingerprint {
			return purchase.CollectionBinding{}, fmt.Errorf("purchase collection binding integrity: %w", billing.ErrConflict)
		}
	}
	if out.Account != t.session.account || out.Scope != scope || out.TransactionID != transactionID || out.IntentID != intentID || out.QuoteFingerprint != quoteFingerprint {
		return purchase.CollectionBinding{}, fmt.Errorf("purchase collection binding integrity: %w", billing.ErrConflict)
	}
	if err := out.Validate(); err != nil {
		return purchase.CollectionBinding{}, fmt.Errorf("purchase collection binding: %w", err)
	}
	return out, nil
}

func (t *purchaseTx) InsertCollectionBinding(ctx context.Context, in purchase.CollectionBinding) error {
	if err := t.session.scope(in.Account); err != nil {
		return err
	}
	if err := in.Validate(); err != nil {
		return err
	}
	intent, err := t.Intent(ctx, in.IntentID)
	if err != nil {
		return err
	}
	if intent.Validate() != nil || intent.Account != in.Account || intent.ID != in.IntentID {
		return billing.ErrState
	}
	if intent.Scope != in.Scope || intent.QuoteFingerprint != in.QuoteFingerprint {
		return billing.ErrConflict
	}
	quote, err := t.Quote(ctx, intent.QuoteID)
	if err != nil {
		return err
	}
	if quote.Validate() != nil || quote.Account != in.Account || quote.ID != intent.QuoteID || quote.Fingerprint() != in.QuoteFingerprint {
		return billing.ErrState
	}
	quantities := make(map[string]int64, len(quote.Lines))
	for _, line := range quote.Lines {
		quantities[line.ID] = line.Quantity
	}
	for _, line := range in.Lines {
		for _, allocation := range line.Allocations {
			if quantities[allocation.QuoteLineID] != allocation.Quantity {
				return billing.ErrConflict
			}
			delete(quantities, allocation.QuoteLineID)
		}
	}
	if len(quantities) != 0 {
		return billing.ErrConflict
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return err
	}
	if len(raw) > collectionBindingJSONLimit {
		return fmt.Errorf("purchase collection binding: %w", billing.ErrInvalid)
	}
	_, err = t.session.tx.ExecContext(ctx, `
		INSERT INTO billing_purchase_collection_bindings(
			account_id,provider,merchant,environment,transaction_id,intent_id,quote_fingerprint,binding,fingerprint
		) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT DO NOTHING`,
		string(in.Account), in.Scope.Provider, in.Scope.Merchant, in.Scope.Environment, in.TransactionID,
		in.IntentID, in.QuoteFingerprint, raw, in.Fingerprint(),
	)
	if err != nil {
		return mapPurchaseLifecycleConflict(err)
	}
	stored, err := t.CollectionBinding(ctx, in.Scope, in.TransactionID)
	if err != nil {
		if errors.Is(err, billing.ErrNotFound) {
			return billing.ErrConflict
		}
		return err
	}
	if stored.Fingerprint() != in.Fingerprint() {
		return billing.ErrConflict
	}
	return nil
}
