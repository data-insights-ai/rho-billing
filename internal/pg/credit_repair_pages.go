package pg

import (
	"context"
	"database/sql"
	"fmt"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/internal/checked"
	"github.com/data-insights-ai/rho-billing/internal/creditledger"
)

func repairReplayPage(ctx context.Context, tx *sql.Tx, r *creditRepairRecord, limit int) error {
	rows, err := tx.QueryContext(ctx, `SELECT sequence,lot_id,kind,available_delta,held_delta,consumed_delta,expired_delta,revoked_delta,pending_revocation_delta FROM billing_journal WHERE account_id=$1 AND sequence>$2 AND sequence<=$3 ORDER BY sequence LIMIT $4`, string(r.Account), r.JournalCursor, r.TargetSequence, limit)
	if err != nil {
		return err
	}
	defer rows.Close()
	type item struct {
		seq int64
		id  string
		d   creditledger.Delta
	}
	items := make([]item, 0, limit)
	ids := make([]string, 0, limit)
	for rows.Next() {
		var x item
		if err := rows.Scan(&x.seq, &x.id, &x.d.Kind, &x.d.Available, &x.d.Held, &x.d.Consumed, &x.d.Expired, &x.d.Revoked, &x.d.PendingRevocation); err != nil {
			return err
		}
		if x.id == "" {
			return fmt.Errorf("%w: repair journal lot missing", billing.ErrState)
		}
		items = append(items, x)
		ids = append(ids, x.id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(items) == 0 {
		if r.JournalCursor != r.TargetSequence {
			return fmt.Errorf("%w: repair journal sequence gap", billing.ErrState)
		}
		r.Phase = "allocations"
		return nil
	}
	states, err := repairLotStates(ctx, tx, r.Account, r.ID, ids)
	if err != nil {
		return err
	}
	newLots := make(map[string]struct{})
	expected := r.JournalCursor + 1
	for _, x := range items {
		if x.seq != expected {
			return fmt.Errorf("%w: repair journal sequence gap", billing.ErrState)
		}
		expected++
		if _, exists := states[x.id]; !exists {
			newLots[x.id] = struct{}{}
		}
		state, err := creditledger.Apply(states[x.id], x.d)
		if err != nil {
			return fmt.Errorf("%w for lot %s", err, x.id)
		}
		states[x.id] = state
		r.JournalCursor = x.seq
	}
	if err := upsertRepairLotStates(ctx, tx, r.Account, r.ID, states); err != nil {
		return err
	}
	var countErr error
	r.ReplayLots, countErr = checked.Add(r.ReplayLots, int64(len(newLots)))
	if countErr != nil {
		return countErr
	}
	return nil
}

func repairLotStates(ctx context.Context, tx *sql.Tx, account billing.AccountID, repairID string, ids []string) (map[string]creditledger.State, error) {
	out := make(map[string]creditledger.State, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT lot_id,granted,available,held,consumed,expired,revoked,pending_revocation FROM billing_credit_repair_lots WHERE account_id=$1 AND repair_id=$2 AND lot_id=ANY($3::text[])`, string(account), repairID, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var s creditledger.State
		if err := rows.Scan(&id, &s.Granted, &s.Available, &s.Held, &s.Consumed, &s.Expired, &s.Revoked, &s.PendingRevocation); err != nil {
			return nil, err
		}
		out[id] = s
	}
	return out, rows.Err()
}

func upsertRepairLotStates(ctx context.Context, tx *sql.Tx, account billing.AccountID, repairID string, states map[string]creditledger.State) error {
	if len(states) == 0 {
		return nil
	}
	ids := make([]string, 0, len(states))
	granted := make([]int64, 0, len(states))
	available := make([]int64, 0, len(states))
	held := make([]int64, 0, len(states))
	consumed := make([]int64, 0, len(states))
	expired := make([]int64, 0, len(states))
	revoked := make([]int64, 0, len(states))
	pending := make([]int64, 0, len(states))
	for id, s := range states {
		ids = append(ids, id)
		granted = append(granted, s.Granted)
		available = append(available, s.Available)
		held = append(held, s.Held)
		consumed = append(consumed, s.Consumed)
		expired = append(expired, s.Expired)
		revoked = append(revoked, s.Revoked)
		pending = append(pending, s.PendingRevocation)
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO billing_credit_repair_lots(account_id,repair_id,lot_id,granted,available,held,consumed,expired,revoked,pending_revocation) SELECT $1,$2,* FROM unnest($3::text[],$4::bigint[],$5::bigint[],$6::bigint[],$7::bigint[],$8::bigint[],$9::bigint[],$10::bigint[]) ON CONFLICT(account_id,repair_id,lot_id) DO UPDATE SET granted=EXCLUDED.granted,available=EXCLUDED.available,held=EXCLUDED.held,consumed=EXCLUDED.consumed,expired=EXCLUDED.expired,revoked=EXCLUDED.revoked,pending_revocation=EXCLUDED.pending_revocation`, string(account), repairID, ids, granted, available, held, consumed, expired, revoked, pending)
	if err != nil {
		return err
	}
	return expectSettlementRows(result, int64(len(ids)))
}

// repairAllocationPage handles one reservation page. Choosing the current or
// next reservation first keeps the allocation position predicate indexable.
func repairAllocationPage(ctx context.Context, tx *sql.Tx, r *creditRepairRecord, limit int) error {
	rows, err := tx.QueryContext(ctx, `WITH candidates AS MATERIALIZED (
 SELECT account_id,reservation_id,authorized,unit,scope FROM billing_reservations r
 WHERE account_id=$1 AND state='held' AND reservation_id=$2
 AND EXISTS (SELECT 1 FROM billing_reservation_allocations a WHERE a.account_id=$1 AND a.reservation_id=$2 AND a.position>$3)
 UNION ALL
 (SELECT account_id,reservation_id,authorized,unit,scope FROM billing_reservations
 WHERE account_id=$1 AND state='held' AND reservation_id>$2 ORDER BY reservation_id LIMIT 1)
), chosen AS (SELECT * FROM candidates ORDER BY reservation_id LIMIT 1)
SELECT r.reservation_id,r.authorized,r.unit,r.scope,a.position,a.lot_id,a.amount,l.unit_code,l.scope
FROM chosen r LEFT JOIN LATERAL (
 SELECT position,lot_id,amount FROM billing_reservation_allocations
 WHERE account_id=r.account_id AND reservation_id=r.reservation_id
 AND position>CASE WHEN r.reservation_id=$2 THEN $3 ELSE -1 END
 ORDER BY position LIMIT $4
) a ON true
LEFT JOIN billing_lots l ON l.account_id=r.account_id AND l.lot_id=a.lot_id
ORDER BY a.position LIMIT $4`, string(r.Account), r.ReservationCursor, r.AllocationPosition, limit)
	if err != nil {
		return err
	}
	defer rows.Close()
	type allocation struct {
		reservation                  string
		authorized, position, amount int64
		unit, scope, lot             string
		hasPosition, hasLot          bool
	}
	items := make([]allocation, 0, limit)
	ids := make([]string, 0, limit)
	for rows.Next() {
		var x allocation
		var position, amount sql.NullInt64
		var lot, lotUnit, lotScope sql.NullString
		if err := rows.Scan(&x.reservation, &x.authorized, &x.unit, &x.scope, &position, &lot, &amount, &lotUnit, &lotScope); err != nil {
			return err
		}
		if position.Valid {
			x.hasPosition = true
			x.position = position.Int64
		}
		if lot.Valid {
			x.hasLot = true
			x.lot = lot.String
		}
		if amount.Valid {
			x.amount = amount.Int64
		}
		if x.hasLot {
			if !position.Valid || !amount.Valid || amount.Int64 <= 0 {
				return billing.ErrState
			}
			if !lotUnit.Valid || !lotScope.Valid || lotUnit.String != x.unit || (lotScope.String != "" && lotScope.String != x.scope) {
				return fmt.Errorf("%w: allocation lot metadata mismatch", billing.ErrState)
			}
		}
		if !x.hasLot && x.authorized != 0 {
			return fmt.Errorf("%w: held reservation has missing allocation", billing.ErrState)
		}
		items = append(items, x)
		if x.hasLot {
			ids = append(ids, x.lot)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(items) == 0 {
		if r.ReservationCursor != "" && r.allocationTotal != r.allocationAuthorized {
			return fmt.Errorf("%w: reservation allocation drift", billing.ErrState)
		}
		r.Phase = "verify"
		r.ReservationCursor = ""
		r.AllocationPosition = -1
		return nil
	}
	states, err := repairLotStates(ctx, tx, r.Account, r.ID, ids)
	if err != nil {
		return err
	}
	amounts := make(map[string]int64)
	for _, x := range items {
		if r.ReservationCursor != "" && x.reservation != r.ReservationCursor {
			if r.allocationTotal != r.allocationAuthorized {
				return fmt.Errorf("%w: reservation allocation drift", billing.ErrState)
			}
			r.allocationTotal, r.allocationAuthorized = 0, 0
			if x.hasPosition && x.position != 0 {
				return fmt.Errorf("%w: reservation allocation position gap", billing.ErrState)
			}
		} else if r.ReservationCursor == "" {
			if x.hasPosition && x.position != 0 {
				return fmt.Errorf("%w: reservation allocation position gap", billing.ErrState)
			}
		} else if x.hasPosition && x.position != r.AllocationPosition+1 {
			return fmt.Errorf("%w: reservation allocation position gap", billing.ErrState)
		}
		if x.hasLot {
			if _, ok := states[x.lot]; !ok {
				return billing.ErrState
			}
		} else if x.amount != 0 {
			return billing.ErrState
		}
		var addErr error
		r.allocationTotal, addErr = checked.Add(r.allocationTotal, x.amount)
		if addErr != nil {
			return addErr
		}
		r.allocationAuthorized = x.authorized
		r.ReservationCursor = x.reservation
		if x.hasPosition {
			r.AllocationPosition = x.position
		} else {
			r.AllocationPosition = -1
		}
		if x.hasLot {
			amounts[x.lot], addErr = checked.Add(amounts[x.lot], x.amount)
			if addErr != nil {
				return addErr
			}
		}
	}
	if len(items) < limit && r.allocationTotal != r.allocationAuthorized {
		return fmt.Errorf("%w: reservation allocation drift", billing.ErrState)
	}
	if err := upsertRepairAllocationHeld(ctx, tx, r.Account, r.ID, amounts); err != nil {
		return err
	}
	return nil
}

func upsertRepairAllocationHeld(ctx context.Context, tx *sql.Tx, account billing.AccountID, repairID string, amounts map[string]int64) error {
	if len(amounts) == 0 {
		return nil
	}
	ids := make([]string, 0, len(amounts))
	values := make([]int64, 0, len(amounts))
	for id, amount := range amounts {
		ids = append(ids, id)
		values = append(values, amount)
	}
	res, err := tx.ExecContext(ctx, `UPDATE billing_credit_repair_lots l SET allocation_held=l.allocation_held+x.amount FROM unnest($1::text[],$2::bigint[]) x(lot_id,amount) WHERE l.account_id=$3 AND l.repair_id=$4 AND l.lot_id=x.lot_id`, ids, values, string(account), repairID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != int64(len(ids)) {
		return billing.ErrState
	}
	return nil
}
