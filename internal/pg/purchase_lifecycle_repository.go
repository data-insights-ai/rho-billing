package pg

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/purchase"
)

func (t *purchaseTx) Intent(ctx context.Context, id string) (purchase.Intent, error) {
	if !billing.ValidID(id) {
		return purchase.Intent{}, billing.ErrInvalid
	}
	var out purchase.Intent
	var account string
	var fingerprint string
	var projectionFingerprint string
	var paidAt, lastPaymentAt sql.NullTime
	err := t.session.tx.QueryRowContext(ctx, `SELECT account_id,intent_id,operation,quote_id,quote_fingerprint,provider,merchant,environment,actor,reason,expires_at,currency,tax_treatment,amount,command,payment,fulfillment,revision,created_at,updated_at,transaction_id,paid_at,last_payment_at,last_payment_event_id,projection_fingerprint,fingerprint FROM billing_purchase_intents WHERE account_id=$1 AND intent_id=$2`, string(t.session.account), id).Scan(&account, &out.ID, &out.Operation, &out.QuoteID, &out.QuoteFingerprint, &out.Scope.Provider, &out.Scope.Merchant, &out.Scope.Environment, &out.Actor, &out.Reason, &out.ExpiresAt, &out.Currency, &out.TaxTreatment, &out.Amount, &out.Command, &out.Payment, &out.Fulfillment, &out.Revision, &out.CreatedAt, &out.UpdatedAt, &out.TransactionID, &paidAt, &lastPaymentAt, &out.LastPaymentEventID, &projectionFingerprint, &fingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		return purchase.Intent{}, billing.ErrNotFound
	}
	if err != nil {
		return purchase.Intent{}, err
	}
	out.Account = billing.AccountID(account)
	if paidAt.Valid {
		out.PaidAt = paidAt.Time
	}
	if lastPaymentAt.Valid {
		out.LastPaymentAt = lastPaymentAt.Time
	}
	if out.Account != t.session.account || out.ID != id || out.Fingerprint() != fingerprint || intentProjectionFingerprint(out) != projectionFingerprint {
		return purchase.Intent{}, fmt.Errorf("purchase intent integrity: %w", billing.ErrConflict)
	}
	if err := out.Validate(); err != nil {
		return purchase.Intent{}, fmt.Errorf("purchase intent: %w", err)
	}
	return out, nil
}

func (t *purchaseTx) IntentByOperation(ctx context.Context, operation string) (purchase.Intent, error) {
	if !billing.ValidID(operation) {
		return purchase.Intent{}, billing.ErrInvalid
	}
	var id string
	err := t.session.tx.QueryRowContext(ctx, `SELECT intent_id FROM billing_purchase_intents WHERE account_id=$1 AND operation=$2`, string(t.session.account), operation).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return purchase.Intent{}, billing.ErrNotFound
	}
	if err != nil {
		return purchase.Intent{}, err
	}
	return t.Intent(ctx, id)
}

