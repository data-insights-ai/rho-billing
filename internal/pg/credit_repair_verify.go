package pg

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math/big"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/internal/checked"
)

// repairVerifyPage validates one bounded retained-lot page. The journal replay
// and allocation pages have already populated the shadow lot state and its
// allocation_held column. This page only reads retained metadata, records
// immutable evidence, and accumulates exact numeric totals.
func repairVerifyPage(ctx context.Context, tx *sql.Tx, r *creditRepairRecord, limit int) error {
	t := &transaction{tx: tx, account: r.Account, ctx: ctx}
	lots, err := t.lotsWhereOrder("lot_id>$2", "lot_id", limit, r.LotCursor)
	if err != nil {
		return err
	}
	if len(lots) == 0 {
		if r.VerifiedLots != r.ReplayLots {
			return fmt.Errorf("%w: repair verified lot count %d does not match replay count %d", billing.ErrState, r.VerifiedLots, r.ReplayLots)
		}
		r.Phase = "ready"
		return nil
	}
	ids := make([]string, 0, len(lots))
	for _, lot := range lots {
		ids = append(ids, lot.ID)
	}
	shadow, err := repairLotStates(ctx, tx, r.Account, r.ID, ids)
	if err != nil {
		return err
	}
	allocationHeld, err := repairAllocationHeld(ctx, tx, r.Account, r.ID, ids)
	if err != nil {
		return err
	}

	totals := make(map[repairTotalKey]*repairTotal, len(lots)*2)
	evidenceIDs := make([]string, 0, len(lots))
	evidenceBefore := make([]string, 0, len(lots))
	evidenceAfter := make([]string, 0, len(lots))
	for _, lot := range lots {
		state, ok := shadow[lot.ID]
		if !ok {
			return fmt.Errorf("%w: missing staged lot %s", billing.ErrState, lot.ID)
		}
		held, ok := allocationHeld[lot.ID]
		if !ok {
			held = 0
		}
		if held != state.Held {
			return fmt.Errorf("%w: held allocation drift for lot %s", billing.ErrState, lot.ID)
		}
		repaired := credit.Lot{
			ID: lot.ID, Unit: lot.Unit, Scope: lot.Scope, Source: lot.Source, SourceRef: lot.SourceRef,
			ValidFrom: lot.ValidFrom, ExpiresAt: lot.ExpiresAt, GrantedAt: lot.GrantedAt, RevokedAt: lot.RevokedAt,
			Initial: state.Granted, Available: state.Available, Held: state.Held, Consumed: state.Consumed,
			Expired: state.Expired, Revoked: state.Revoked, PendingRevocation: state.PendingRevocation,
		}
		if err := validLot(repaired); err != nil {
			return fmt.Errorf("%w: invalid retained metadata for lot %s: %v", billing.ErrState, lot.ID, err)
		}
		if lot.Initial != repaired.Initial || lot.Available != repaired.Available || lot.Held != repaired.Held || lot.Consumed != repaired.Consumed || lot.Expired != repaired.Expired || lot.Revoked != repaired.Revoked || lot.PendingRevocation != repaired.PendingRevocation {
			r.ChangedLots, err = checked.Add(r.ChangedLots, 1)
			if err != nil {
				return err
			}
		}

		before := CreditRepairQuantities{Initial: lot.Initial, Available: lot.Available, Held: lot.Held, Consumed: lot.Consumed, Expired: lot.Expired, Revoked: lot.Revoked, PendingRevocation: lot.PendingRevocation}
		after := CreditRepairQuantities{Initial: repaired.Initial, Available: repaired.Available, Held: repaired.Held, Consumed: repaired.Consumed, Expired: repaired.Expired, Revoked: repaired.Revoked, PendingRevocation: repaired.PendingRevocation}
		beforeJSON, err := json.Marshal(before)
		if err != nil {
			return err
		}
		afterJSON, err := json.Marshal(after)
		if err != nil {
			return err
		}
		evidenceIDs = append(evidenceIDs, lot.ID)
		evidenceBefore = append(evidenceBefore, string(beforeJSON))
		evidenceAfter = append(evidenceAfter, string(afterJSON))

		addRepairTotal(totals, repairTotalKey{kind: "account", unit: lot.Unit.Code}, repaired)
		addRepairTotal(totals, repairTotalKey{kind: "scope", unit: lot.Unit.Code, scope: lot.Scope}, repaired)
		r.LotCursor = lot.ID
		r.VerifiedLots, err = checked.Add(r.VerifiedLots, 1)
		if err != nil {
			return err
		}
	}
	if err := insertRepairEvidence(ctx, tx, r.Account, r.ID, evidenceIDs, evidenceBefore, evidenceAfter); err != nil {
		return err
	}
	return upsertRepairTotals(ctx, tx, r.Account, r.ID, totals)
}

