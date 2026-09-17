package pg

import (
	"context"
	"database/sql"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/internal/checked"
)

func repairPublishPage(ctx context.Context, tx *sql.Tx, r *creditRepairRecord, limit int) error {
	switch r.Phase {
	case "apply_lots":
		rows, err := tx.QueryContext(ctx, `SELECT lot_id,granted,available,held,consumed,expired,revoked,pending_revocation FROM billing_credit_repair_lots WHERE account_id=$1 AND repair_id=$2 AND lot_id>$3 ORDER BY lot_id LIMIT $4`, string(r.Account), r.ID, r.LotCursor, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		var ids []string
		var initial, available, held, consumed, expired, revoked, pending []int64
		for rows.Next() {
			var id string
			var q CreditRepairQuantities
			if err = rows.Scan(&id, &q.Initial, &q.Available, &q.Held, &q.Consumed, &q.Expired, &q.Revoked, &q.PendingRevocation); err != nil {
				return err
			}
			ids = append(ids, id)
			initial = append(initial, q.Initial)
			available = append(available, q.Available)
			held = append(held, q.Held)
			consumed = append(consumed, q.Consumed)
			expired = append(expired, q.Expired)
			revoked = append(revoked, q.Revoked)
			pending = append(pending, q.PendingRevocation)
		}
		if err = rows.Err(); err != nil {
			return err
		}
		if err = rows.Close(); err != nil {
			return err
		}
		if len(ids) > 0 {
			result, err := tx.ExecContext(ctx, `UPDATE billing_lots l SET initial=x.initial,available=x.available,held=x.held,consumed=x.consumed,expired=x.expired,revoked=x.revoked,pending_revocation=x.pending FROM unnest($3::text[],$4::bigint[],$5::bigint[],$6::bigint[],$7::bigint[],$8::bigint[],$9::bigint[],$10::bigint[]) x(id,initial,available,held,consumed,expired,revoked,pending) WHERE l.account_id=$1 AND l.lot_id=x.id AND EXISTS(SELECT 1 FROM billing_credit_repair_jobs j WHERE j.account_id=$1 AND j.repair_id=$2)`, string(r.Account), r.ID, ids, initial, available, held, consumed, expired, revoked, pending)
			if err != nil {
				return err
			}
			if err = expectSettlementRows(result, int64(len(ids))); err != nil {
				return err
			}
			r.AppliedLots, err = checked.Add(r.AppliedLots, int64(len(ids)))
			if err != nil {
				return err
			}
			r.LotCursor = ids[len(ids)-1]
		}
		if len(ids) < limit {
			if r.AppliedLots != r.VerifiedLots {
				return billing.ErrState
			}
			r.Phase = "clear_accounts"
			r.LotCursor = ""
		}
		return nil
	case "clear_accounts", "clear_scopes":
		table, key := "billing_credit_account_balances", "unit_code"
		if r.Phase == "clear_scopes" {
			table, key = "billing_credit_scope_balances", "unit_code,scope"
		}
		result, err := tx.ExecContext(ctx, `WITH page AS (SELECT `+key+` FROM `+table+` WHERE account_id=$1 ORDER BY `+key+` LIMIT $2) DELETE FROM `+table+` b USING page p WHERE b.account_id=$1 AND b.unit_code=p.unit_code`+repairScopePredicate(r.Phase == "clear_scopes"), string(r.Account), limit)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count < int64(limit) {
			if r.Phase == "clear_scopes" {
				r.Phase = "write_accounts"
			} else {
				r.Phase = "clear_scopes"
			}
			r.unitCursor = ""
			r.scopeCursor = ""
		}
		return nil
	case "write_accounts", "write_scopes":
		return repairPublishTotals(ctx, tx, r, limit)
	default:
		return billing.ErrState
	}
}

func repairScopePredicate(scoped bool) string {
	if scoped {
		return " AND b.scope=p.scope"
	}
	return ""
}

func repairPublishTotals(ctx context.Context, tx *sql.Tx, r *creditRepairRecord, limit int) error {
	kind, table := "account", "billing_credit_account_balances"
	if r.Phase == "write_scopes" {
		kind, table = "scope", "billing_credit_scope_balances"
	}
	rows, err := tx.QueryContext(ctx, `SELECT unit_code,scope,available::text,held::text,consumed::text,expired::text,revoked::text FROM billing_credit_repair_totals WHERE account_id=$1 AND repair_id=$2 AND kind=$3 AND (unit_code,scope)>($4,$5) ORDER BY unit_code,scope LIMIT $6`, string(r.Account), r.ID, kind, r.unitCursor, r.scopeCursor, limit)
	if err != nil {
		return err
	}
	defer rows.Close()
	var units, scopes, available, held, consumed, expired, revoked []string
	for rows.Next() {
		var u, s, a, h, c, e, v string
		if err = rows.Scan(&u, &s, &a, &h, &c, &e, &v); err != nil {
			return err
		}
		units = append(units, u)
		scopes = append(scopes, s)
		available = append(available, a)
		held = append(held, h)
		consumed = append(consumed, c)
		expired = append(expired, e)
		revoked = append(revoked, v)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if err = rows.Close(); err != nil {
		return err
	}
	if len(units) > 0 {
		columns, selection := "unit_code,available,held,consumed,expired,revoked", "unit_code,available,held,consumed,expired,revoked"
		if kind == "scope" {
			columns = "unit_code,scope,available,held,consumed,expired,revoked"
			selection = columns
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO `+table+`(account_id,`+columns+`) SELECT $1,`+selection+` FROM unnest($2::text[],$3::text[],$4::numeric[],$5::numeric[],$6::numeric[],$7::numeric[],$8::numeric[]) x(unit_code,scope,available,held,consumed,expired,revoked)`, string(r.Account), units, scopes, available, held, consumed, expired, revoked)
		if err != nil {
			return err
		}
		if err = expectSettlementRows(result, int64(len(units))); err != nil {
			return err
		}
		r.unitCursor = units[len(units)-1]
		r.scopeCursor = scopes[len(scopes)-1]
	}
	if len(units) == limit {
		return nil
	}
	if kind == "account" {
		r.Phase = "write_scopes"
		r.unitCursor = ""
		r.scopeCursor = ""
		return nil
	}
	if r.AppliedLots != r.VerifiedLots || r.VerifiedLots != r.ReplayLots || r.JournalCursor != r.TargetSequence {
		return billing.ErrState
	}
	result, err := tx.ExecContext(ctx, `UPDATE billing_credit_balance_projection_state SET ready=true,last_lot_id=NULL,repair_job_id=NULL WHERE account_id=$1 AND repair_job_id=$2 AND NOT ready`, string(r.Account), r.ID)
	if err != nil {
		return err
	}
	if err = expectSettlementRows(result, 1); err != nil {
		return err
	}
	r.Phase = "completed"
	return nil
}
