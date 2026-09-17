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

func (t *purchaseTx) Adjustment(ctx context.Context, id string) (purchase.AdjustmentRecord, error) {
	if !billing.ValidID(id) {
		return purchase.AdjustmentRecord{}, billing.ErrInvalid
	}
	var account, fp, digest string
	var intentID, providerName, merchant, environment, providerAdjustmentID, transactionID, kind, currency string
	var inputRaw, resultRaw []byte
	var out purchase.AdjustmentRecord
	err := t.session.tx.QueryRowContext(ctx, `SELECT account_id,input,result,created_at,fingerprint,record_digest,intent_id,provider,merchant,environment,provider_adjustment_id,transaction_id,kind,currency FROM billing_purchase_adjustments WHERE account_id=$1 AND adjustment_id=$2`, string(t.session.account), id).Scan(&account, &inputRaw, &resultRaw, &out.CreatedAt, &fp, &digest, &intentID, &providerName, &merchant, &environment, &providerAdjustmentID, &transactionID, &kind, &currency)
	if errors.Is(err, sql.ErrNoRows) {
		return purchase.AdjustmentRecord{}, billing.ErrNotFound
	}
	if err != nil {
		return purchase.AdjustmentRecord{}, err
	}
	if len(inputRaw) > purchaseJSONLimit || len(resultRaw) > purchaseJSONLimit {
		return purchase.AdjustmentRecord{}, billing.ErrInvalid
	}
	if err := json.Unmarshal(inputRaw, &out.Input); err != nil {
		return purchase.AdjustmentRecord{}, err
	}
	if err := json.Unmarshal(resultRaw, &out.Result); err != nil {
		return purchase.AdjustmentRecord{}, err
	}
	if out.Input.Account != t.session.account || out.Input.ID != id || account != string(t.session.account) || out.Input.IntentID != intentID || out.Input.Scope.Provider != providerName || out.Input.Scope.Merchant != merchant || out.Input.Scope.Environment != environment || out.Input.ProviderAdjustmentID != providerAdjustmentID || out.Input.TransactionID != transactionID || string(out.Input.Kind) != kind || out.Input.Currency != currency || out.Input.Fingerprint() != fp || out.Fingerprint() != digest {
		return purchase.AdjustmentRecord{}, fmt.Errorf("purchase adjustment integrity: %w", billing.ErrConflict)
	}
	if err := out.Validate(); err != nil {
		return purchase.AdjustmentRecord{}, err
	}
	return out, nil
}

func (t *purchaseTx) ProviderAdjustment(ctx context.Context, scope billing.Scope, providerID string) (purchase.AdjustmentRecord, error) {
	if !scope.Valid() || !billing.ValidID(providerID) {
		return purchase.AdjustmentRecord{}, billing.ErrInvalid
	}
	var id string
	err := t.session.tx.QueryRowContext(ctx, `SELECT adjustment_id FROM billing_purchase_adjustments WHERE provider=$1 AND merchant=$2 AND environment=$3 AND provider_adjustment_id=$4 AND account_id=$5`, scope.Provider, scope.Merchant, scope.Environment, providerID, string(t.session.account)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		var owner string
		foreign := t.session.tx.QueryRowContext(ctx, `SELECT account_id FROM billing_purchase_adjustments WHERE provider=$1 AND merchant=$2 AND environment=$3 AND provider_adjustment_id=$4`, scope.Provider, scope.Merchant, scope.Environment, providerID).Scan(&owner)
		if foreign == nil && owner != string(t.session.account) {
			return purchase.AdjustmentRecord{}, billing.ErrConflict
		}
		if foreign != nil && !errors.Is(foreign, sql.ErrNoRows) {
			return purchase.AdjustmentRecord{}, foreign
		}
		return purchase.AdjustmentRecord{}, billing.ErrNotFound
	}
	if err != nil {
		return purchase.AdjustmentRecord{}, err
	}
	return t.Adjustment(ctx, id)
}

