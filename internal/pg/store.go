package pg

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/internal/checked"
	"github.com/data-insights-ai/rho-billing/internal/migrate"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Store is PostgreSQL persistence for the billing domains.
type Store struct {
	db    *sql.DB
	clock func() time.Time
}

// The caller owns db's lifetime and driver registration; the store never opens
// or closes a database connection.
func New(db *sql.DB) *Store { return NewWithClock(db, time.Now) }

// Injects the domain workflow clock. Queue lease fencing deliberately uses the
// database clock so worker clock skew cannot extend ownership.
func NewWithClock(db *sql.DB, clock func() time.Time) *Store {
	if clock == nil {
		clock = time.Now
	}
	return &Store{db: db, clock: clock}
}
func (s *Store) now() time.Time { return billing.CanonicalTime(s.clock()) }

// Applies the embedded schema as one transaction. The advisory lock serializes
// migrations started by multiple application processes.
func (s *Store) Migrate(ctx context.Context) error { return s.migrate(ctx, migrationFiles) }

func (s *Store) migrate(ctx context.Context, source fs.FS) error {
	if s == nil || s.db == nil {
		return billing.ErrInvalid
	}
	return migrate.Run(ctx, s.db, source)
}

// Repeating the same account and subject is idempotent; reusing either identity
// with different data is a conflict.
func (s *Store) CreateAccount(ctx context.Context, account billing.AccountID, subject string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("%w: nil database", billing.ErrInvalid)
	}
	if !billing.ValidID(string(account)) || !billing.ValidID(subject) {
		return fmt.Errorf("%w: account and subject are required", billing.ErrInvalid)
	}
	result, err := s.db.ExecContext(ctx,
		`INSERT INTO billing_accounts (account_id, subject) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
		string(account), subject,
	)
	if err != nil {
		return err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if inserted != 0 {
		return nil
	}
	var existingSubject string
	err = s.db.QueryRowContext(ctx,
		`SELECT subject FROM billing_accounts WHERE account_id = $1`, string(account),
	).Scan(&existingSubject)
	if err == nil {
		if existingSubject == subject {
			return nil
		}
		return fmt.Errorf("%w: account already belongs to another subject", billing.ErrConflict)
	}
	if err != sql.ErrNoRows {
		return err
	}
	return fmt.Errorf("%w: subject already belongs to another account", billing.ErrConflict)
}

// Locks the account row and commits writes made through the transaction only if fn returns nil.
func (s *Store) WithinAccount(ctx context.Context, account billing.AccountID, fn func(credit.Tx) error) error {
	if s == nil || s.db == nil || fn == nil {
		return fmt.Errorf("%w: nil database or callback", billing.ErrInvalid)
	}
	if !billing.ValidID(string(account)) {
		return fmt.Errorf("%w: invalid account", billing.ErrInvalid)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var found string
	if err := tx.QueryRowContext(ctx,
		`SELECT account_id FROM billing_accounts WHERE account_id = $1 FOR UPDATE`, string(account),
	).Scan(&found); err != nil {
		_ = tx.Rollback()
		if errors.Is(err, sql.ErrNoRows) {
			return billing.ErrNotFound
		}
		return err
	}
	view := &transaction{tx: tx, account: account, ctx: ctx, now: s.now}
	if err := fn(view); err != nil {
		_ = tx.Rollback()
		return mapCreditRepairError(err)
	}
	if err := tx.Commit(); err != nil {
		return mapCreditRepairError(err)
	}
	return nil
}

func (s *Store) History(ctx context.Context, account billing.AccountID, afterSequence int64, limit int) ([]credit.Entry, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("%w: nil database", billing.ErrInvalid)
	}
	if !billing.ValidID(string(account)) || afterSequence < 0 || limit < 1 || limit > 1000 {
		return nil, fmt.Errorf("%w: invalid history cursor or limit", billing.ErrInvalid)
	}
	var exists bool
	if err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM billing_accounts WHERE account_id = $1)`, string(account),
	).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, billing.ErrNotFound
	}
	return journalPage(ctx, s.db, account, afterSequence, limit)
}

func (t *transaction) History(afterSequence int64, limit int) ([]credit.Entry, error) {
	if afterSequence < 0 || limit < 1 || limit > 1000 {
		return nil, billing.ErrInvalid
	}
	return journalPage(t.ctx, t.tx, t.account, afterSequence, limit)
}

