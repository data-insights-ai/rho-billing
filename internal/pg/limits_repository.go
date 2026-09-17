package pg

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/integration"
)

type limitsRepository struct {
	store   *Store
	session *session
}

func (s *Store) Limits() credit.LimitRepository { return &limitsRepository{store: s} }

func (s *session) Limits() credit.LimitRepository {
	return &limitsRepository{store: s.store, session: s}
}

func (r *limitsRepository) WithinAccount(ctx context.Context, account billing.AccountID, fn func(credit.LimitTx) error) error {
	if fn == nil || !billing.ValidID(string(account)) {
		return billing.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.session != nil {
		if err := r.session.scope(account); err != nil {
			return err
		}
		return fn(&limitsTx{session: r.session})
	}
	return r.store.Atomic(ctx, account, func(v integration.Session) error {
		return fn(&limitsTx{session: v.(*session)})
	})
}

type limitsTx struct{ session *session }

func (t *limitsTx) Budget(ctx context.Context, id string) (credit.Budget, error) {
	return readJSON[credit.Budget](ctx, t.session.tx, `SELECT record FROM billing_budgets WHERE account_id=$1 AND budget_id=$2`, string(t.session.account), id)
}

func (t *limitsTx) SaveBudget(ctx context.Context, budget credit.Budget) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := t.session.scope(budget.Account); err != nil {
		return err
	}
	raw, err := json.Marshal(budget)
	if err != nil {
		return err
	}
	_, err = t.session.tx.ExecContext(ctx, `INSERT INTO billing_budgets(account_id,budget_id,actor,project,basis,currency,period_start,period_end,amount,record) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT (account_id,budget_id) DO UPDATE SET actor=EXCLUDED.actor,project=EXCLUDED.project,basis=EXCLUDED.basis,currency=EXCLUDED.currency,period_start=EXCLUDED.period_start,period_end=EXCLUDED.period_end,amount=EXCLUDED.amount,record=EXCLUDED.record`,
		string(t.session.account), budget.ID, budget.Actor, budget.Project, string(budget.Basis), budget.Currency, budget.Period.Start, budget.Period.End, budget.Amount, raw)
	return mapLimitConflict(err)
}

func (t *limitsTx) Budgets(ctx context.Context) ([]credit.Budget, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := t.session.tx.QueryContext(ctx, `SELECT record FROM billing_budgets WHERE account_id=$1 ORDER BY budget_id COLLATE "C"`, string(t.session.account))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]credit.Budget, 0)
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var budget credit.Budget
		if err := json.Unmarshal(raw, &budget); err != nil {
			return nil, err
		}
		out = append(out, budget)
	}
	return out, rows.Err()
}

func (t *limitsTx) Hold(ctx context.Context, id string) (credit.Hold, error) {
	return readJSON[credit.Hold](ctx, t.session.tx, `SELECT record FROM billing_budget_holds WHERE account_id=$1 AND hold_id=$2`, string(t.session.account), id)
}

func (t *limitsTx) HoldsByGroup(ctx context.Context, groupID string) ([]credit.Hold, error) {
	return t.queryHolds(ctx, `SELECT record FROM billing_budget_holds WHERE account_id=$1 AND group_id=$2 ORDER BY hold_id COLLATE "C"`, groupID)
}

func (t *limitsTx) HoldsByBudget(ctx context.Context, budgetID string) ([]credit.Hold, error) {
	return t.queryHolds(ctx, `SELECT record FROM billing_budget_holds WHERE account_id=$1 AND budget_id=$2`, budgetID)
}

func (t *limitsTx) queryHolds(ctx context.Context, query, id string) ([]credit.Hold, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := t.session.tx.QueryContext(ctx, query, string(t.session.account), id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]credit.Hold, 0)
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var hold credit.Hold
		if err := json.Unmarshal(raw, &hold); err != nil {
			return nil, err
		}
		out = append(out, hold)
	}
	return out, rows.Err()
}

func (t *limitsTx) SaveHold(ctx context.Context, hold credit.Hold) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := t.session.scope(hold.Account); err != nil {
		return err
	}
	raw, err := json.Marshal(hold)
	if err != nil {
		return err
	}
	_, err = t.session.tx.ExecContext(ctx, `INSERT INTO billing_budget_holds(account_id,hold_id,group_id,budget_id,state,amount,settled,record) VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT (account_id,hold_id) DO UPDATE SET state=EXCLUDED.state,settled=EXCLUDED.settled,record=EXCLUDED.record`,
		string(t.session.account), hold.ID, hold.GroupID, hold.BudgetID, string(hold.State), hold.Amount, hold.Settled, raw)
	return mapLimitConflict(err)
}

func mapLimitConflict(err error) error {
	if err == nil {
		return nil
	}
	if pgerr, ok := errors.AsType[*pgconn.PgError](err); ok && pgerr.Code == "23505" {
		return billing.ErrConflict
	}
	return err
}

var _ credit.LimitRepository = (*limitsRepository)(nil)
var _ credit.LimitTx = (*limitsTx)(nil)