func (t *purchaseTx) InsertIntent(ctx context.Context, in purchase.Intent) error {
	if err := t.session.scope(in.Account); err != nil {
		return err
	}
	if err := in.Validate(); err != nil {
		return err
	}
	_, err := t.session.tx.ExecContext(ctx, `INSERT INTO billing_purchase_intents(account_id,intent_id,operation,quote_id,quote_fingerprint,provider,merchant,environment,actor,reason,expires_at,currency,tax_treatment,amount,command,payment,fulfillment,revision,created_at,updated_at,transaction_id,paid_at,last_payment_at,last_payment_event_id,projection_fingerprint,fingerprint) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26) ON CONFLICT DO NOTHING`, string(in.Account), in.ID, in.Operation, in.QuoteID, in.QuoteFingerprint, in.Scope.Provider, in.Scope.Merchant, in.Scope.Environment, in.Actor, in.Reason, in.ExpiresAt, in.Currency, in.TaxTreatment, in.Amount, in.Command, in.Payment, in.Fulfillment, in.Revision, in.CreatedAt, in.UpdatedAt, in.TransactionID, nullTime(in.PaidAt), nullTime(in.LastPaymentAt), in.LastPaymentEventID, intentProjectionFingerprint(in), in.Fingerprint())
	if err != nil {
		return mapPurchaseLifecycleConflict(err)
	}
	stored, err := t.Intent(ctx, in.ID)
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

func (t *purchaseTx) SaveIntent(ctx context.Context, in purchase.Intent, expected int64) error {
	if err := t.session.scope(in.Account); err != nil {
		return err
	}
	if expected < 0 || in.Revision != expected+1 {
		return billing.ErrConflict
	}
	if err := in.Validate(); err != nil {
		return err
	}
	old, err := t.Intent(ctx, in.ID)
	if err != nil {
		return err
	}
	if old.Revision != expected || old.Fingerprint() != in.Fingerprint() || !old.CreatedAt.Equal(in.CreatedAt) {
		return billing.ErrConflict
	}
	result, err := t.session.tx.ExecContext(ctx, `UPDATE billing_purchase_intents SET command=$3,payment=$4,fulfillment=$5,revision=$6,updated_at=$7,transaction_id=$8,paid_at=$9,last_payment_at=$10,last_payment_event_id=$11,projection_fingerprint=$12 WHERE account_id=$1 AND intent_id=$2 AND revision=$13 AND fingerprint=$14 AND projection_fingerprint=$15`, string(in.Account), in.ID, in.Command, in.Payment, in.Fulfillment, in.Revision, in.UpdatedAt, in.TransactionID, nullTime(in.PaidAt), nullTime(in.LastPaymentAt), in.LastPaymentEventID, intentProjectionFingerprint(in), expected, in.Fingerprint(), intentProjectionFingerprint(old))
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return billing.ErrConflict
	}
	return nil
}

func (t *purchaseTx) Command(ctx context.Context, operation string) (purchase.CommandRecord, error) {
	if !billing.ValidID(operation) {
		return purchase.CommandRecord{}, billing.ErrInvalid
	}
	var inputRaw, resultRaw []byte
	var out purchase.CommandRecord
	var fingerprint string
	var intentID string
	err := t.session.tx.QueryRowContext(ctx, `SELECT intent_id,input,result,fingerprint FROM billing_purchase_commands WHERE account_id=$1 AND operation=$2`, string(t.session.account), operation).Scan(&intentID, &inputRaw, &resultRaw, &fingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		return purchase.CommandRecord{}, billing.ErrNotFound
	}
	if err != nil {
		return purchase.CommandRecord{}, err
	}
	if len(inputRaw) > purchaseJSONLimit || len(resultRaw) > purchaseJSONLimit {
		return purchase.CommandRecord{}, billing.ErrInvalid
	}
	if err := json.Unmarshal(inputRaw, &out.Input); err != nil {
		return purchase.CommandRecord{}, err
	}
	if err := json.Unmarshal(resultRaw, &out.Result); err != nil {
		return purchase.CommandRecord{}, err
	}
	if out.Input.Account != t.session.account || out.Input.Operation != operation || out.Input.IntentID != intentID || out.Result.Account != t.session.account || out.Result.ID != out.Input.IntentID || out.Fingerprint() != fingerprint {
		return purchase.CommandRecord{}, fmt.Errorf("purchase command integrity: %w", billing.ErrConflict)
	}
	if err := out.Validate(); err != nil {
		return purchase.CommandRecord{}, err
	}
	return out, nil
}

