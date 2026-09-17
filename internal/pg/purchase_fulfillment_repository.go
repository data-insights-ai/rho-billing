package pg

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/catalog"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/purchase"
	"github.com/data-insights-ai/rho-billing/usage"
)

type fulfillmentScanner interface{ Scan(...any) error }

func scanPurchaseFulfillment(s fulfillmentScanner, out *purchase.Fulfillment, account *string, settlementID *sql.NullString, raw *[]byte, applied, acknowledged *sql.NullTime, immutable, digest *string) error {
	return s.Scan(account, &out.ID, &out.IntentID, &out.LineID, raw, &out.Quantity, settlementID, &out.EffectiveAt, &out.CreatedAt, &out.State, &out.HostReference, applied, acknowledged, immutable, digest)
}

func validateScannedFulfillment(out *purchase.Fulfillment, account string, settlementID *sql.NullString, raw []byte, applied, acknowledged sql.NullTime, immutable, digest string, bound billing.AccountID, id string) error {
	if len(raw) > purchaseJSONLimit {
		return billing.ErrInvalid
	}
	if err := json.Unmarshal(raw, &out.Effect); err != nil {
		return err
	}
	out.Account = billing.AccountID(account)
	if settlementID != nil && settlementID.Valid {
		out.SettlementBatchID = settlementID.String
	}
	if applied.Valid {
		out.AppliedAt = applied.Time
	}
	if acknowledged.Valid {
		out.AcknowledgedAt = acknowledged.Time
	}
	if out.Account != bound || out.ID != id || out.RecordFingerprint() != digest || out.Fingerprint() != immutable {
		return fmt.Errorf("purchase fulfillment integrity: %w", billing.ErrConflict)
	}
	return out.Validate()
}

func (t *purchaseTx) Credits() credit.Repository                  { return t.session.Credits() }
func (t *purchaseTx) Entitlements() catalog.EntitlementRepository { return t.session.Entitlements() }

func (t *purchaseTx) Settlement(ctx context.Context, id string) (usage.BatchSummary, error) {
	if !billing.ValidID(id) {
		return usage.BatchSummary{}, billing.ErrInvalid
	}
	st := &settlementTx{tx: t.session.tx, ctx: ctx, account: t.session.account}
	record, ok, err := st.BatchHeader(id)
	if err != nil {
		return usage.BatchSummary{}, err
	}
	if !ok {
		return usage.BatchSummary{}, billing.ErrNotFound
	}
	return record.Summary, nil
}

func (t *purchaseTx) SettlementFunding(ctx context.Context, batchID string) (purchase.SettlementFunding, error) {
	if !billing.ValidID(batchID) {
		return purchase.SettlementFunding{}, billing.ErrInvalid
	}
	var out purchase.SettlementFunding
	var account string
	var fp string
	err := t.session.tx.QueryRowContext(ctx, `SELECT account_id,batch_id,intent_id,effect_id,scope_provider,scope_merchant,scope_environment,transaction_id,currency,amount,paid_at,fingerprint FROM billing_purchase_settlement_funding WHERE account_id=$1 AND batch_id=$2`, string(t.session.account), batchID).Scan(&account, &out.BatchID, &out.IntentID, &out.EffectID, &out.Scope.Provider, &out.Scope.Merchant, &out.Scope.Environment, &out.TransactionID, &out.Currency, &out.Amount, &out.PaidAt, &fp)
	if errors.Is(err, sql.ErrNoRows) {
		return purchase.SettlementFunding{}, billing.ErrNotFound
	}
	if err != nil {
		return purchase.SettlementFunding{}, err
	}
	out.Account = billing.AccountID(account)
	if out.Account != t.session.account || out.BatchID != batchID || out.Fingerprint() != fp {
		return purchase.SettlementFunding{}, fmt.Errorf("purchase settlement funding integrity: %w", billing.ErrConflict)
	}
	if err := out.Validate(); err != nil {
		return purchase.SettlementFunding{}, err
	}
	return out, nil
}

