package pg

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/purchase"
)

func (t *purchaseTx) TopUpPolicy(ctx context.Context, id string) (purchase.TopUpPolicy, error) {
	return readJSON[purchase.TopUpPolicy](ctx, t.session.tx, `SELECT record FROM billing_topup_policies WHERE account_id=$1 AND policy_id=$2`, string(t.session.account), id)
}

func (t *purchaseTx) SaveTopUpPolicy(ctx context.Context, policy purchase.TopUpPolicy) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := t.session.scope(policy.Account); err != nil {
		return err
	}
	raw, err := json.Marshal(policy)
	if err != nil {
		return err
	}
	_, err = t.session.tx.ExecContext(ctx, `INSERT INTO billing_topup_policies(account_id,policy_id,record) VALUES($1,$2,$3) ON CONFLICT (account_id,policy_id) DO UPDATE SET record=EXCLUDED.record`, string(t.session.account), policy.ID, raw)
	return mapPurchaseConflict(err)
}

func (t *purchaseTx) TopUpAttempt(ctx context.Context, id string) (purchase.TopUpAttempt, error) {
	return readJSON[purchase.TopUpAttempt](ctx, t.session.tx, `SELECT record FROM billing_topup_attempts WHERE account_id=$1 AND attempt_id=$2`, string(t.session.account), id)
}

func (t *purchaseTx) ActiveTopUpAttempt(ctx context.Context, policyID string) (purchase.TopUpAttempt, error) {
	return readJSON[purchase.TopUpAttempt](ctx, t.session.tx, `SELECT record FROM billing_topup_attempts WHERE account_id=$1 AND policy_id=$2 AND state IN ('planned','dispatched','unknown','action_required') ORDER BY attempt_id COLLATE "C" LIMIT 1`, string(t.session.account), policyID)
}

func (t *purchaseTx) SaveTopUpAttempt(ctx context.Context, attempt purchase.TopUpAttempt) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := t.session.scope(attempt.Account); err != nil {
		return err
	}
	raw, err := json.Marshal(attempt)
	if err != nil {
		return err
	}
	_, err = t.session.tx.ExecContext(ctx, `INSERT INTO billing_topup_attempts(account_id,attempt_id,policy_id,state,created_at,record) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT (account_id,attempt_id) DO UPDATE SET state=EXCLUDED.state, record=EXCLUDED.record`, string(t.session.account), attempt.ID, attempt.PolicyID, string(attempt.State), attempt.CreatedAt, raw)
	if err == nil {
		return nil
	}
	if pgerr, ok := errors.AsType[*pgconn.PgError](err); ok && pgerr.Code == "23505" {
		return billing.ErrConflict
	}
	return err
}

func (t *purchaseTx) TopUpAttempts(ctx context.Context, policyID string) ([]purchase.TopUpAttempt, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := t.session.tx.QueryContext(ctx, `SELECT record FROM billing_topup_attempts WHERE account_id=$1 AND policy_id=$2`, string(t.session.account), policyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]purchase.TopUpAttempt, 0)
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var attempt purchase.TopUpAttempt
		if err := json.Unmarshal(raw, &attempt); err != nil {
			return nil, err
		}
		out = append(out, attempt)
	}
	return out, rows.Err()
}
