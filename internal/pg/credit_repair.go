package pg

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/internal/checked"
	"github.com/data-insights-ai/rho-billing/internal/identity"
)

type (
	CreditRepairRequest    = credit.RepairRequest
	CreditRepairStatus     = credit.RepairStatus
	CreditRepairQuantities = credit.RepairQuantities
	CreditRepairEvidence   = credit.RepairEvidence
)

type creditRepairRecord struct {
	CreditRepairStatus
	fingerprint                           string
	allocationTotal, allocationAuthorized int64
	unitCursor, scopeCursor               string
}

const repairColumns = `repair_id,actor,reason,phase,revision,target_sequence,journal_cursor,lot_cursor,reservation_cursor,allocation_position,replay_lots,verified_lots,changed_lots,applied_lots,created_at,updated_at,fingerprint,allocation_total,allocation_authorized,unit_cursor,scope_cursor`

func readCreditRepair(ctx context.Context, q querier, account billing.AccountID, id string) (creditRepairRecord, error) {
	var r creditRepairRecord
	r.Account = account
	err := q.QueryRowContext(ctx, `SELECT `+repairColumns+` FROM billing_credit_repair_jobs WHERE account_id=$1 AND repair_id=$2`, string(account), id).Scan(&r.ID, &r.Actor, &r.Reason, &r.Phase, &r.Revision, &r.TargetSequence, &r.JournalCursor, &r.LotCursor, &r.ReservationCursor, &r.AllocationPosition, &r.ReplayLots, &r.VerifiedLots, &r.ChangedLots, &r.AppliedLots, &r.CreatedAt, &r.UpdatedAt, &r.fingerprint, &r.allocationTotal, &r.allocationAuthorized, &r.unitCursor, &r.scopeCursor)
	if errors.Is(err, sql.ErrNoRows) {
		return creditRepairRecord{}, billing.ErrNotFound
	}
	return r, err
}

// Recovers the last committed checkpoint, including after an ambiguous COMMIT
// response. It never starts work or changes readiness.
func (s *Store) CreditRepair(ctx context.Context, account billing.AccountID, id string) (CreditRepairStatus, error) {
	if s == nil || s.db == nil || !billing.ValidID(string(account)) || !billing.ValidID(id) {
		return CreditRepairStatus{}, billing.ErrInvalid
	}
	r, err := readCreditRepair(ctx, s.db, account, id)
	if err != nil {
		return CreditRepairStatus{}, err
	}
	return r.CreditRepairStatus, nil
}

func lockCreditRepairAccount(ctx context.Context, tx *sql.Tx, account billing.AccountID) (int64, error) {
	var sequence int64
	err := tx.QueryRowContext(ctx, `SELECT next_journal_sequence FROM billing_accounts WHERE account_id=$1 FOR UPDATE`, string(account)).Scan(&sequence)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, billing.ErrNotFound
	}
	return sequence, err
}

