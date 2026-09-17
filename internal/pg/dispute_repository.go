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

const disputeJSONLimit = 1 << 20

func (t *purchaseTx) Dispute(ctx context.Context, scope billing.Scope, id string) (purchase.Dispute, error) {
	if !scope.Valid() || !billing.ValidID(id) {
		return purchase.Dispute{}, billing.ErrInvalid
	}
	var out purchase.Dispute
	var account, providerName, merchant, environment, disputeID, termsFP, digest string
	var recovered []byte
	var debitID sql.NullString
	err := t.session.tx.QueryRowContext(ctx, `SELECT account_id,provider,merchant,environment,dispute_id,intent_id,transaction_id,currency,amount,status,status_occurred_at,status_event_id,debit_adjustment_id,recovered,revision,created_at,updated_at,terms_fingerprint,record_digest FROM billing_purchase_disputes WHERE provider=$1 AND merchant=$2 AND environment=$3 AND dispute_id=$4 AND account_id=$5`, scope.Provider, scope.Merchant, scope.Environment, id, string(t.session.account)).Scan(&account, &providerName, &merchant, &environment, &disputeID, &out.IntentID, &out.TransactionID, &out.Currency, &out.Amount, &out.Status, &out.StatusOccurredAt, &out.StatusEventID, &debitID, &recovered, &out.Revision, &out.CreatedAt, &out.UpdatedAt, &termsFP, &digest)
	if errors.Is(err, sql.ErrNoRows) {
		var owner string
		e := t.session.tx.QueryRowContext(ctx, `SELECT account_id FROM billing_purchase_disputes WHERE provider=$1 AND merchant=$2 AND environment=$3 AND dispute_id=$4`, scope.Provider, scope.Merchant, scope.Environment, id).Scan(&owner)
		if e == nil && owner != string(t.session.account) {
			return purchase.Dispute{}, billing.ErrConflict
		}
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return purchase.Dispute{}, e
		}
		return purchase.Dispute{}, billing.ErrNotFound
	}
	if err != nil {
		return purchase.Dispute{}, err
	}
	if len(recovered) > disputeJSONLimit {
		return purchase.Dispute{}, billing.ErrInvalid
	}
	if err := json.Unmarshal(recovered, &out.Recovered); err != nil {
		return purchase.Dispute{}, err
	}
	out.DebitAdjustmentID = debitID.String
	out.Account = billing.AccountID(account)
	out.Scope = scope
	out.ID = disputeID
	out.StatusOccurredAt = billing.CanonicalTime(out.StatusOccurredAt)
	out.CreatedAt = billing.CanonicalTime(out.CreatedAt)
	out.UpdatedAt = billing.CanonicalTime(out.UpdatedAt)
	if out.Account != t.session.account || out.Scope != scope || out.ID != id || out.TermsFingerprint() != termsFP || out.Fingerprint() != digest {
		return purchase.Dispute{}, fmt.Errorf("purchase dispute integrity: %w", billing.ErrConflict)
	}
	if err := out.Validate(); err != nil {
		return purchase.Dispute{}, err
	}
	return out, nil
}

func (t *purchaseTx) SaveDispute(ctx context.Context, in purchase.Dispute, expected int64) error {
	if err := t.session.scope(in.Account); err != nil {
		return err
	}
	if err := in.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(in.Recovered)
	if err != nil || len(raw) > disputeJSONLimit {
		return billing.ErrInvalid
	}
	if expected < 0 || in.Revision != expected+1 {
		return billing.ErrConflict
	}
	if expected == 0 {
		_, err = t.session.tx.ExecContext(ctx, `INSERT INTO billing_purchase_disputes(account_id,provider,merchant,environment,dispute_id,intent_id,transaction_id,currency,amount,status,status_occurred_at,status_event_id,debit_adjustment_id,recovered,revision,created_at,updated_at,terms_fingerprint,record_digest) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,NULLIF($13::text,''),$14,$15,$16,$17,$18,$19) ON CONFLICT DO NOTHING`, string(in.Account), in.Scope.Provider, in.Scope.Merchant, in.Scope.Environment, in.ID, in.IntentID, in.TransactionID, in.Currency, in.Amount, in.Status, in.StatusOccurredAt, in.StatusEventID, in.DebitAdjustmentID, raw, in.Revision, in.CreatedAt, in.UpdatedAt, in.TermsFingerprint(), in.Fingerprint())
		if err != nil {
			return mapPurchaseLifecycleConflict(err)
		}
		got, e := t.Dispute(ctx, in.Scope, in.ID)
		if e != nil {
			return e
		}
		if got.Fingerprint() != in.Fingerprint() {
			return billing.ErrConflict
		}
		return nil
	}
	old, err := t.Dispute(ctx, in.Scope, in.ID)
	if err != nil {
		return err
	}
	if old.TermsFingerprint() != in.TermsFingerprint() || billing.CanonicalTime(old.CreatedAt) != billing.CanonicalTime(in.CreatedAt) || (old.DebitAdjustmentID != "" && old.DebitAdjustmentID != in.DebitAdjustmentID) {
		return billing.ErrConflict
	}
	res, err := t.session.tx.ExecContext(ctx, `UPDATE billing_purchase_disputes SET status=$5,status_occurred_at=$6,status_event_id=$7,debit_adjustment_id=NULLIF($8::text,''),recovered=$9,revision=$10,updated_at=$11,record_digest=$12 WHERE account_id=$1 AND provider=$2 AND merchant=$3 AND environment=$4 AND dispute_id=$13 AND revision=$14`, string(in.Account), in.Scope.Provider, in.Scope.Merchant, in.Scope.Environment, in.Status, in.StatusOccurredAt, in.StatusEventID, in.DebitAdjustmentID, raw, in.Revision, in.UpdatedAt, in.Fingerprint(), in.ID, expected)
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
	got, err := t.Dispute(ctx, in.Scope, in.ID)
	if err != nil {
		return err
	}
	if got.Fingerprint() != in.Fingerprint() {
		return billing.ErrConflict
	}
	return nil
}