func journalPage(ctx context.Context, q querier, account billing.AccountID, afterSequence int64, limit int) ([]credit.Entry, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT sequence, operation_id, lot_id, reservation_id, kind, reason,
		       recorded_at, effective_at, available_delta, held_delta,
		       consumed_delta, expired_delta, revoked_delta, pending_revocation_delta
		FROM billing_journal
		WHERE account_id = $1 AND sequence > $2
		ORDER BY sequence ASC
		LIMIT $3`, string(account), afterSequence, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := make([]credit.Entry, 0, limit)
	for rows.Next() {
		var entry credit.Entry
		var lotID, reservationID sql.NullString
		if err := rows.Scan(
			&entry.Sequence, &entry.OperationID, &lotID, &reservationID,
			&entry.Kind, &entry.Reason, &entry.RecordedAt, &entry.EffectiveAt,
			&entry.Available, &entry.Held, &entry.Consumed, &entry.Expired, &entry.Revoked, &entry.PendingRevocation,
		); err != nil {
			return nil, err
		}
		if lotID.Valid {
			entry.LotID = lotID.String
		}
		if reservationID.Valid {
			entry.ReservationID = reservationID.String
		}
		entry.RecordedAt = billing.CanonicalTime(entry.RecordedAt)
		entry.EffectiveAt = billing.CanonicalTime(entry.EffectiveAt)
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

type transaction struct {
	tx      *sql.Tx
	account billing.AccountID
	ctx     context.Context
	now     func() time.Time
}

func (t *transaction) accountID() string { return string(t.account) }

func validOperation(operation billing.OperationID) error {
	if !billing.ValidID(string(operation)) {
		return fmt.Errorf("%w: invalid operation ID", billing.ErrInvalid)
	}
	return nil
}

func validLot(lot credit.Lot) error {
	if !billing.ValidID(lot.ID) || !lot.Unit.Valid() || !billing.ValidID(lot.Source) || !billing.ValidID(lot.SourceRef) || lot.ValidFrom.IsZero() || (!lot.ExpiresAt.IsZero() && !lot.ExpiresAt.After(lot.ValidFrom)) {
		return fmt.Errorf("%w: invalid lot identity, unit, or validity interval", billing.ErrInvalid)
	}
	if lot.PendingRevocation < 0 || lot.PendingRevocation > lot.Held || lot.Initial < 0 || lot.Available < 0 || lot.Held < 0 || lot.Consumed < 0 || lot.Expired < 0 || lot.Revoked < 0 {
		return fmt.Errorf("%w: negative lot quantity", billing.ErrInvalid)
	}
	sum := lot.Available
	for _, value := range []int64{lot.Held, lot.Consumed, lot.Expired, lot.Revoked} {
		var err error
		sum, err = checked.Add(sum, value)
		if err != nil {
			return err
		}
	}
	if sum != lot.Initial {
		return fmt.Errorf("%w: lot quantities do not conserve", billing.ErrInvalid)
	}
	return nil
}

func nullTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return databaseTime(value)
}

// PostgreSQL timestamptz stores microsecond precision. Canonicalizing at the
// boundary prevents a read-after-write value from changing a domain
// fingerprint solely because nanoseconds were truncated by the database.
func databaseTime(value time.Time) time.Time { return billing.CanonicalTime(value) }

func nullString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func scanTime(value sql.NullTime) time.Time {
	if !value.Valid {
		return time.Time{}
	}
	return value.Time.UTC()
}

func (t *transaction) Lots() ([]credit.Lot, error) { return t.lotsWhere("TRUE") }
func (t *transaction) LiveLots() ([]credit.Lot, error) {
	if err := ensureCreditBalanceReady(t.ctx, t.tx, t.account); err != nil {
		return nil, err
	}
	return t.lotsWhere("available > 0 OR held > 0 OR pending_revocation > 0")
}
func (t *transaction) DueLots(at time.Time, limit int) ([]credit.Lot, error) {
	if at.IsZero() || limit < 1 || limit > 1001 {
		return nil, billing.ErrInvalid
	}
	return t.lotsWhereOrder("available > 0 AND expires_at IS NOT NULL AND expires_at <= $2", "expires_at, lot_id", limit, databaseTime(at))
}
func (t *transaction) EligibleLots(unit, scope string, at time.Time, limit int) ([]credit.Lot, error) {
	if !billing.ValidID(unit) || at.IsZero() || limit < 1 || limit > 1001 {
		return nil, billing.ErrInvalid
	}
	predicate := "unit_code=$2 AND available > 0 AND valid_from <= $3 AND (expires_at IS NULL OR expires_at > $3) AND revoked_at IS NULL"
	args := []any{unit, databaseTime(at)}
	if scope != "" {
		predicate = "unit_code=$2 AND (scope='' OR scope=$3) AND available > 0 AND valid_from <= $4 AND (expires_at IS NULL OR expires_at > $4) AND revoked_at IS NULL"
		args = []any{unit, scope, databaseTime(at)}
	}
	return t.lotsWhereOrder(predicate, "expires_at ASC NULLS LAST, granted_at, lot_id", limit, args...)
}
func (t *transaction) Lot(id string) (credit.Lot, bool, error) {
	rows, err := t.lotsWhere("lot_id = $2", id)
	if err != nil || len(rows) == 0 {
		return credit.Lot{}, false, err
	}
	return rows[0], true, nil
}
func (t *transaction) LotsByID(ids []string) ([]credit.Lot, error) {
	if len(ids) > 1000 {
		return nil, credit.ErrAllocationBudget
	}
	if len(ids) == 0 {
		return nil, nil
	}
	return t.lotsWhereOrder("lot_id = ANY($2::text[])", "lot_id", 0, ids)
}
func (t *transaction) LotBySource(unit, source, reference string) (credit.Lot, bool, error) {
	rows, err := t.lotsWhere("unit_code=$2 AND source=$3 AND source_ref=$4", unit, source, reference)
	if err != nil || len(rows) == 0 {
		return credit.Lot{}, false, err
	}
	return rows[0], true, nil
}
func (t *transaction) StoredBalance(unit, scope string, at time.Time) (credit.Balance, error) {
	return storedCreditBalanceAt(t.ctx, t.tx, t.account, unit, scope, at)
}

func (t *transaction) lotsWhere(predicate string, args ...any) ([]credit.Lot, error) {
	return t.lotsWhereOrder(predicate, "lot_id", 0, args...)
}

func (t *transaction) lotsWhereOrder(predicate, order string, limit int, args ...any) ([]credit.Lot, error) {
	query := `
		SELECT lot_id, unit_code, unit_scale, scope, source, source_ref,
		       valid_from, expires_at, granted_at, revoked_at, initial,
		       available, held, consumed, expired, revoked, pending_revocation
		FROM billing_lots WHERE account_id = $1 AND (` + predicate + `) ORDER BY ` + order
	queryArgs := append([]any{t.accountID()}, args...)
	if limit > 0 {
		query += ` LIMIT $` + fmt.Sprint(len(queryArgs)+1)
		queryArgs = append(queryArgs, limit)
	}
	rows, err := t.tx.QueryContext(t.ctx, query, queryArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	lots := make([]credit.Lot, 0)
	for rows.Next() {
		var lot credit.Lot
		var expiresAt, revokedAt sql.NullTime
		if err := rows.Scan(
			&lot.ID, &lot.Unit.Code, &lot.Unit.Scale, &lot.Scope, &lot.Source, &lot.SourceRef,
			&lot.ValidFrom, &expiresAt, &lot.GrantedAt, &revokedAt, &lot.Initial,
			&lot.Available, &lot.Held, &lot.Consumed, &lot.Expired, &lot.Revoked, &lot.PendingRevocation,
		); err != nil {
			return nil, err
		}
		lot.ExpiresAt = scanTime(expiresAt)
		lot.RevokedAt = scanTime(revokedAt)
		lots = append(lots, lot)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return lots, nil
}

func (t *transaction) PutLot(lot credit.Lot) error {
	if err := validLot(lot); err != nil {
		return err
	}
	if err := t.EnsureUnit(lot.Unit); err != nil {
		return err
	}
	_, err := t.tx.ExecContext(t.ctx, `
		INSERT INTO billing_lots (
			account_id, lot_id, unit_code, unit_scale, scope, source, source_ref,
			valid_from, expires_at, granted_at, revoked_at, initial, available,
			held, consumed, expired, revoked, pending_revocation
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)
		ON CONFLICT (account_id, lot_id) DO UPDATE SET
			unit_code = EXCLUDED.unit_code, unit_scale = EXCLUDED.unit_scale,
			scope = EXCLUDED.scope, source = EXCLUDED.source, source_ref = EXCLUDED.source_ref,
			valid_from = EXCLUDED.valid_from, expires_at = EXCLUDED.expires_at,
			granted_at = EXCLUDED.granted_at, revoked_at = EXCLUDED.revoked_at,
			initial = EXCLUDED.initial, available = EXCLUDED.available,
			held = EXCLUDED.held, consumed = EXCLUDED.consumed,
			expired = EXCLUDED.expired, revoked = EXCLUDED.revoked, pending_revocation = EXCLUDED.pending_revocation`,
		t.accountID(), lot.ID, lot.Unit.Code, lot.Unit.Scale, lot.Scope, lot.Source, lot.SourceRef,
		databaseTime(lot.ValidFrom), nullTime(lot.ExpiresAt), databaseTime(lot.GrantedAt), nullTime(lot.RevokedAt),
		lot.Initial, lot.Available, lot.Held, lot.Consumed, lot.Expired, lot.Revoked, lot.PendingRevocation,
	)
	return err
}

func (t *transaction) EnsureUnit(unit billing.Unit) error {
	return ensureUnit(t.ctx, t.tx, unit)
}

const creditUnitRegistryLockKey = "rho-billing:credit-unit-registry-v1"

// Checks the common case without locking. If a unit is new, all creators
// serialize on one transaction-level registry lock before rechecking and
// inserting. A single global lock avoids opposing-order deadlocks when one
// transaction registers several new units.
func ensureUnit(ctx context.Context, q querier, unit billing.Unit) error {
	if !unit.Valid() {
		return billing.ErrInvalid
	}
	var storedScale int64
	err := q.QueryRowContext(ctx, `SELECT unit_scale FROM billing_credit_units WHERE unit_code=$1`, unit.Code).Scan(&storedScale)
	if err == nil {
		if storedScale != unit.Scale {
			return fmt.Errorf("%w: credit unit scale is immutable", billing.ErrConflict)
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err := q.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, creditUnitRegistryLockKey); err != nil {
		return err
	}
	if err := q.QueryRowContext(ctx, `SELECT unit_scale FROM billing_credit_units WHERE unit_code=$1`, unit.Code).Scan(&storedScale); err == nil {
		if storedScale != unit.Scale {
			return fmt.Errorf("%w: credit unit scale is immutable", billing.ErrConflict)
		}
		return nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err := q.ExecContext(ctx, `INSERT INTO billing_credit_units (unit_code, unit_scale) VALUES ($1, $2) ON CONFLICT (unit_code) DO NOTHING`, unit.Code, unit.Scale); err != nil {
		return err
	}
	if err := q.QueryRowContext(ctx, `SELECT unit_scale FROM billing_credit_units WHERE unit_code=$1`, unit.Code).Scan(&storedScale); err != nil {
		return err
	}
	if storedScale != unit.Scale {
		return fmt.Errorf("%w: credit unit scale is immutable", billing.ErrConflict)
	}
	return nil
}

func (t *transaction) Reservations() ([]credit.Reservation, error) {
	return t.reservations(`
		SELECT reservation_id, actor, unit, scope, created_at, deadline, state,
		       authorized, consumed, limit_period_start,
		       limit_period_end, evidence
		FROM billing_reservations WHERE account_id = $1 ORDER BY reservation_id`, t.accountID())

}

func (t *transaction) ActiveReservations() ([]credit.Reservation, error) {
	return t.reservations(`
		SELECT reservation_id, actor, unit, scope, created_at, deadline, state,
		       authorized, consumed, limit_period_start,
		       limit_period_end, evidence
		FROM billing_reservations WHERE account_id = $1 AND state = 'held' ORDER BY reservation_id`, t.accountID())
}

func (t *transaction) Reservation(id string) (credit.Reservation, bool, error) {
	reservations, err := t.reservations(`
		SELECT reservation_id, actor, unit, scope, created_at, deadline, state,
		       authorized, consumed, limit_period_start,
		       limit_period_end, evidence
		FROM billing_reservations WHERE account_id = $1 AND reservation_id = $2`, t.accountID(), id)
	if err != nil {
		return credit.Reservation{}, false, err
	}
	if len(reservations) == 0 {
		return credit.Reservation{}, false, nil
	}
	return reservations[0], true, nil
}

func (t *transaction) reservations(query string, args ...any) ([]credit.Reservation, error) {
	rows, err := t.tx.QueryContext(t.ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	reservations := make([]credit.Reservation, 0)
	for rows.Next() {
		var reservation credit.Reservation
		var evidence []byte
		var periodStart, periodEnd sql.NullTime
		if err := rows.Scan(
			&reservation.ID, &reservation.Actor, &reservation.Unit, &reservation.Scope,
			&reservation.CreatedAt, &reservation.Deadline, &reservation.State,
			&reservation.Authorized, &reservation.Consumed,
			&periodStart, &periodEnd, &evidence,
		); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(evidence, &reservation.Evidence); err != nil {
			return nil, fmt.Errorf("decode reservation evidence: %w", err)
		}
		if periodStart.Valid || periodEnd.Valid {
			if !periodStart.Valid || !periodEnd.Valid {
				return nil, fmt.Errorf("%w: malformed stored limit period", billing.ErrInvalid)
			}
			reservation.LimitPeriod = billing.Period{Start: periodStart.Time.UTC(), End: periodEnd.Time.UTC()}
		}
		reservation.CreatedAt = billing.CanonicalTime(reservation.CreatedAt)
		reservation.Deadline = billing.CanonicalTime(reservation.Deadline)
		reservations = append(reservations, reservation)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	// Fetch allocation pages rather than issuing one query for each reservation.
	// Position, not lot identity, is authoritative: extensions may allocate the
	// same lot more than once. Both sides remain scoped to this transaction's account.
	const allocationBatchSize = 500
	for offset := 0; offset < len(reservations); offset += allocationBatchSize {
		end := min(offset+allocationBatchSize, len(reservations))
		ids := make([]string, 0, end-offset)
		positions := make(map[string]int, end-offset)
		for i := offset; i < end; i++ {
			ids = append(ids, reservations[i].ID)
			positions[reservations[i].ID] = i
		}
		if err := t.loadAllocations(reservations, ids, positions); err != nil {
			return nil, err
		}
	}
	return reservations, nil
}

func (t *transaction) loadAllocations(reservations []credit.Reservation, ids []string, positions map[string]int) error {
	rows, err := t.tx.QueryContext(t.ctx, `
		SELECT reservation_id, lot_id, amount FROM billing_reservation_allocations
		WHERE account_id = $1 AND reservation_id = ANY($2::text[])
		ORDER BY reservation_id, position`, t.accountID(), ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var allocation credit.Allocation
		if err := rows.Scan(&id, &allocation.LotID, &allocation.Amount); err != nil {
			return err
		}
		index, ok := positions[id]
		if !ok {
			return fmt.Errorf("%w: allocation outside requested reservation page", billing.ErrInvalid)
		}
		reservations[index].Allocations = append(reservations[index].Allocations, allocation)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return rows.Close()
}

func (t *transaction) HasUsage(usageID string) (bool, error) {
	var found bool
	err := t.tx.QueryRowContext(t.ctx, `SELECT EXISTS (
		SELECT 1 FROM billing_reservations
		WHERE account_id = $1 AND evidence->>'UsageID' <> '' AND evidence->>'UsageID' = $2
	)`, t.accountID(), usageID).Scan(&found)
	return found, err
}

func (t *transaction) SettledForLimit(actor, unit string, period billing.Period) (int64, error) {
	var total int64
	err := t.tx.QueryRowContext(t.ctx, `SELECT COALESCE(SUM(consumed), 0)
		FROM billing_reservations
		WHERE account_id = $1 AND actor = $2 AND unit = $3 AND state = 'settled'
		  AND created_at >= $4 AND created_at < $5`,
		t.accountID(), actor, unit, databaseTime(period.Start), databaseTime(period.End)).Scan(&total)
	return total, err
}

func validReservation(reservation credit.Reservation) error {
	if !billing.ValidID(reservation.ID) || reservation.CreatedAt.IsZero() || reservation.Deadline.IsZero() || reservation.State == "" || reservation.Authorized < 0 || reservation.Consumed < 0 || reservation.Consumed > reservation.Authorized {
		return fmt.Errorf("%w: invalid reservation", billing.ErrInvalid)
	}
	switch reservation.State {
	case credit.ReservationHeld, credit.ReservationSettled, credit.ReservationReleased, credit.ReservationTimedOut:
	default:
		return fmt.Errorf("%w: invalid reservation state", billing.ErrInvalid)
	}
	if reservation.LimitPeriod.Start.IsZero() != reservation.LimitPeriod.End.IsZero() || (!reservation.LimitPeriod.Start.IsZero() && !reservation.LimitPeriod.Valid()) {
		return fmt.Errorf("%w: invalid reservation limit period", billing.ErrInvalid)
	}
	return nil
}

func (t *transaction) PutReservation(reservation credit.Reservation) error {
	if err := validReservation(reservation); err != nil {
		return err
	}
	evidence, err := json.Marshal(reservation.Evidence)
	if err != nil {
		return fmt.Errorf("encode reservation evidence: %w", err)
	}
	_, err = t.tx.ExecContext(t.ctx, `
		INSERT INTO billing_reservations (
			account_id, reservation_id, actor, unit, scope, created_at, deadline,
			state, authorized, consumed, limit_period_start,
			limit_period_end, evidence
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		ON CONFLICT (account_id, reservation_id) DO UPDATE SET
			actor = EXCLUDED.actor, unit = EXCLUDED.unit, scope = EXCLUDED.scope,
			created_at = EXCLUDED.created_at, deadline = EXCLUDED.deadline,
			state = EXCLUDED.state, authorized = EXCLUDED.authorized,
			consumed = EXCLUDED.consumed,
			limit_period_start = EXCLUDED.limit_period_start,
			limit_period_end = EXCLUDED.limit_period_end, evidence = EXCLUDED.evidence`,
		t.accountID(), reservation.ID, reservation.Actor, reservation.Unit, reservation.Scope,
		databaseTime(reservation.CreatedAt), databaseTime(reservation.Deadline), reservation.State,
		reservation.Authorized, reservation.Consumed,
		nullTime(reservation.LimitPeriod.Start), nullTime(reservation.LimitPeriod.End), evidence,
	)
	if err != nil {
		return err
	}
	if _, err := t.tx.ExecContext(t.ctx, `
		DELETE FROM billing_reservation_allocations
		WHERE account_id = $1 AND reservation_id = $2`, t.accountID(), reservation.ID); err != nil {
		return err
	}
	if len(reservation.Allocations) == 0 {
		return nil
	}
	lotIDs := make([]string, len(reservation.Allocations))
	amounts := make([]int64, len(reservation.Allocations))
	for i, allocation := range reservation.Allocations {
		if !billing.ValidID(allocation.LotID) || allocation.Amount <= 0 {
			return fmt.Errorf("%w: invalid reservation allocation", billing.ErrInvalid)
		}
		lotIDs[i], amounts[i] = allocation.LotID, allocation.Amount
	}
	_, err = t.tx.ExecContext(t.ctx, `
		INSERT INTO billing_reservation_allocations (account_id, reservation_id, position, lot_id, amount)
		SELECT $1, $2, position - 1, lot_id, amount
		FROM unnest($3::text[], $4::bigint[]) WITH ORDINALITY AS allocations(lot_id, amount, position)`,
		t.accountID(), reservation.ID, lotIDs, amounts)
	if err != nil {
		return err
	}
	return nil
}

func (t *transaction) Limits() ([]credit.Limit, error) {
	rows, err := t.tx.QueryContext(t.ctx, `
		SELECT actor, unit, period_start, period_end, amount
		FROM billing_limits WHERE account_id = $1
		ORDER BY actor, unit, period_start`, t.accountID())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	limits := make([]credit.Limit, 0)
	for rows.Next() {
		var limit credit.Limit
		if err := rows.Scan(&limit.Actor, &limit.Unit, &limit.Period.Start, &limit.Period.End, &limit.Amount); err != nil {
			return nil, err
		}
		limits = append(limits, limit)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return limits, nil
}

func (t *transaction) LimitAt(actor, unit string, at time.Time) (credit.Limit, bool, error) {
	var l credit.Limit
	err := t.tx.QueryRowContext(t.ctx, `
		SELECT actor, unit, period_start, period_end, amount
		FROM billing_limits
		WHERE account_id=$1 AND actor=$2 AND unit=$3 AND period_start <= $4 AND period_end > $4`,
		t.accountID(), actor, unit, databaseTime(at)).Scan(&l.Actor, &l.Unit, &l.Period.Start, &l.Period.End, &l.Amount)
	l.Period = billing.Period{Start: l.Period.Start.UTC(), End: l.Period.End.UTC()}
	if errors.Is(err, sql.ErrNoRows) {
		return credit.Limit{}, false, nil
	}
	if err != nil {
		return credit.Limit{}, false, err
	}
	return l, true, nil
}

func (t *transaction) LimitsOverlapping(actor, unit string, period billing.Period) ([]credit.Limit, error) {
	rows, err := t.tx.QueryContext(t.ctx, `
		SELECT actor, unit, period_start, period_end, amount
		FROM billing_limits
		WHERE account_id=$1 AND actor=$2 AND unit=$3 AND period_start < $5 AND $4 < period_end
		ORDER BY period_start LIMIT 3`, t.accountID(), actor, unit, databaseTime(period.Start), databaseTime(period.End))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]credit.Limit, 0, 2)
	for rows.Next() {
		var l credit.Limit
		if err := rows.Scan(&l.Actor, &l.Unit, &l.Period.Start, &l.Period.End, &l.Amount); err != nil {
			return nil, err
		}
		l.Period = billing.Period{Start: l.Period.Start.UTC(), End: l.Period.End.UTC()}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) > 2 {
		return nil, billing.ErrConflict
	}
	return out, nil
}

func (t *transaction) PutLimit(limit credit.Limit) error {
	if !billing.ValidID(limit.Actor) || !billing.ValidID(limit.Unit) || !limit.Period.Valid() || limit.Amount < 0 {
		return fmt.Errorf("%w: invalid spending limit", billing.ErrInvalid)
	}
	_, err := t.tx.ExecContext(t.ctx, `
		INSERT INTO billing_limits (account_id, actor, unit, period_start, period_end, amount)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (account_id, actor, unit, period_start) DO UPDATE SET
			period_end = EXCLUDED.period_end, amount = EXCLUDED.amount`,
		t.accountID(), limit.Actor, limit.Unit, databaseTime(limit.Period.Start), databaseTime(limit.Period.End), limit.Amount,
	)
	return err
}

func (t *transaction) Outcome(operation billing.OperationID) (credit.Outcome, bool, error) {
	if err := validOperation(operation); err != nil {
		return credit.Outcome{}, false, err
	}
	var outcome credit.Outcome
	var result []byte
	err := t.tx.QueryRowContext(t.ctx, `
		SELECT fingerprint, error, result FROM billing_operations
		WHERE account_id = $1 AND operation_id = $2`, t.accountID(), string(operation),
	).Scan(&outcome.Fingerprint, &outcome.Error, &result)
	if errors.Is(err, sql.ErrNoRows) {
		return credit.Outcome{}, false, nil
	}
	if err != nil {
		return credit.Outcome{}, false, err
	}
	if err := json.Unmarshal(result, &outcome.Result); err != nil {
		return credit.Outcome{}, false, fmt.Errorf("decode operation result: %w", err)
	}
	return outcome, true, nil
}

func (t *transaction) PutOutcome(operation billing.OperationID, outcome credit.Outcome) error {
	if err := validOperation(operation); err != nil {
		return err
	}
	if outcome.Fingerprint == "" {
		return fmt.Errorf("%w: empty outcome fingerprint", billing.ErrInvalid)
	}
	result, err := json.Marshal(outcome.Result)
	if err != nil {
		return fmt.Errorf("encode operation result: %w", err)
	}
	inserted, err := t.tx.ExecContext(t.ctx, `
		INSERT INTO billing_operations (account_id, operation_id, fingerprint, error, result)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (account_id, operation_id) DO NOTHING`,
		t.accountID(), string(operation), outcome.Fingerprint, outcome.Error, result,
	)
	if err != nil {
		return err
	}
	count, err := inserted.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return billing.ErrConflict
	}
	return nil
}

var _ credit.Repository = (*Store)(nil)
var _ credit.Tx = (*transaction)(nil)