// Freezes credit writes and creates an auditable repair generation. Identical
// requests replay; mismatched requests never take over an existing generation.
// Completed repair history is retained.
func (s *Store) StartCreditRepair(ctx context.Context, in CreditRepairRequest) (CreditRepairStatus, error) {
	if s == nil || s.db == nil || !billing.ValidID(string(in.Account)) || !billing.ValidID(in.ID) || !billing.ValidID(in.Actor) || strings.TrimSpace(in.Reason) == "" || len(in.Reason) > 2000 || in.ExpectedJournalSequence < 0 {
		return CreditRepairStatus{}, billing.ErrInvalid
	}
	fp := identity.Fingerprint("credit-repair", string(in.Account), in.ID, in.Actor, in.Reason, fmt.Sprint(in.ExpectedJournalSequence))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CreditRepairStatus{}, err
	}
	defer tx.Rollback()
	sequence, err := lockCreditRepairAccount(ctx, tx, in.Account)
	if err != nil {
		return CreditRepairStatus{}, err
	}
	old, err := readCreditRepair(ctx, tx, in.Account, in.ID)
	if err == nil {
		if old.fingerprint != fp {
			return CreditRepairStatus{}, billing.ErrConflict
		}
		if err = tx.Commit(); err != nil {
			return CreditRepairStatus{}, err
		}
		return old.CreditRepairStatus, nil
	}
	if !errors.Is(err, billing.ErrNotFound) {
		return CreditRepairStatus{}, err
	}
	if sequence != in.ExpectedJournalSequence {
		return CreditRepairStatus{}, billing.ErrConflict
	}
	var latest int64
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT sequence FROM billing_journal WHERE account_id=$1 ORDER BY sequence DESC LIMIT 1),0)`, string(in.Account)).Scan(&latest); err != nil {
		return CreditRepairStatus{}, err
	}
	if latest != sequence {
		return CreditRepairStatus{}, billing.ErrState
	}
	var active sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT repair_job_id FROM billing_credit_balance_projection_state WHERE account_id=$1`, string(in.Account)).Scan(&active)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return CreditRepairStatus{}, err
	}
	if active.Valid {
		return CreditRepairStatus{}, credit.ErrRepairInProgress
	}
	now := s.now()
	if now.IsZero() {
		return CreditRepairStatus{}, billing.ErrInvalid
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO billing_credit_repair_jobs(account_id,repair_id,fingerprint,actor,reason,phase,target_sequence,created_at,updated_at) VALUES($1,$2,$3,$4,$5,'replay',$6,$7,$7)`, string(in.Account), in.ID, fp, in.Actor, in.Reason, sequence, now)
	if err != nil {
		return CreditRepairStatus{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO billing_credit_balance_projection_state(account_id,last_lot_id,ready,repair_job_id) VALUES($1,NULL,false,$2) ON CONFLICT(account_id) DO UPDATE SET last_lot_id=NULL,ready=false,repair_job_id=EXCLUDED.repair_job_id`, string(in.Account), in.ID)
	if err != nil {
		return CreditRepairStatus{}, err
	}
	r, err := readCreditRepair(ctx, tx, in.Account, in.ID)
	if err != nil {
		return CreditRepairStatus{}, err
	}
	if err = tx.Commit(); err != nil {
		return CreditRepairStatus{}, err
	}
	return r.CreditRepairStatus, nil
}

