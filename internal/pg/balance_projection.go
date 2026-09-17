package pg

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
)

const creditBalanceProjectionPageMaximum = 1000

type balanceProjectionLot struct {
	id                                          string
	unit, scope                                 string
	available, held, consumed, expired, revoked int64
}

type balanceProjectionTotals struct {
	available, held, consumed, expired, revoked big.Int
}

func (t *balanceProjectionTotals) add(lot balanceProjectionLot, sign int64) {
	add := func(dst *big.Int, value int64) {
		var n big.Int
		n.SetInt64(value)
		if sign < 0 {
			dst.Sub(dst, &n)
		} else {
			dst.Add(dst, &n)
		}
	}
	add(&t.available, lot.available)
	add(&t.held, lot.held)
	add(&t.consumed, lot.consumed)
	add(&t.expired, lot.expired)
	add(&t.revoked, lot.revoked)
}

type balanceProjectionKey struct{ unit, scope string }

// The account row lock is the same serialization boundary used by credit mutations.
func (s *Store) BuildCreditBalanceProjection(ctx context.Context, account billing.AccountID, limit int) (bool, error) {
	if s == nil || s.db == nil || !billing.ValidID(string(account)) || limit < 1 || limit > creditBalanceProjectionPageMaximum {
		return false, billing.ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, mapCreditRepairError(err)
	}
	defer tx.Rollback()

	var found string
	if err := tx.QueryRowContext(ctx,
		`SELECT account_id FROM billing_accounts WHERE account_id=$1 FOR UPDATE`, string(account),
	).Scan(&found); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, billing.ErrNotFound
		}
		return false, mapCreditRepairError(err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO billing_credit_balance_projection_state (account_id, last_lot_id, ready)
		VALUES ($1, NULL, false)
		ON CONFLICT (account_id) DO NOTHING`, string(account)); err != nil {
		return false, mapCreditRepairError(err)
	}

	var cursor sql.NullString
	var ready bool
	if err := tx.QueryRowContext(ctx, `
		SELECT last_lot_id, ready
		FROM billing_credit_balance_projection_state
		WHERE account_id=$1`, string(account)).Scan(&cursor, &ready); err != nil {
		return false, mapCreditRepairError(err)
	}
	if ready {
		if err := tx.Commit(); err != nil {
			return false, mapCreditRepairError(err)
		}
		return true, nil
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT lot_id, unit_code, scope, available, held, consumed, expired, revoked
		FROM billing_lots
		WHERE account_id=$1 AND ($2::text IS NULL OR lot_id>$2)
		ORDER BY lot_id
		LIMIT $3`, string(account), nullableString(cursor), limit+1)
	if err != nil {
		return false, mapCreditRepairError(err)
	}
	page := make([]balanceProjectionLot, 0, limit+1)
	for rows.Next() {
		var lot balanceProjectionLot
		if err := rows.Scan(&lot.id, &lot.unit, &lot.scope, &lot.available, &lot.held, &lot.consumed, &lot.expired, &lot.revoked); err != nil {
			_ = rows.Close()
			return false, mapCreditRepairError(err)
		}
		page = append(page, lot)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return false, mapCreditRepairError(err)
	}
	if err := rows.Close(); err != nil {
		return false, mapCreditRepairError(err)
	}

	processed := page
	done := len(page) <= limit
	if !done {
		processed = page[:limit]
	}
	if err := addProjectionPage(ctx, tx, account, processed); err != nil {
		return false, mapCreditRepairError(err)
	}
	if len(processed) > 0 {
		cursor = sql.NullString{String: processed[len(processed)-1].id, Valid: true}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE billing_credit_balance_projection_state
		SET last_lot_id=$2, ready=$3
		WHERE account_id=$1`, string(account), nullableString(cursor), done); err != nil {
		return false, mapCreditRepairError(err)
	}
	if err := tx.Commit(); err != nil {
		return false, mapCreditRepairError(err)
	}
	return done, nil
}

func nullableString(value sql.NullString) any {
	if !value.Valid {
		return nil
	}
	return value.String
}

func addProjectionPage(ctx context.Context, q querier, account billing.AccountID, page []balanceProjectionLot) error {
	accountTotals := make(map[string]*balanceProjectionTotals)
	scopeTotals := make(map[balanceProjectionKey]*balanceProjectionTotals)
	for _, lot := range page {
		accountTotal := accountTotals[lot.unit]
		if accountTotal == nil {
			accountTotal = new(balanceProjectionTotals)
			accountTotals[lot.unit] = accountTotal
		}
		accountTotal.add(lot, 1)
		key := balanceProjectionKey{unit: lot.unit, scope: lot.scope}
		scopeTotal := scopeTotals[key]
		if scopeTotal == nil {
			scopeTotal = new(balanceProjectionTotals)
			scopeTotals[key] = scopeTotal
		}
		scopeTotal.add(lot, 1)
	}
	for unit, totals := range accountTotals {
		if err := upsertAccountBalance(ctx, q, account, unit, totals); err != nil {
			return err
		}
	}
	for key, totals := range scopeTotals {
		if err := upsertScopeBalance(ctx, q, account, key.unit, key.scope, totals); err != nil {
			return err
		}
	}
	return nil
}

func upsertAccountBalance(ctx context.Context, q querier, account billing.AccountID, unit string, totals *balanceProjectionTotals) error {
	_, err := q.ExecContext(ctx, `
		INSERT INTO billing_credit_account_balances
		(account_id, unit_code, available, held, consumed, expired, revoked)
		VALUES ($1,$2,$3::numeric,$4::numeric,$5::numeric,$6::numeric,$7::numeric)
		ON CONFLICT (account_id, unit_code) DO UPDATE SET
		available=billing_credit_account_balances.available+EXCLUDED.available,
		held=billing_credit_account_balances.held+EXCLUDED.held,
		consumed=billing_credit_account_balances.consumed+EXCLUDED.consumed,
		expired=billing_credit_account_balances.expired+EXCLUDED.expired,
		revoked=billing_credit_account_balances.revoked+EXCLUDED.revoked`,
		string(account), unit, totals.available.String(), totals.held.String(), totals.consumed.String(), totals.expired.String(), totals.revoked.String())
	return err
}

func upsertScopeBalance(ctx context.Context, q querier, account billing.AccountID, unit, scope string, totals *balanceProjectionTotals) error {
	_, err := q.ExecContext(ctx, `
		INSERT INTO billing_credit_scope_balances
		(account_id, unit_code, scope, available, held, consumed, expired, revoked)
		VALUES ($1,$2,$3,$4::numeric,$5::numeric,$6::numeric,$7::numeric,$8::numeric)
		ON CONFLICT (account_id, unit_code, scope) DO UPDATE SET
		available=billing_credit_scope_balances.available+EXCLUDED.available,
		held=billing_credit_scope_balances.held+EXCLUDED.held,
		consumed=billing_credit_scope_balances.consumed+EXCLUDED.consumed,
		expired=billing_credit_scope_balances.expired+EXCLUDED.expired,
		revoked=billing_credit_scope_balances.revoked+EXCLUDED.revoked`,
		string(account), unit, scope, totals.available.String(), totals.held.String(), totals.consumed.String(), totals.expired.String(), totals.revoked.String())
	return err
}

// Available includes future and expired lots; callers must not use this value
// as authorization balance. Values outside credit.Balance's int64 representation
// are rejected.
func (s *Store) StoredCreditBalance(ctx context.Context, account billing.AccountID, unit, scope string) (credit.Balance, error) {
	if s == nil || s.db == nil {
		return credit.Balance{}, billing.ErrInvalid
	}
	if err := validateBalanceRead(account, unit, scope); err != nil {
		return credit.Balance{}, err
	}
	return storedCreditBalance(ctx, s.db, account, unit, scope)
}

func validateBalanceRead(account billing.AccountID, unit, scope string) error {
	if !billing.ValidID(string(account)) || !billing.ValidID(unit) || (scope != "" && !billing.ValidID(scope)) {
		return billing.ErrInvalid
	}
	return nil
}

func ensureCreditBalanceReady(ctx context.Context, q querier, account billing.AccountID) error {
	var ready bool
	err := q.QueryRowContext(ctx, `
		SELECT COALESCE(s.ready, false)
		FROM billing_accounts a
		LEFT JOIN billing_credit_balance_projection_state s ON s.account_id=a.account_id
		WHERE a.account_id=$1`, string(account)).Scan(&ready)
	if errors.Is(err, sql.ErrNoRows) {
		return billing.ErrNotFound
	}
	if err != nil {
		return err
	}
	if !ready {
		return billing.ErrState
	}
	return nil
}

func storedCreditBalance(ctx context.Context, q querier, account billing.AccountID, unit, scope string) (credit.Balance, error) {
	if err := validateBalanceRead(account, unit, scope); err != nil {
		return credit.Balance{}, err
	}
	if err := ensureCreditBalanceReady(ctx, q, account); err != nil {
		return credit.Balance{}, err
	}
	query := `
		SELECT COALESCE(SUM(available),0), COALESCE(SUM(held),0),
		       COALESCE(SUM(consumed),0), COALESCE(SUM(expired),0),
		       COALESCE(SUM(revoked),0)
		FROM billing_credit_account_balances
		WHERE account_id=$1 AND unit_code=$2`
	args := []any{string(account), unit}
	if scope != "" {
		query = `
			SELECT COALESCE(SUM(available),0), COALESCE(SUM(held),0),
			       COALESCE(SUM(consumed),0), COALESCE(SUM(expired),0),
			       COALESCE(SUM(revoked),0)
			FROM billing_credit_scope_balances
			WHERE account_id=$1 AND unit_code=$2 AND (scope='' OR scope=$3)`
		args = append(args, scope)
	}
	var values [5]string
	if err := q.QueryRowContext(ctx, query, args...).Scan(&values[0], &values[1], &values[2], &values[3], &values[4]); err != nil {
		return credit.Balance{}, err
	}
	var balance credit.Balance
	fields := []*int64{&balance.Available, &balance.Held, &balance.Consumed, &balance.Expired, &balance.Revoked}
	for i, field := range fields {
		value, err := projectionNumericInt64(values[i])
		if err != nil {
			return credit.Balance{}, err
		}
		*field = value
	}
	return balance, nil
}

func projectionNumericInt64(value string) (int64, error) {
	var number big.Int
	if _, ok := number.SetString(strings.TrimSpace(value), 10); !ok {
		return 0, fmt.Errorf("%w: invalid numeric balance projection", billing.ErrState)
	}
	if !number.IsInt64() {
		return 0, billing.ErrOverflow
	}
	if number.Sign() < 0 {
		return 0, fmt.Errorf("%w: negative numeric balance projection", billing.ErrState)
	}
	return number.Int64(), nil
}

// Retains exact historical counters while excluding projected available quantity.
func storedCreditLifetimeBalance(ctx context.Context, q querier, account billing.AccountID, unit, scope string) (credit.Balance, error) {
	if err := validateBalanceRead(account, unit, scope); err != nil {
		return credit.Balance{}, err
	}
	if err := ensureCreditBalanceReady(ctx, q, account); err != nil {
		return credit.Balance{}, err
	}
	query := `
		SELECT COALESCE(SUM(held),0), COALESCE(SUM(consumed),0),
		       COALESCE(SUM(expired),0), COALESCE(SUM(revoked),0)
		FROM billing_credit_account_balances
		WHERE account_id=$1 AND unit_code=$2`
	args := []any{string(account), unit}
	if scope != "" {
		query = `
			SELECT COALESCE(SUM(held),0), COALESCE(SUM(consumed),0),
			       COALESCE(SUM(expired),0), COALESCE(SUM(revoked),0)
			FROM billing_credit_scope_balances
			WHERE account_id=$1 AND unit_code=$2 AND (scope='' OR scope=$3)`
		args = append(args, scope)
	}
	var values [4]string
	if err := q.QueryRowContext(ctx, query, args...).Scan(&values[0], &values[1], &values[2], &values[3]); err != nil {
		return credit.Balance{}, err
	}
	var balance credit.Balance
	fields := []*int64{&balance.Held, &balance.Consumed, &balance.Expired, &balance.Revoked}
	for i, field := range fields {
		value, err := projectionNumericInt64(values[i])
		if err != nil {
			return credit.Balance{}, err
		}
		*field = value
	}
	return balance, nil
}

// Spendability is time-sensitive and future raw availability may overflow int64,
// so available is an indexed SUM over currently eligible live rows rather than
// the lifetime projection's available value.
func storedCreditBalanceAt(ctx context.Context, q querier, account billing.AccountID, unit, scope string, at time.Time) (credit.Balance, error) {
	b, err := storedCreditLifetimeBalance(ctx, q, account, unit, scope)
	if err != nil {
		return credit.Balance{}, err
	}
	if at.IsZero() {
		return credit.Balance{}, billing.ErrInvalid
	}
	query := `SELECT COALESCE(SUM(available),0) FROM billing_lots WHERE account_id=$1 AND unit_code=$2 AND available>0 AND valid_from <= $3 AND (expires_at IS NULL OR expires_at > $3) AND revoked_at IS NULL`
	args := []any{string(account), unit, databaseTime(at)}
	if scope != "" {
		query = `SELECT COALESCE(SUM(available),0) FROM billing_lots WHERE account_id=$1 AND unit_code=$2 AND (scope='' OR scope=$3) AND available>0 AND valid_from <= $4 AND (expires_at IS NULL OR expires_at > $4) AND revoked_at IS NULL`
		args = []any{string(account), unit, scope, databaseTime(at)}
	}
	var available string
	if err := q.QueryRowContext(ctx, query, args...).Scan(&available); err != nil {
		return credit.Balance{}, err
	}
	b.Available, err = projectionNumericInt64(available)
	return b, err
}