func (t *purchaseTx) InsertAdjustment(ctx context.Context, in purchase.AdjustmentRecord) error {
	if err := t.session.scope(in.Input.Account); err != nil {
		return err
	}
	if err := in.Validate(); err != nil {
		return err
	}
	inputRaw, err := json.Marshal(in.Input)
	if err != nil || len(inputRaw) > purchaseJSONLimit {
		return billing.ErrInvalid
	}
	resultRaw, err := json.Marshal(in.Result)
	if err != nil || len(resultRaw) > purchaseJSONLimit {
		return billing.ErrInvalid
	}
	_, err = t.session.tx.ExecContext(ctx, `INSERT INTO billing_purchase_adjustments(account_id,adjustment_id,intent_id,provider,merchant,environment,provider_adjustment_id,transaction_id,kind,currency,input,result,created_at,fingerprint,record_digest) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15) ON CONFLICT DO NOTHING`, string(in.Input.Account), in.Input.ID, in.Input.IntentID, in.Input.Scope.Provider, in.Input.Scope.Merchant, in.Input.Scope.Environment, in.Input.ProviderAdjustmentID, in.Input.TransactionID, in.Input.Kind, in.Input.Currency, inputRaw, resultRaw, in.CreatedAt, in.Input.Fingerprint(), in.Fingerprint())
	if err != nil {
		return mapPurchaseLifecycleConflict(err)
	}
	stored, err := t.Adjustment(ctx, in.Input.ID)
	if errors.Is(err, billing.ErrNotFound) {
		return billing.ErrConflict
	}
	if err != nil {
		return err
	}
	if stored.Fingerprint() != in.Fingerprint() {
		return billing.ErrConflict
	}
	return nil
}

func (t *purchaseTx) AdjustmentState(ctx context.Context, intentID string) (purchase.AdjustmentState, error) {
	if !billing.ValidID(intentID) {
		return purchase.AdjustmentState{}, billing.ErrInvalid
	}
	var out purchase.AdjustmentState
	var account, fp string
	var linesRaw, effectsRaw []byte
	err := t.session.tx.QueryRowContext(ctx, `SELECT account_id,intent_id,revision,lines,effects,fingerprint FROM billing_purchase_adjustment_states WHERE account_id=$1 AND intent_id=$2`, string(t.session.account), intentID).Scan(&account, &out.IntentID, &out.Revision, &linesRaw, &effectsRaw, &fp)
	if errors.Is(err, sql.ErrNoRows) {
		return purchase.AdjustmentState{}, billing.ErrNotFound
	}
	if err != nil {
		return purchase.AdjustmentState{}, err
	}
	if len(linesRaw) > purchaseJSONLimit || len(effectsRaw) > purchaseJSONLimit {
		return purchase.AdjustmentState{}, billing.ErrInvalid
	}
	if err := json.Unmarshal(linesRaw, &out.Lines); err != nil {
		return purchase.AdjustmentState{}, err
	}
	if err := json.Unmarshal(effectsRaw, &out.Effects); err != nil {
		return purchase.AdjustmentState{}, err
	}
	out.Account = billing.AccountID(account)
	if out.Account != t.session.account || out.IntentID != intentID || out.Fingerprint() != fp {
		return purchase.AdjustmentState{}, fmt.Errorf("purchase adjustment state integrity: %w", billing.ErrConflict)
	}
	if err := out.Validate(); err != nil {
		return purchase.AdjustmentState{}, err
	}
	if out.Fingerprint() != fp {
		return purchase.AdjustmentState{}, fmt.Errorf("purchase adjustment state digest: %w", billing.ErrConflict)
	}
	return out, nil
}