func (t *purchaseTx) InsertSettlementFunding(ctx context.Context, in purchase.SettlementFunding) error {
	if err := t.session.scope(in.Account); err != nil {
		return err
	}
	if err := in.Validate(); err != nil {
		return err
	}
	_, err := t.session.tx.ExecContext(ctx, `INSERT INTO billing_purchase_settlement_funding(account_id,batch_id,intent_id,effect_id,scope_provider,scope_merchant,scope_environment,transaction_id,currency,amount,paid_at,fingerprint) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) ON CONFLICT DO NOTHING`, string(in.Account), in.BatchID, in.IntentID, in.EffectID, in.Scope.Provider, in.Scope.Merchant, in.Scope.Environment, in.TransactionID, in.Currency, in.Amount, in.PaidAt, in.Fingerprint())
	if err != nil {
		return mapPurchaseLifecycleConflict(err)
	}
	stored, err := t.SettlementFunding(ctx, in.BatchID)
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

func (t *purchaseTx) Fulfillment(ctx context.Context, id string) (purchase.Fulfillment, error) {
	if !billing.ValidID(id) {
		return purchase.Fulfillment{}, billing.ErrInvalid
	}
	var out purchase.Fulfillment
	var account, immutable, digest string
	var settlementID sql.NullString
	var raw []byte
	var applied, acknowledged sql.NullTime
	err := scanPurchaseFulfillment(t.session.tx.QueryRowContext(ctx, `SELECT account_id,fulfillment_id,intent_id,line_id,effect,quantity,settlement_batch_id,effective_at,created_at,state,host_reference,applied_at,acknowledged_at,immutable_fingerprint,record_digest FROM billing_purchase_fulfillments WHERE account_id=$1 AND fulfillment_id=$2`, string(t.session.account), id), &out, &account, &settlementID, &raw, &applied, &acknowledged, &immutable, &digest)
	if errors.Is(err, sql.ErrNoRows) {
		return purchase.Fulfillment{}, billing.ErrNotFound
	}
	if err != nil {
		return purchase.Fulfillment{}, err
	}
	if err := validateScannedFulfillment(&out, account, &settlementID, raw, applied, acknowledged, immutable, digest, t.session.account, id); err != nil {
		return purchase.Fulfillment{}, err
	}
	return out, nil
}

func (t *purchaseTx) Fulfillments(ctx context.Context, intentID string) ([]purchase.Fulfillment, error) {
	if !billing.ValidID(intentID) {
		return nil, billing.ErrInvalid
	}
	rows, err := t.session.tx.QueryContext(ctx, `SELECT account_id,fulfillment_id,intent_id,line_id,effect,quantity,settlement_batch_id,effective_at,created_at,state,host_reference,applied_at,acknowledged_at,immutable_fingerprint,record_digest FROM billing_purchase_fulfillments WHERE account_id=$1 AND intent_id=$2 ORDER BY fulfillment_id LIMIT 1001`, string(t.session.account), intentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]purchase.Fulfillment, 0, 1000)
	for rows.Next() {
		var row purchase.Fulfillment
		var account, immutable, digest string
		var raw []byte
		var applied, acknowledged sql.NullTime
		var settlement sql.NullString
		if err := scanPurchaseFulfillment(rows, &row, &account, &settlement, &raw, &applied, &acknowledged, &immutable, &digest); err != nil {
			return nil, err
		}
		if err := validateScannedFulfillment(&row, account, &settlement, raw, applied, acknowledged, immutable, digest, t.session.account, row.ID); err != nil {
			return nil, err
		}
		if row.IntentID != intentID {
			return nil, billing.ErrConflict
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

func (t *purchaseTx) InsertFulfillment(ctx context.Context, in purchase.Fulfillment) error {
	if err := t.session.scope(in.Account); err != nil {
		return err
	}
	if err := in.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(in.Effect)
	if err != nil || len(raw) > purchaseJSONLimit {
		return billing.ErrInvalid
	}
	_, err = t.session.tx.ExecContext(ctx, `INSERT INTO billing_purchase_fulfillments(account_id,fulfillment_id,intent_id,line_id,effect,quantity,settlement_batch_id,effective_at,created_at,state,host_reference,applied_at,acknowledged_at,immutable_fingerprint,record_digest) VALUES($1,$2,$3,$4,$5,$6,NULLIF($7,''),$8,$9,$10,$11,$12,$13,$14,$15) ON CONFLICT DO NOTHING`, string(in.Account), in.ID, in.IntentID, in.LineID, raw, in.Quantity, in.SettlementBatchID, in.EffectiveAt, in.CreatedAt, in.State, in.HostReference, nullTime(in.AppliedAt), nullTime(in.AcknowledgedAt), in.Fingerprint(), in.RecordFingerprint())
	if err != nil {
		return mapPurchaseLifecycleConflict(err)
	}
	stored, err := t.Fulfillment(ctx, in.ID)
	if errors.Is(err, billing.ErrNotFound) {
		return billing.ErrConflict
	}
	if err != nil {
		return err
	}
	if stored.Fingerprint() != in.Fingerprint() || stored.RecordFingerprint() != in.RecordFingerprint() {
		return billing.ErrConflict
	}
	return nil
}

func (t *purchaseTx) SaveFulfillment(ctx context.Context, in purchase.Fulfillment, expected string) error {
	if err := t.session.scope(in.Account); err != nil {
		return err
	}
	if !billing.ValidID(in.ID) || expected == "" || (in.State != purchase.FulfillmentComplete && in.State != purchase.FulfillmentCanceled) {
		return billing.ErrInvalid
	}
	if err := in.Validate(); err != nil {
		return err
	}
	old, err := t.Fulfillment(ctx, in.ID)
	if err != nil {
		return err
	}
	if old.State != purchase.FulfillmentPending || old.Fingerprint() != expected || old.Fingerprint() != in.Fingerprint() || !old.CreatedAt.Equal(in.CreatedAt) {
		return billing.ErrConflict
	}
	result, err := t.session.tx.ExecContext(ctx, `UPDATE billing_purchase_fulfillments SET state=$3,host_reference=$4,applied_at=$5,acknowledged_at=$6,record_digest=$7 WHERE account_id=$1 AND fulfillment_id=$2 AND state='pending' AND immutable_fingerprint=$8`, string(in.Account), in.ID, in.State, in.HostReference, nullTime(in.AppliedAt), nullTime(in.AcknowledgedAt), in.RecordFingerprint(), expected)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return billing.ErrConflict
	}
	return nil
}
