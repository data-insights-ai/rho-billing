package pg

import (
	"context"
	"database/sql"
	"errors"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/integration"
)

type session struct {
	store   *Store
	tx      *sql.Tx
	ctx     context.Context
	account billing.AccountID
}

// Atomic composes multiple billing domains in one account-scoped transaction.
// No callback, session, or transaction-bound repository may escape its lifetime.
func (s *Store) Atomic(ctx context.Context, account billing.AccountID, fn func(integration.Session) error) error {
	if s == nil || s.db == nil || fn == nil || !billing.ValidID(string(account)) {
		return billing.ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var found string
	if err = tx.QueryRowContext(ctx, `SELECT account_id FROM billing_accounts WHERE account_id=$1 FOR UPDATE`, string(account)).Scan(&found); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return billing.ErrNotFound
		}
		return err
	}
	view := &session{store: s, tx: tx, ctx: ctx, account: account}
	if err = fn(view); err != nil {
		return mapCreditRepairError(err)
	}
	return mapCreditRepairError(tx.Commit())
}
func (s *session) scope(account billing.AccountID) error {
	if account != s.account {
		return billing.ErrNotFound
	}
	return s.ctx.Err()
}
func (s *session) Credits() credit.Repository { return &boundCredits{s} }

type boundCredits struct{ session *session }

func (b *boundCredits) WithinAccount(ctx context.Context, account billing.AccountID, fn func(credit.Tx) error) error {
	if err := b.session.scope(account); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return mapCreditRepairError(fn(&transaction{tx: b.session.tx, ctx: ctx, account: account, now: b.session.store.now}))
}
func (b *boundCredits) History(ctx context.Context, account billing.AccountID, after int64, limit int) ([]credit.Entry, error) {
	if err := b.session.scope(account); err != nil {
		return nil, err
	}
	if after < 0 || limit < 1 || limit > 1000 {
		return nil, billing.ErrInvalid
	}
	return journalPage(ctx, b.session.tx, account, after, limit)
}

type querier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}