func (t *purchaseTx) InsertCommand(ctx context.Context, in purchase.CommandRecord) error {
	if err := t.session.scope(in.Input.Account); err != nil {
		return err
	}
	if err := in.Validate(); err != nil {
		return err
	}
	inputRaw, err := json.Marshal(in.Input)
	if err != nil {
		return err
	}
	resultRaw, err := json.Marshal(in.Result)
	if err != nil {
		return err
	}
	if len(inputRaw) > purchaseJSONLimit || len(resultRaw) > purchaseJSONLimit {
		return billing.ErrInvalid
	}
	_, err = t.session.tx.ExecContext(ctx, `INSERT INTO billing_purchase_commands(account_id,operation,intent_id,input,result,fingerprint) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, string(in.Input.Account), in.Input.Operation, in.Input.IntentID, inputRaw, resultRaw, in.Fingerprint())
	if err != nil {
		return mapPurchaseLifecycleConflict(err)
	}
	stored, err := t.Command(ctx, in.Input.Operation)
	if err != nil {
		return err
	}
	if stored.Fingerprint() != in.Fingerprint() {
		return billing.ErrConflict
	}
	return nil
}

func (t *purchaseTx) PaymentEvent(ctx context.Context, scope billing.Scope, eventID string) (purchase.PaymentRecord, error) {
	if !scope.Valid() || !billing.ValidID(eventID) {
		return purchase.PaymentRecord{}, billing.ErrInvalid
	}
	var factRaw, resultRaw []byte
	var out purchase.PaymentRecord
	var fingerprint string
	var intentID string
	err := t.session.tx.QueryRowContext(ctx, `SELECT intent_id,fact,result,fingerprint FROM billing_purchase_payment_events WHERE account_id=$1 AND provider=$2 AND merchant=$3 AND environment=$4 AND event_id=$5`, string(t.session.account), scope.Provider, scope.Merchant, scope.Environment, eventID).Scan(&intentID, &factRaw, &resultRaw, &fingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		var owner string
		foreignErr := t.session.tx.QueryRowContext(ctx, `SELECT account_id FROM billing_purchase_payment_events WHERE provider=$1 AND merchant=$2 AND environment=$3 AND event_id=$4`, scope.Provider, scope.Merchant, scope.Environment, eventID).Scan(&owner)
		if foreignErr == nil && owner != string(t.session.account) {
			return purchase.PaymentRecord{}, billing.ErrConflict
		}
		if !errors.Is(foreignErr, sql.ErrNoRows) && foreignErr != nil {
			return purchase.PaymentRecord{}, foreignErr
		}
		return purchase.PaymentRecord{}, billing.ErrNotFound
	}
	if err != nil {
		return purchase.PaymentRecord{}, err
	}
	if len(factRaw) > purchaseJSONLimit || len(resultRaw) > purchaseJSONLimit {
		return purchase.PaymentRecord{}, billing.ErrInvalid
	}
	if err := json.Unmarshal(factRaw, &out.Fact); err != nil {
		return purchase.PaymentRecord{}, err
	}
	if err := json.Unmarshal(resultRaw, &out.Result); err != nil {
		return purchase.PaymentRecord{}, err
	}
	if out.Fact.Account != t.session.account || out.Fact.Scope != scope || out.Fact.EventID != eventID || out.Fact.IntentID != intentID || out.Result.Account != t.session.account || out.Result.EventID != eventID || out.Result.IntentID != intentID || out.Fingerprint() != fingerprint {
		return purchase.PaymentRecord{}, fmt.Errorf("purchase payment event integrity: %w", billing.ErrConflict)
	}
	if err := out.Validate(); err != nil {
		return purchase.PaymentRecord{}, err
	}
	return out, nil
}

func (t *purchaseTx) InsertPaymentEvent(ctx context.Context, in purchase.PaymentRecord) error {
	if err := t.session.scope(in.Fact.Account); err != nil {
		return err
	}
	if err := in.Validate(); err != nil {
		return err
	}
	factRaw, err := json.Marshal(in.Fact)
	if err != nil {
		return err
	}
	resultRaw, err := json.Marshal(in.Result)
	if err != nil {
		return err
	}
	if len(factRaw) > purchaseJSONLimit || len(resultRaw) > purchaseJSONLimit {
		return billing.ErrInvalid
	}
	_, err = t.session.tx.ExecContext(ctx, `INSERT INTO billing_purchase_payment_events(account_id,provider,merchant,environment,event_id,intent_id,fact,result,fingerprint) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT DO NOTHING`, string(in.Fact.Account), in.Fact.Scope.Provider, in.Fact.Scope.Merchant, in.Fact.Scope.Environment, in.Fact.EventID, in.Fact.IntentID, factRaw, resultRaw, in.Fingerprint())
	if err != nil {
		return mapPurchaseLifecycleConflict(err)
	}
	stored, err := t.PaymentEvent(ctx, in.Fact.Scope, in.Fact.EventID)
	if err != nil {
		return err
	}
	if stored.Fingerprint() != in.Fingerprint() {
		return billing.ErrConflict
	}
	return nil
}

func (t *purchaseTx) Funding(ctx context.Context, scope billing.Scope, transactionID string) (purchase.Funding, error) {
	if !scope.Valid() || !billing.ValidID(transactionID) {
		return purchase.Funding{}, billing.ErrInvalid
	}
	var out purchase.Funding
	var raw []byte
	var fingerprint string
	err := t.session.tx.QueryRowContext(ctx, `SELECT account_id,transaction_id,intent_id,currency,gross,tax,discount,paid_at,lines,fingerprint FROM billing_purchase_funding WHERE account_id=$1 AND provider=$2 AND merchant=$3 AND environment=$4 AND transaction_id=$5`, string(t.session.account), scope.Provider, scope.Merchant, scope.Environment, transactionID).Scan(&out.Account, &out.TransactionID, &out.IntentID, &out.Currency, &out.Gross, &out.Tax, &out.Discount, &out.PaidAt, &raw, &fingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		var owner string
		foreignErr := t.session.tx.QueryRowContext(ctx, `SELECT account_id FROM billing_purchase_funding WHERE provider=$1 AND merchant=$2 AND environment=$3 AND transaction_id=$4`, scope.Provider, scope.Merchant, scope.Environment, transactionID).Scan(&owner)
		if foreignErr == nil && owner != string(t.session.account) {
			return purchase.Funding{}, billing.ErrConflict
		}
		if !errors.Is(foreignErr, sql.ErrNoRows) && foreignErr != nil {
			return purchase.Funding{}, foreignErr
		}
		return purchase.Funding{}, billing.ErrNotFound
	}
	if err != nil {
		return purchase.Funding{}, err
	}
	if len(raw) > purchaseJSONLimit {
		return purchase.Funding{}, billing.ErrInvalid
	}
	if err := json.Unmarshal(raw, &out.Lines); err != nil {
		return purchase.Funding{}, err
	}
	out.Scope = scope
	if out.Fingerprint() != fingerprint {
		return purchase.Funding{}, fmt.Errorf("purchase funding integrity: %w", billing.ErrConflict)
	}
	if err := out.Validate(); err != nil {
		return purchase.Funding{}, err
	}
	return out, nil
}

func intentProjectionFingerprint(in purchase.Intent) string {
	in.ExpiresAt = billing.CanonicalTime(in.ExpiresAt)
	in.CreatedAt = billing.CanonicalTime(in.CreatedAt)
	in.UpdatedAt = billing.CanonicalTime(in.UpdatedAt)
	in.PaidAt = billing.CanonicalTime(in.PaidAt)
	in.LastPaymentAt = billing.CanonicalTime(in.LastPaymentAt)
	b, _ := json.Marshal(in)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (t *purchaseTx) InsertFunding(ctx context.Context, in purchase.Funding) error {
	if err := t.session.scope(in.Account); err != nil {
		return err
	}
	if err := in.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(in.Lines)
	if err != nil {
		return err
	}
	if len(raw) > purchaseJSONLimit {
		return billing.ErrInvalid
	}
	_, err = t.session.tx.ExecContext(ctx, `INSERT INTO billing_purchase_funding(account_id,provider,merchant,environment,transaction_id,intent_id,currency,gross,tax,discount,paid_at,lines,fingerprint) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) ON CONFLICT DO NOTHING`, string(in.Account), in.Scope.Provider, in.Scope.Merchant, in.Scope.Environment, in.TransactionID, in.IntentID, in.Currency, in.Gross, in.Tax, in.Discount, in.PaidAt, raw, in.Fingerprint())
	if err != nil {
		return mapPurchaseLifecycleConflict(err)
	}
	stored, err := t.Funding(ctx, in.Scope, in.TransactionID)
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

func mapPurchaseLifecycleConflict(err error) error {
	if err == nil {
		return nil
	}
	if pgerr, ok := errors.AsType[*pgconn.PgError](err); ok && pgerr.Code == "23505" {
		return billing.ErrConflict
	}
	return err
}
