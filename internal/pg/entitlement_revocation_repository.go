package pg

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/catalog"
)

func (t *entitlementTx) Revocation(ctx context.Context, id string) (catalog.Revocation, error) {
	if !billing.ValidID(id) {
		return catalog.Revocation{}, billing.ErrInvalid
	}
	var out catalog.Revocation
	var fingerprint string
	err := t.session.tx.QueryRowContext(ctx, `
		SELECT account_id, assignment_id, source_ref, actor, reason, effective_at, created_at, fingerprint
		FROM billing_entitlement_assignment_revocations
		WHERE account_id=$1 AND assignment_id=$2`, string(t.session.account), id).
		Scan(&out.Account, &out.AssignmentID, &out.SourceRef, &out.Actor, &out.Reason, &out.EffectiveAt, &out.CreatedAt, &fingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		return catalog.Revocation{}, billing.ErrNotFound
	}
	if err != nil {
		return catalog.Revocation{}, err
	}
	out.EffectiveAt = billing.CanonicalTime(out.EffectiveAt)
	out.CreatedAt = billing.CanonicalTime(out.CreatedAt)
	if err := out.Validate(); err != nil {
		return catalog.Revocation{}, fmt.Errorf("entitlement revocation: %w", err)
	}
	if out.Account != t.session.account || out.AssignmentID != id || out.Fingerprint() != fingerprint {
		return catalog.Revocation{}, fmt.Errorf("entitlement revocation fingerprint: %w", billing.ErrConflict)
	}
	return out, nil
}

func (t *entitlementTx) InsertRevocation(ctx context.Context, in catalog.Revocation) error {
	if err := t.session.scope(in.Account); err != nil {
		return err
	}
	if err := in.Validate(); err != nil {
		return err
	}
	_, err := t.session.tx.ExecContext(ctx, `
		INSERT INTO billing_entitlement_assignment_revocations
		(account_id, assignment_id, source_ref, actor, reason, effective_at, created_at, fingerprint)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT DO NOTHING`,
		string(in.Account), in.AssignmentID, in.SourceRef, in.Actor, in.Reason, in.EffectiveAt, in.CreatedAt, in.Fingerprint())
	if err != nil {
		return mapEntitlementConflict(err)
	}
	stored, err := t.Revocation(ctx, in.AssignmentID)
	if err != nil {
		return err
	}
	if stored.Fingerprint() != in.Fingerprint() {
		return billing.ErrConflict
	}
	return nil
}

// RevocationsPage walks an account's revocations in assignment_id order.
func (t *entitlementTx) RevocationsPage(ctx context.Context, after string, limit int) ([]catalog.Revocation, error) {
	if limit < 1 || limit > 1000 || (after != "" && !billing.ValidID(after)) {
		return nil, billing.ErrInvalid
	}
	rows, err := t.session.tx.QueryContext(ctx, `
		SELECT account_id, assignment_id, source_ref, actor, reason, effective_at, created_at, fingerprint
		FROM billing_entitlement_assignment_revocations
		WHERE account_id=$1 AND assignment_id>$2
		ORDER BY assignment_id ASC LIMIT $3`, string(t.session.account), after, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]catalog.Revocation, 0, limit)
	for rows.Next() {
		var value catalog.Revocation
		var fingerprint string
		if err := rows.Scan(&value.Account, &value.AssignmentID, &value.SourceRef, &value.Actor, &value.Reason, &value.EffectiveAt, &value.CreatedAt, &fingerprint); err != nil {
			return nil, err
		}
		value.EffectiveAt = billing.CanonicalTime(value.EffectiveAt)
		value.CreatedAt = billing.CanonicalTime(value.CreatedAt)
		if err := value.Validate(); err != nil {
			return nil, fmt.Errorf("entitlement revocation: %w", err)
		}
		if value.Account != t.session.account || value.Fingerprint() != fingerprint {
			return nil, fmt.Errorf("entitlement revocation fingerprint: %w", billing.ErrConflict)
		}
		out = append(out, value)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
