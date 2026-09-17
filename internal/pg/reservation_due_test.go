package pg

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
)

type reservationDueQueryCounter struct {
	allocationReads  atomic.Int64
	reservationReads atomic.Int64
	withoutDeadline  atomic.Int64
}

func (c *reservationDueQueryCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	sql := strings.ToLower(data.SQL)
	if strings.Contains(sql, "from billing_reservation_allocations") {
		c.allocationReads.Add(1)
	}
	if strings.Contains(sql, "from billing_reservations") {
		c.reservationReads.Add(1)
		if !strings.Contains(sql, "deadline") {
			c.withoutDeadline.Add(1)
		}
	}
	return ctx
}

func (*reservationDueQueryCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestPostgresDueReservationsAreOrderedAndBounded(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	const account = billing.AccountID("due-page")
	if err := store.CreateAccount(ctx, account, "due-page-subject"); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO billing_reservations (
			account_id, reservation_id, actor, unit, scope, created_at, deadline,
			state, authorized, consumed, limit_period_start, limit_period_end, evidence
		)
		SELECT $1, 'due-' || lpad(i::text, 4, '0'), 'actor', 'credits', $3, $2::timestamptz,
			$2::timestamptz - interval '1 minute' + (i % 3) * interval '1 second',
			'held', 0, 0, NULL, NULL, '{}'::jsonb
		FROM generate_series(0, 1000) AS values(i)`, account, now, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO billing_reservations (
			account_id, reservation_id, actor, unit, scope, created_at, deadline,
			state, authorized, consumed, limit_period_start, limit_period_end, evidence
		)
		SELECT $1, 'future-' || lpad(i::text, 2, '0'), 'actor', 'credits', '', $2::timestamptz,
			$2::timestamptz + interval '1 hour', 'held', 0, 0, NULL, NULL, '{}'::jsonb
		FROM generate_series(0, 9) AS values(i)`, account, now); err != nil {
		t.Fatal(err)
	}

	if err := store.WithinAccount(ctx, account, func(tx credit.Tx) error {
		rows, err := tx.DueReservations(now, 1000)
		if err != nil {
			return err
		}
		if len(rows) != 1000 {
			t.Fatalf("due page length=%d, want 1000", len(rows))
		}
		for i, row := range rows {
			if row.State != "held" || row.Deadline.After(now) {
				t.Fatalf("row %d is not due held reservation: %+v", i, row)
			}
			if i == 0 {
				continue
			}
			previous := rows[i-1]
			if row.Deadline.Before(previous.Deadline) ||
				(row.Deadline.Equal(previous.Deadline) && row.ID < previous.ID) {
				t.Fatalf("page is not ordered by deadline, reservation ID: previous=%+v row=%+v", previous, row)
			}
		}
		all, err := tx.DueReservations(now, 1001)
		if err != nil {
			return err
		}
		if len(all) != 1001 || all[len(all)-1].ID != "due-0998" {
			t.Fatalf("all due rows=%d last=%q, want 1001 and due-0998", len(all), all[len(all)-1].ID)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresCommittedForLimitExcludesDueHeldReservations(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	const account = billing.AccountID("due-cap")
	if err := store.CreateAccount(ctx, account, "due-cap-subject"); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	period := billing.Period{Start: now.Add(-time.Hour), End: now.Add(time.Hour)}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO billing_reservations (
			account_id, reservation_id, actor, unit, scope, created_at, deadline,
			state, authorized, consumed, limit_period_start, limit_period_end, evidence
		) VALUES
			($1, 'due-held', 'actor', 'credits', '', $2::timestamptz, $2::timestamptz, 'held', 7, 0, NULL, NULL, '{}'::jsonb),
			($1, 'future-held', 'actor', 'credits', '', $2::timestamptz, $2::timestamptz + interval '1 hour', 'held', 11, 0, NULL, NULL, '{}'::jsonb),
			($1, 'settled', 'actor', 'credits', '', $2::timestamptz, $2::timestamptz, 'settled', 3, 3, NULL, NULL, '{}'::jsonb),
			($1, 'other-actor', 'other', 'credits', '', $2::timestamptz, $2::timestamptz + interval '1 hour', 'held', 100, 0, NULL, NULL, '{}'::jsonb),
			($1, 'outside-period', 'actor', 'credits', '', $2::timestamptz - interval '2 hours', $2::timestamptz + interval '1 hour', 'held', 200, 0, NULL, NULL, '{}'::jsonb)`, account, now); err != nil {
		t.Fatal(err)
	}
	if err := store.WithinAccount(ctx, account, func(tx credit.Tx) error {
		got, err := tx.CommittedForLimit("actor", "credits", period, now)
		if err != nil {
			return err
		}
		if got != 14 {
			t.Fatalf("committed cap=%d, want settled 3 plus future held 11", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresBalanceUsesDueQueryForFutureHeldReservations(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	const account = billing.AccountID("future-query")
	if err := store.CreateAccount(ctx, account, "future-query-subject"); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	if _, err := db.ExecContext(ctx, `INSERT INTO billing_credit_units(unit_code,unit_scale) VALUES ('credits',1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO billing_lots (
			account_id, lot_id, unit_code, unit_scale, scope, source, source_ref,
			valid_from, expires_at, granted_at, revoked_at, initial, available,
			held, consumed, expired, revoked, pending_revocation
		) VALUES ($1, 'future-lot', 'credits', 1, '', 'fixture', 'future-query',
			$2::timestamptz + interval '1 hour', NULL, $2::timestamptz + interval '1 hour',
			NULL, 10, 3, 7, 0, 0, 0, 0)`, account, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO billing_reservations (
			account_id, reservation_id, actor, unit, scope, created_at, deadline,
			state, authorized, consumed, limit_period_start, limit_period_end, evidence
		) VALUES ($1, 'future-hold', 'actor', 'credits', '', $2::timestamptz,
			$2::timestamptz + interval '1 hour', 'held', 7, 0, NULL, NULL, '{}'::jsonb)`, account, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO billing_reservation_allocations(account_id,reservation_id,position,lot_id,amount)
		VALUES ($1, 'future-hold', 0, 'future-lot', 7)`, account); err != nil {
		t.Fatal(err)
	}
	var schema string
	if err := db.QueryRowContext(ctx, "SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	config, err := pgx.ParseConfig(os.Getenv("BILLING_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	config.RuntimeParams["search_path"] = schema
	counter := new(reservationDueQueryCounter)
	config.Tracer = counter
	traced := stdlib.OpenDB(*config)
	t.Cleanup(func() { _ = traced.Close() })
	if _, err := credit.New(New(traced), func() time.Time { return now }).Balance(ctx, account, "credits", ""); err != nil {
		t.Fatal(err)
	}
	if counter.reservationReads.Load() == 0 {
		t.Fatal("balance did not issue a due reservation query")
	}
	if counter.withoutDeadline.Load() != 0 {
		t.Fatalf("balance issued %d reservation query without deadline predicate", counter.withoutDeadline.Load())
	}
	if counter.allocationReads.Load() != 0 {
		t.Fatalf("balance loaded %d allocation queries for future holds", counter.allocationReads.Load())
	}
}

func TestPostgresMaintenanceRefusalSweepAndOriginalGrantRetry(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	const account = billing.AccountID("maintenance-pg")
	if err := store.CreateAccount(ctx, account, "maintenance-pg-subject"); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	// These rows intentionally have zero authorization and no allocations. They
	// exercise maintenance control flow without making a ledger conservation
	// claim about a synthetic allocation fixture.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO billing_reservations (
			account_id, reservation_id, actor, unit, scope, created_at, deadline,
			state, authorized, consumed, limit_period_start, limit_period_end, evidence
		)
		SELECT $1, 'maintenance-' || lpad(i::text, 4, '0'), 'maintenance', 'credits', '',
			$2::timestamptz - interval '1 hour', $2::timestamptz - interval '1 minute', 'held', 0, 0,
			NULL, NULL, '{}'::jsonb
		FROM generate_series(0, 1000) AS values(i)`, account, now); err != nil {
		t.Fatal(err)
	}

	engine := credit.New(store, func() time.Time { return now })
	input := credit.GrantInput{
		Account: account, Operation: "maintenance-grant", LotID: "maintenance-lot",
		Unit: billing.Unit{Code: "credits", Scale: 1000}, Amount: 1,
		Source: "maintenance", SourceRef: "maintenance-grant", ValidFrom: now,
	}
	if _, err := engine.Grant(ctx, input); !errors.Is(err, credit.ErrMaintenanceRequired) || credit.IsRejection(err) {
		t.Fatalf("grant error=%v, rejection=%v; want non-rejection maintenance error", err, credit.IsRejection(err))
	}
	if err := store.WithinAccount(ctx, account, func(tx credit.Tx) error {
		_, found, err := tx.Outcome(input.Operation)
		if err != nil {
			return err
		}
		if found {
			t.Fatal("maintenance failure persisted a grant outcome")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	first, err := engine.Sweep(ctx, account)
	if err != nil {
		t.Fatal(err)
	}
	if first.Reservations != 1000 || !first.HasMore {
		t.Fatalf("first sweep=%+v, want 1000 reservations and more", first)
	}
	second, err := engine.Sweep(ctx, account)
	if err != nil {
		t.Fatal(err)
	}
	if second.Reservations != 1 || second.HasMore {
		t.Fatalf("second sweep=%+v, want one reservation and complete", second)
	}
	third, err := engine.Sweep(ctx, account)
	if err != nil {
		t.Fatal(err)
	}
	if third.Reservations != 0 || third.HasMore {
		t.Fatalf("repeat sweep=%+v, want zero and complete", third)
	}
	if _, err := engine.Grant(ctx, input); err != nil {
		t.Fatalf("grant after maintenance: %v", err)
	}
	retry, err := engine.Grant(ctx, input)
	if err != nil {
		t.Fatalf("original grant retry: %v", err)
	}
	if retry.LotID != "maintenance-lot" || retry.Balance.Available != 1 {
		t.Fatalf("retry result=%+v, want original lot and balance", retry)
	}

	if err := store.WithinAccount(ctx, account, func(tx credit.Tx) error {
		rows, err := tx.Reservations()
		if err != nil {
			return err
		}
		for _, row := range rows {
			if row.State != "timed_out" {
				t.Fatalf("reservation remained active after sweep: %+v", row)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