type repairTotalKey struct{ kind, unit, scope string }

type repairTotal struct {
	available, held, consumed, expired, revoked big.Int
}

func addRepairTotal(totals map[repairTotalKey]*repairTotal, key repairTotalKey, lot credit.Lot) {
	total := totals[key]
	if total == nil {
		total = &repairTotal{}
		totals[key] = total
	}
	add := func(dst *big.Int, value int64) { dst.Add(dst, big.NewInt(value)) }
	add(&total.available, lot.Available)
	add(&total.held, lot.Held)
	add(&total.consumed, lot.Consumed)
	add(&total.expired, lot.Expired)
	add(&total.revoked, lot.Revoked)
}

func upsertRepairTotals(ctx context.Context, tx *sql.Tx, account billing.AccountID, repairID string, totals map[repairTotalKey]*repairTotal) error {
	if len(totals) == 0 {
		return nil
	}
	kinds := make([]string, 0, len(totals))
	units := make([]string, 0, len(totals))
	scopes := make([]string, 0, len(totals))
	available := make([]string, 0, len(totals))
	held := make([]string, 0, len(totals))
	consumed := make([]string, 0, len(totals))
	expired := make([]string, 0, len(totals))
	revoked := make([]string, 0, len(totals))
	for key, total := range totals {
		kinds = append(kinds, key.kind)
		units = append(units, key.unit)
		scopes = append(scopes, key.scope)
		available = append(available, total.available.String())
		held = append(held, total.held.String())
		consumed = append(consumed, total.consumed.String())
		expired = append(expired, total.expired.String())
		revoked = append(revoked, total.revoked.String())
	}
	result, err := tx.ExecContext(ctx, `
		INSERT INTO billing_credit_repair_totals(account_id,repair_id,kind,unit_code,scope,available,held,consumed,expired,revoked)
		SELECT $1,$2,k,u,s,a::numeric,h::numeric,c::numeric,e::numeric,v::numeric
		FROM unnest($3::text[],$4::text[],$5::text[],$6::text[],$7::text[],$8::text[],$9::text[],$10::text[]) AS x(k,u,s,a,h,c,e,v)
		ON CONFLICT(account_id,repair_id,kind,unit_code,scope) DO UPDATE SET
			available=billing_credit_repair_totals.available+EXCLUDED.available,
			held=billing_credit_repair_totals.held+EXCLUDED.held,
			consumed=billing_credit_repair_totals.consumed+EXCLUDED.consumed,
			expired=billing_credit_repair_totals.expired+EXCLUDED.expired,
			revoked=billing_credit_repair_totals.revoked+EXCLUDED.revoked`,
		string(account), repairID, kinds, units, scopes, available, held, consumed, expired, revoked)
	if err != nil {
		return err
	}
	return expectSettlementRows(result, int64(len(totals)))
}

func insertRepairEvidence(ctx context.Context, tx *sql.Tx, account billing.AccountID, repairID string, ids, before, after []string) error {
	if len(ids) == 0 {
		return nil
	}
	result, err := tx.ExecContext(ctx, `
		INSERT INTO billing_credit_repair_evidence(account_id,repair_id,lot_id,before_state,after_state)
		SELECT $1,$2,id,b::jsonb,a::jsonb
		FROM unnest($3::text[],$4::text[],$5::text[]) AS x(id,b,a)
		ON CONFLICT(account_id,repair_id,lot_id) DO NOTHING`,
		string(account), repairID, ids, before, after)
	if err != nil {
		return err
	}
	return expectSettlementRows(result, int64(len(ids)))
}

func repairAllocationHeld(ctx context.Context, tx *sql.Tx, account billing.AccountID, repairID string, ids []string) (map[string]int64, error) {
	result := make(map[string]int64, len(ids))
	if len(ids) == 0 {
		return result, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT lot_id,allocation_held FROM billing_credit_repair_lots WHERE account_id=$1 AND repair_id=$2 AND lot_id=ANY($3::text[])`, string(account), repairID, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var held int64
		if err := rows.Scan(&id, &held); err != nil {
			return nil, err
		}
		result[id] = held
	}
	return result, rows.Err()
}