func (s *Store) advanceCreditRepair(ctx context.Context, account billing.AccountID, id string, revision int64, fn func(*sql.Tx, *creditRepairRecord) (bool, error)) (CreditRepairStatus, error) {
	if s == nil || s.db == nil || !billing.ValidID(string(account)) || !billing.ValidID(id) || revision < 0 {
		return CreditRepairStatus{}, billing.ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CreditRepairStatus{}, err
	}
	defer tx.Rollback()
	sequence, err := lockCreditRepairAccount(ctx, tx, account)
	if err != nil {
		return CreditRepairStatus{}, err
	}
	r, err := readCreditRepair(ctx, tx, account, id)
	if err != nil {
		return CreditRepairStatus{}, err
	}
	if r.Revision != revision {
		return CreditRepairStatus{}, billing.ErrConflict
	}
	if r.Phase != "completed" {
		var active sql.NullString
		if err = tx.QueryRowContext(ctx, `SELECT repair_job_id FROM billing_credit_balance_projection_state WHERE account_id=$1`, string(account)).Scan(&active); err != nil {
			return CreditRepairStatus{}, err
		}
		if !active.Valid || active.String != id || sequence != r.TargetSequence {
			return CreditRepairStatus{}, billing.ErrState
		}
	}
	changed, err := fn(tx, &r)
	if err != nil {
		return CreditRepairStatus{}, err
	}
	if changed {
		r.Revision, err = checked.Add(r.Revision, 1)
		if err != nil {
			return CreditRepairStatus{}, err
		}
		r.UpdatedAt = s.now()
		if r.UpdatedAt.IsZero() {
			return CreditRepairStatus{}, billing.ErrInvalid
		}
		result, err := tx.ExecContext(ctx, `UPDATE billing_credit_repair_jobs SET phase=$3,revision=$4,journal_cursor=$5,lot_cursor=$6,reservation_cursor=$7,allocation_position=$8,replay_lots=$9,verified_lots=$10,changed_lots=$11,applied_lots=$12,updated_at=$13,allocation_total=$14,allocation_authorized=$15,unit_cursor=$16,scope_cursor=$17 WHERE account_id=$1 AND repair_id=$2 AND revision=$18`, string(account), id, r.Phase, r.Revision, r.JournalCursor, r.LotCursor, r.ReservationCursor, r.AllocationPosition, r.ReplayLots, r.VerifiedLots, r.ChangedLots, r.AppliedLots, r.UpdatedAt, r.allocationTotal, r.allocationAuthorized, r.unitCursor, r.scopeCursor, revision)
		if err != nil {
			return CreditRepairStatus{}, err
		}
		if err = expectSettlementRows(result, 1); err != nil {
			return CreditRepairStatus{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return CreditRepairStatus{}, err
	}
	return r.CreditRepairStatus, nil
}

// Approves the verified staged result. Publication proceeds through bounded
// AdvanceCreditRepair calls while ordinary writes remain fenced.
func (s *Store) ApplyCreditRepair(ctx context.Context, account billing.AccountID, id string, revision int64) (CreditRepairStatus, error) {
	return s.advanceCreditRepair(ctx, account, id, revision, func(_ *sql.Tx, r *creditRepairRecord) (bool, error) {
		if r.Phase != "ready" {
			return false, billing.ErrState
		}
		r.Phase = "apply_lots"
		r.LotCursor = ""
		return true, nil
	})
}

// Processes one page. Ready and completed checkpoints are read-only until
// explicitly applied, or already complete, respectively.
func (s *Store) AdvanceCreditRepair(ctx context.Context, account billing.AccountID, id string, revision int64, limit int) (CreditRepairStatus, error) {
	if limit < 1 || limit > 1000 {
		return CreditRepairStatus{}, billing.ErrInvalid
	}
	return s.advanceCreditRepair(ctx, account, id, revision, func(tx *sql.Tx, r *creditRepairRecord) (bool, error) {
		var err error
		switch r.Phase {
		case "replay":
			err = repairReplayPage(ctx, tx, r, limit)
		case "allocations":
			err = repairAllocationPage(ctx, tx, r, limit)
		case "verify":
			err = repairVerifyPage(ctx, tx, r, limit)
		case "ready", "completed":
			return false, nil
		case "apply_lots", "clear_accounts", "clear_scopes", "write_accounts", "write_scopes":
			if _, err = tx.ExecContext(ctx, `SELECT set_config('rho_billing.credit_repair',$1,true)`, r.ID); err == nil {
				err = repairPublishPage(ctx, tx, r, limit)
			}
		default:
			err = billing.ErrState
		}
		return err == nil, err
	})
}

// Returns immutable staged evidence after verification. Earlier phases return
// ErrState rather than expose an unstable page sequence.
func (s *Store) CreditRepairEvidencePage(ctx context.Context, account billing.AccountID, id, after string, limit int) ([]CreditRepairEvidence, string, bool, error) {
	if limit < 1 || limit > 1000 {
		return nil, "", false, billing.ErrInvalid
	}
	status, err := s.CreditRepair(ctx, account, id)
	if err != nil {
		return nil, "", false, err
	}
	if status.Phase == "replay" || status.Phase == "allocations" || status.Phase == "verify" {
		return nil, "", false, billing.ErrState
	}
	rows, err := s.db.QueryContext(ctx, `SELECT lot_id,before_state,after_state FROM billing_credit_repair_evidence WHERE account_id=$1 AND repair_id=$2 AND lot_id>$3 ORDER BY lot_id LIMIT $4`, string(account), id, after, limit+1)
	if err != nil {
		return nil, "", false, err
	}
	defer rows.Close()
	result := make([]CreditRepairEvidence, 0, limit+1)
	for rows.Next() {
		var e CreditRepairEvidence
		var before, afterRaw []byte
		if err = rows.Scan(&e.LotID, &before, &afterRaw); err != nil {
			return nil, "", false, err
		}
		if err = json.Unmarshal(before, &e.Before); err != nil {
			return nil, "", false, err
		}
		if err = json.Unmarshal(afterRaw, &e.After); err != nil {
			return nil, "", false, err
		}
		result = append(result, e)
	}
	if err = rows.Err(); err != nil {
		return nil, "", false, err
	}
	more := len(result) > limit
	if more {
		result = result[:limit]
	}
	next := ""
	if len(result) > 0 {
		next = result[len(result)-1].LotID
	}
	return result, next, more, nil
}