func (t *purchaseTx) DisputeEvent(ctx context.Context, scope billing.Scope, eventID string) (purchase.DisputeRecord, error) {
	if !scope.Valid() || !billing.ValidID(eventID) {
		return purchase.DisputeRecord{}, billing.ErrInvalid
	}
	var account, providerName, merchant, environment, storedEvent, disputeID, intentID, transactionID, digest string
	var factRaw, resultRaw []byte
	var created sql.NullTime
	err := t.session.tx.QueryRowContext(ctx, `SELECT account_id,provider,merchant,environment,event_id,dispute_id,intent_id,transaction_id,fact,result,created_at,record_digest FROM billing_purchase_dispute_events WHERE provider=$1 AND merchant=$2 AND environment=$3 AND event_id=$4 AND account_id=$5`, scope.Provider, scope.Merchant, scope.Environment, eventID, string(t.session.account)).Scan(&account, &providerName, &merchant, &environment, &storedEvent, &disputeID, &intentID, &transactionID, &factRaw, &resultRaw, &created, &digest)
	if errors.Is(err, sql.ErrNoRows) {
		var owner string
		e := t.session.tx.QueryRowContext(ctx, `SELECT account_id FROM billing_purchase_dispute_events WHERE provider=$1 AND merchant=$2 AND environment=$3 AND event_id=$4`, scope.Provider, scope.Merchant, scope.Environment, eventID).Scan(&owner)
		if e == nil && owner != string(t.session.account) {
			return purchase.DisputeRecord{}, billing.ErrConflict
		}
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return purchase.DisputeRecord{}, e
		}
		return purchase.DisputeRecord{}, billing.ErrNotFound
	}
	if err != nil {
		return purchase.DisputeRecord{}, err
	}
	if len(factRaw) > disputeJSONLimit || len(resultRaw) > disputeJSONLimit {
		return purchase.DisputeRecord{}, billing.ErrInvalid
	}
	var out purchase.DisputeRecord
	if err := json.Unmarshal(factRaw, &out.Fact); err != nil {
		return out, err
	}
	if err := json.Unmarshal(resultRaw, &out.Result); err != nil {
		return out, err
	}
	out.CreatedAt = created.Time
	out.Fact.OccurredAt = billing.CanonicalTime(out.Fact.OccurredAt)
	if account != string(t.session.account) || storedEvent != eventID || out.Fact.EventID != storedEvent || disputeID != out.Fact.DisputeID || intentID != out.Fact.IntentID || transactionID != out.Fact.TransactionID || out.Fact.Scope != scope || out.Fact.Account != t.session.account || out.Fingerprint() != digest {
		return purchase.DisputeRecord{}, fmt.Errorf("purchase dispute event integrity: %w", billing.ErrConflict)
	}
	if err := out.Validate(); err != nil {
		return purchase.DisputeRecord{}, err
	}
	return out, nil
}
func (t *purchaseTx) InsertDisputeEvent(ctx context.Context, in purchase.DisputeRecord) error {
	if err := t.session.scope(in.Fact.Account); err != nil {
		return err
	}
	if err := in.Validate(); err != nil {
		return err
	}
	factRaw, e := json.Marshal(in.Fact)
	if e != nil || len(factRaw) > disputeJSONLimit {
		return billing.ErrInvalid
	}
	resultRaw, e := json.Marshal(in.Result)
	if e != nil || len(resultRaw) > disputeJSONLimit {
		return billing.ErrInvalid
	}
	_, e = t.session.tx.ExecContext(ctx, `INSERT INTO billing_purchase_dispute_events(account_id,provider,merchant,environment,event_id,dispute_id,intent_id,transaction_id,fact,result,created_at,record_digest) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) ON CONFLICT DO NOTHING`, string(in.Fact.Account), in.Fact.Scope.Provider, in.Fact.Scope.Merchant, in.Fact.Scope.Environment, in.Fact.EventID, in.Fact.DisputeID, in.Fact.IntentID, in.Fact.TransactionID, factRaw, resultRaw, in.CreatedAt, in.Fingerprint())
	if e != nil {
		return mapPurchaseLifecycleConflict(e)
	}
	got, e := t.DisputeEvent(ctx, in.Fact.Scope, in.Fact.EventID)
	if e != nil {
		return e
	}
	if got.Fingerprint() != in.Fingerprint() {
		return billing.ErrConflict
	}
	return nil
}

