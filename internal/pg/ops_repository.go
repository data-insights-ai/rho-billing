package pg

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/integration"
)

type opsRepository struct {
	store   *Store
	session *session
}

func (s *Store) Ops() integration.OpsRepository { return &opsRepository{store: s} }

func (r *opsRepository) WithinAccount(ctx context.Context, account billing.AccountID, fn func(integration.OpsTx) error) error {
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
		return fn(&opsTx{session: r.session})
	}
	return r.store.Atomic(ctx, account, func(v integration.Session) error {
		return fn(&opsTx{session: v.(*session)})
	})
}

type opsTx struct{ session *session }

func (t *opsTx) Repair(ctx context.Context, id string) (integration.RepairRecord, error) {
	return readJSON[integration.RepairRecord](ctx, t.session.tx, `SELECT record FROM billing_ops_repairs WHERE account_id=$1 AND repair_id=$2`, string(t.session.account), id)
}

func (t *opsTx) SaveRepair(ctx context.Context, record integration.RepairRecord) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	_, err = t.session.tx.ExecContext(ctx, `INSERT INTO billing_ops_repairs(account_id,repair_id,revision,actor,reason,evidence,fingerprint,record) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`,
		string(t.session.account), record.ID, record.Revision, record.Actor, record.Reason, record.Evidence, record.ID, raw)
	return mapOpsConflict(err)
}

func (t *opsTx) Backfill(ctx context.Context, id string) (integration.BackfillJob, error) {
	return readJSON[integration.BackfillJob](ctx, t.session.tx, `SELECT record FROM billing_ops_backfills WHERE account_id=$1 AND job_id=$2`, string(t.session.account), id)
}

func (t *opsTx) SaveBackfill(ctx context.Context, job integration.BackfillJob) error {
	raw, err := json.Marshal(job)
	if err != nil {
		return err
	}
	_, err = t.session.tx.ExecContext(ctx, `INSERT INTO billing_ops_backfills(account_id,job_id,cursor,processed,state,record) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT (account_id,job_id) DO UPDATE SET cursor=EXCLUDED.cursor,processed=EXCLUDED.processed,state=EXCLUDED.state,record=EXCLUDED.record`,
		string(t.session.account), job.ID, job.Cursor, job.Processed, job.State, raw)
	return mapOpsConflict(err)
}

func (t *opsTx) Tombstone(ctx context.Context, kind, id string) (integration.Tombstone, error) {
	return readJSON[integration.Tombstone](ctx, t.session.tx, `SELECT record FROM billing_ops_tombstones WHERE account_id=$1 AND kind=$2 AND identity=$3`, string(t.session.account), kind, id)
}

func (t *opsTx) SaveTombstone(ctx context.Context, stone integration.Tombstone) error {
	raw, err := json.Marshal(stone)
	if err != nil {
		return err
	}
	_, err = t.session.tx.ExecContext(ctx, `INSERT INTO billing_ops_tombstones(account_id,kind,identity,fingerprint,record) VALUES($1,$2,$3,$4,$5) ON CONFLICT (account_id,kind,identity) DO NOTHING`,
		string(t.session.account), stone.Kind, stone.ID, stone.Fingerprint, raw)
	return mapOpsConflict(err)
}

func (t *opsTx) Tombstones(ctx context.Context, after string, limit int) ([]integration.Tombstone, error) {
	if limit < 1 || limit > 1000 {
		return nil, billing.ErrInvalid
	}
	rows, err := t.session.tx.QueryContext(ctx, `SELECT record FROM billing_ops_tombstones WHERE account_id=$1 AND (kind||':'||identity) COLLATE "C" > $2 ORDER BY kind COLLATE "C", identity COLLATE "C" LIMIT $3`, string(t.session.account), after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]integration.Tombstone, 0)
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var stone integration.Tombstone
		if err := json.Unmarshal(raw, &stone); err != nil {
			return nil, err
		}
		out = append(out, stone)
	}
	return out, rows.Err()
}

func mapOpsConflict(err error) error {
	if err == nil {
		return nil
	}
	if pgerr, ok := errors.AsType[*pgconn.PgError](err); ok && pgerr.Code == "23505" {
		return billing.ErrConflict
	}
	return err
}

var _ integration.OpsRepository = (*opsRepository)(nil)
var _ integration.OpsTx = (*opsTx)(nil)
var _ integration.Inbox = (*Store)(nil)