func (t *purchaseTx) SaveAdjustmentState(ctx context.Context, in purchase.AdjustmentState, expected int64) error {
	if err := t.session.scope(in.Account); err != nil {
		return err
	}
	if err := in.Validate(); err != nil {
		return err
	}
	if expected < 0 || in.Revision != expected+1 {
		return billing.ErrConflict
	}
	linesRaw, err := json.Marshal(in.Lines)
	if err != nil || len(linesRaw) > purchaseJSONLimit {
		return billing.ErrInvalid
	}
	effectsRaw, err := json.Marshal(in.Effects)
	if err != nil || len(effectsRaw) > purchaseJSONLimit {
		return billing.ErrInvalid
	}
	if expected == 0 {
		_, err = t.session.tx.ExecContext(ctx, `INSERT INTO billing_purchase_adjustment_states(account_id,intent_id,revision,lines,effects,fingerprint) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, string(in.Account), in.IntentID, in.Revision, linesRaw, effectsRaw, in.Fingerprint())
		if err != nil {
			return mapPurchaseLifecycleConflict(err)
		}
		got, e := t.AdjustmentState(ctx, in.IntentID)
		if errors.Is(e, billing.ErrNotFound) {
			return billing.ErrConflict
		}
		if e != nil {
			return e
		}
		if got.Fingerprint() != in.Fingerprint() {
			return billing.ErrConflict
		}
		return nil
	}
	res, err := t.session.tx.ExecContext(ctx, `UPDATE billing_purchase_adjustment_states SET revision=$3,lines=$4,effects=$5,fingerprint=$6 WHERE account_id=$1 AND intent_id=$2 AND revision=$7`, string(in.Account), in.IntentID, in.Revision, linesRaw, effectsRaw, in.Fingerprint(), expected)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return billing.ErrConflict
	}
	return nil
}

func (t *purchaseTx) Reversal(ctx context.Context, id string) (purchase.Reversal, error) {
	if !billing.ValidID(id) {
		return purchase.Reversal{}, billing.ErrInvalid
	}
	return t.readReversal(ctx, `account_id=$1 AND reversal_id=$2`, string(t.session.account), id)
}
func (t *purchaseTx) Reversals(ctx context.Context, intentID string) ([]purchase.Reversal, error) {
	if !billing.ValidID(intentID) {
		return nil, billing.ErrInvalid
	}
	rows, err := t.session.tx.QueryContext(ctx, `SELECT account_id,reversal_id,adjustment_id,intent_id,original_effect_id,original,effective_at,created_at,state,host_reference,applied_at,acknowledged_at,immutable_fingerprint,record_digest FROM billing_purchase_reversals WHERE account_id=$1 AND intent_id=$2 ORDER BY reversal_id LIMIT 1001`, string(t.session.account), intentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]purchase.Reversal, 0, 1000)
	for rows.Next() {
		row, err := scanPurchaseReversal(rows, t.session.account)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) > 1000 {
		return nil, billing.ErrInvalid
	}
	return out, nil
}

type reversalScanner interface{ Scan(...any) error }

func scanPurchaseReversal(s reversalScanner, bound billing.AccountID) (purchase.Reversal, error) {
	var out purchase.Reversal
	var account, originalEffectID, immutable, digest string
	var raw []byte
	var applied, acknowledged sql.NullTime
	if err := s.Scan(&account, &out.ID, &out.AdjustmentID, &out.IntentID, &originalEffectID, &raw, &out.EffectiveAt, &out.CreatedAt, &out.State, &out.HostReference, &applied, &acknowledged, &immutable, &digest); err != nil {
		return purchase.Reversal{}, err
	}
	if len(raw) > purchaseJSONLimit {
		return purchase.Reversal{}, billing.ErrInvalid
	}
	if err := json.Unmarshal(raw, &out.Original); err != nil {
		return purchase.Reversal{}, err
	}
	out.Account = billing.AccountID(account)
	if applied.Valid {
		out.AppliedAt = applied.Time
	}
	if acknowledged.Valid {
		out.AcknowledgedAt = acknowledged.Time
	}
	if out.Account != bound || originalEffectID != out.Original.ID || out.RecordFingerprint() != digest || out.Fingerprint() != immutable {
		return purchase.Reversal{}, fmt.Errorf("purchase reversal integrity: %w", billing.ErrConflict)
	}
	if err := out.Validate(); err != nil {
		return purchase.Reversal{}, err
	}
	return out, nil
}

func (t *purchaseTx) readReversal(ctx context.Context, where string, args ...any) (purchase.Reversal, error) {
	row := t.session.tx.QueryRowContext(ctx, `SELECT account_id,reversal_id,adjustment_id,intent_id,original_effect_id,original,effective_at,created_at,state,host_reference,applied_at,acknowledged_at,immutable_fingerprint,record_digest FROM billing_purchase_reversals WHERE `+where, args...)
	out, err := scanPurchaseReversal(row, t.session.account)
	if errors.Is(err, sql.ErrNoRows) {
		return purchase.Reversal{}, billing.ErrNotFound
	}
	return out, err
}

func (t *purchaseTx) InsertReversal(ctx context.Context, in purchase.Reversal) error {
	if err := t.session.scope(in.Account); err != nil {
		return err
	}
	if err := in.Validate(); err != nil {
		return err
	}
	original, err := t.Fulfillment(ctx, in.Original.ID)
	if err != nil {
		return err
	}
	if original.Account != in.Account || original.RecordFingerprint() != in.Original.RecordFingerprint() {
		return billing.ErrConflict
	}
	raw, err := json.Marshal(in.Original)
	if err != nil || len(raw) > purchaseJSONLimit {
		return billing.ErrInvalid
	}
	_, err = t.session.tx.ExecContext(ctx, `INSERT INTO billing_purchase_reversals(account_id,reversal_id,adjustment_id,intent_id,original_effect_id,original,effective_at,created_at,state,host_reference,applied_at,acknowledged_at,immutable_fingerprint,record_digest) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14) ON CONFLICT DO NOTHING`, string(in.Account), in.ID, in.AdjustmentID, in.IntentID, in.Original.ID, raw, in.EffectiveAt, in.CreatedAt, in.State, in.HostReference, nullTime(in.AppliedAt), nullTime(in.AcknowledgedAt), in.Fingerprint(), in.RecordFingerprint())
	if err != nil {
		return mapPurchaseLifecycleConflict(err)
	}
	stored, err := t.Reversal(ctx, in.ID)
	if errors.Is(err, billing.ErrNotFound) {
		return billing.ErrConflict
	}
	if err != nil {
		return err
	}
	if stored.RecordFingerprint() != in.RecordFingerprint() {
		return billing.ErrConflict
	}
	return nil
}
func (t *purchaseTx) SaveReversal(ctx context.Context, in purchase.Reversal, expected string) error {
	if err := t.session.scope(in.Account); err != nil {
		return err
	}
	if err := in.Validate(); err != nil {
		return err
	}
	if in.State != purchase.FulfillmentComplete {
		return billing.ErrInvalid
	}
	old, err := t.Reversal(ctx, in.ID)
	if err != nil {
		return err
	}
	if old.State != purchase.FulfillmentPending || old.Fingerprint() != expected || old.Fingerprint() != in.Fingerprint() || !old.CreatedAt.Equal(in.CreatedAt) {
		return billing.ErrConflict
	}
	res, err := t.session.tx.ExecContext(ctx, `UPDATE billing_purchase_reversals SET state=$3,host_reference=$4,applied_at=$5,acknowledged_at=$6,record_digest=$7 WHERE account_id=$1 AND reversal_id=$2 AND state='pending' AND immutable_fingerprint=$8`, string(in.Account), in.ID, in.State, in.HostReference, nullTime(in.AppliedAt), nullTime(in.AcknowledgedAt), in.RecordFingerprint(), expected)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return billing.ErrConflict
	}
	return nil
}

var _ purchase.AdjustmentTx = (*purchaseTx)(nil)
