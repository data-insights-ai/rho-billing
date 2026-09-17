package pg

import (
	"database/sql"
	"errors"
	"math"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
)

const journalBatchSize = 500

func (t *transaction) Append(entry credit.Entry) error {
	return t.AppendEntries([]credit.Entry{entry})
}

func (t *transaction) AppendEntries(entries []credit.Entry) error {
	for _, entry := range entries {
		if err := validOperation(entry.OperationID); err != nil {
			return err
		}
		if entry.Kind == "" {
			return billing.ErrInvalid
		}
	}
	for start := 0; start < len(entries); start += journalBatchSize {
		if err := t.appendJournalPage(entries[start:min(start+journalBatchSize, len(entries))]); err != nil {
			return err
		}
	}
	return nil
}

func (t *transaction) appendJournalPage(entries []credit.Entry) error {
	var last int64
	err := t.tx.QueryRowContext(t.ctx, `
 UPDATE billing_accounts SET next_journal_sequence=next_journal_sequence+$2
 WHERE account_id=$1 AND next_journal_sequence <= $3
 RETURNING next_journal_sequence`, t.accountID(), len(entries), math.MaxInt64-int64(len(entries))).Scan(&last)
	if errors.Is(err, sql.ErrNoRows) {
		return billing.ErrOverflow
	}
	if err != nil {
		return err
	}
	first := last - int64(len(entries)) + 1
	operations := make([]string, len(entries))
	lots := make([]string, len(entries))
	reservations := make([]string, len(entries))
	kinds := make([]string, len(entries))
	reasons := make([]string, len(entries))
	recorded := make([]time.Time, len(entries))
	effective := make([]time.Time, len(entries))
	available := make([]int64, len(entries))
	held := make([]int64, len(entries))
	consumed := make([]int64, len(entries))
	expired := make([]int64, len(entries))
	revoked := make([]int64, len(entries))
	pending := make([]int64, len(entries))
	for i, entry := range entries {
		if entry.Sequence != 0 && entry.Sequence != first+int64(i) {
			return billing.ErrConflict
		}
		operations[i], lots[i], reservations[i] = string(entry.OperationID), entry.LotID, entry.ReservationID
		kinds[i], reasons[i] = entry.Kind, entry.Reason
		recorded[i], effective[i] = databaseTime(entry.RecordedAt), databaseTime(entry.EffectiveAt)
		available[i], held[i], consumed[i] = entry.Available, entry.Held, entry.Consumed
		expired[i], revoked[i], pending[i] = entry.Expired, entry.Revoked, entry.PendingRevocation
	}
	_, err = t.tx.ExecContext(t.ctx, `
 INSERT INTO billing_journal(account_id,sequence,operation_id,lot_id,reservation_id,
 kind,reason,recorded_at,effective_at,available_delta,held_delta,consumed_delta,
 expired_delta,revoked_delta,pending_revocation_delta)
 SELECT $1,$2::bigint+(ordinality-1),operation_id,NULLIF(lot_id,''),NULLIF(reservation_id,''),
 kind,reason,recorded_at,effective_at,available,held,consumed,expired,revoked,pending
 FROM unnest($3::text[],$4::text[],$5::text[],$6::text[],$7::text[],
 $8::timestamptz[],$9::timestamptz[],$10::bigint[],$11::bigint[],$12::bigint[],
 $13::bigint[],$14::bigint[],$15::bigint[]) WITH ORDINALITY AS entries(
 operation_id,lot_id,reservation_id,kind,reason,recorded_at,effective_at,
 available,held,consumed,expired,revoked,pending,ordinality)
 ORDER BY ordinality`, t.accountID(), first, operations, lots, reservations, kinds, reasons,
		recorded, effective, available, held, consumed, expired, revoked, pending)
	return err
}