func (t *purchaseTx) DisputeRecovery(ctx context.Context, scope billing.Scope, id string) (purchase.DisputeRecoveryRecord, error) {
	if !scope.Valid() || !billing.ValidID(id) {
		return purchase.DisputeRecoveryRecord{}, billing.ErrInvalid
	}
	var out purchase.DisputeRecoveryRecord
	var account, storedID, adjustment, disputeID, intentID, transactionID, recoveryFP, digest string
	var raw []byte
	err := t.session.tx.QueryRowContext(ctx, `SELECT account_id,recovery_id,adjustment_id,dispute_id,intent_id,transaction_id,recovery,created_at,recovery_fingerprint,record_digest FROM billing_purchase_dispute_recoveries WHERE provider=$1 AND merchant=$2 AND environment=$3 AND recovery_id=$4 AND account_id=$5`, scope.Provider, scope.Merchant, scope.Environment, id, string(t.session.account)).Scan(&account, &storedID, &adjustment, &disputeID, &intentID, &transactionID, &raw, &out.CreatedAt, &recoveryFP, &digest)
	if errors.Is(err, sql.ErrNoRows) {
		var owner string
		e := t.session.tx.QueryRowContext(ctx, `SELECT account_id FROM billing_purchase_dispute_recoveries WHERE provider=$1 AND merchant=$2 AND environment=$3 AND recovery_id=$4`, scope.Provider, scope.Merchant, scope.Environment, id).Scan(&owner)
		if e == nil && owner != string(t.session.account) {
			return out, billing.ErrConflict
		}
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return out, e
		}
		return out, billing.ErrNotFound
	}
	if err != nil {
		return out, err
	}
	if len(raw) > disputeJSONLimit {
		return purchase.DisputeRecoveryRecord{}, billing.ErrInvalid
	}
	if err := json.Unmarshal(raw, &out.Recovery); err != nil {
		return purchase.DisputeRecoveryRecord{}, err
	}
	if out.Recovery.ID != storedID || out.Recovery.AdjustmentID != adjustment {
		return purchase.DisputeRecoveryRecord{}, fmt.Errorf("purchase dispute recovery identity: %w", billing.ErrConflict)
	}
	out.Account = billing.AccountID(account)
	out.Scope = scope
	out.DisputeID = disputeID
	out.IntentID = intentID
	out.TransactionID = transactionID
	out.CreatedAt = billing.CanonicalTime(out.CreatedAt)
	if out.Account != t.session.account || out.RecoveryFingerprint() != recoveryFP || out.Fingerprint() != digest {
		return purchase.DisputeRecoveryRecord{}, fmt.Errorf("purchase dispute recovery integrity: %w", billing.ErrConflict)
	}
	if err := out.Validate(); err != nil {
		return purchase.DisputeRecoveryRecord{}, err
	}
	return out, nil
}
func (t *purchaseTx) InsertDisputeRecovery(ctx context.Context, in purchase.DisputeRecoveryRecord) error {
	if err := t.session.scope(in.Account); err != nil {
		return err
	}
	if err := in.Validate(); err != nil {
		return err
	}
	raw, e := json.Marshal(in.Recovery)
	if e != nil || len(raw) > disputeJSONLimit {
		return billing.ErrInvalid
	}
	_, e = t.session.tx.ExecContext(ctx, `INSERT INTO billing_purchase_dispute_recoveries(account_id,provider,merchant,environment,recovery_id,adjustment_id,dispute_id,intent_id,transaction_id,recovery,created_at,recovery_fingerprint,record_digest) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) ON CONFLICT DO NOTHING`, string(in.Account), in.Scope.Provider, in.Scope.Merchant, in.Scope.Environment, in.Recovery.ID, in.Recovery.AdjustmentID, in.DisputeID, in.IntentID, in.TransactionID, raw, in.CreatedAt, in.RecoveryFingerprint(), in.Fingerprint())
	if e != nil {
		return mapPurchaseLifecycleConflict(e)
	}
	got, e := t.DisputeRecovery(ctx, in.Scope, in.Recovery.ID)
	if e != nil {
		return e
	}
	if got.RecoveryFingerprint() != in.RecoveryFingerprint() {
		return billing.ErrConflict
	}
	return nil
}
