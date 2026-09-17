package pg

import (
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
)

func TestReservationBatchReadsPreserveScopeOrderAndAudit(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	for _, account := range []struct {
		id      billing.AccountID
		subject string
	}{{"acct-batch-a", "subject-batch-a"}, {"acct-batch-b", "subject-batch-b"}} {
		if err := store.CreateAccount(ctx, account.id, account.subject); err != nil {
			t.Fatal(err)
		}
	}

	const totalReservations = 1002
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	deadline := now.Add(time.Hour)
	activeCount := 0
	terminalConsumed := int64(0)
	for i := range totalReservations {
		if i%2 == 0 {
			activeCount++
		} else if !(i%2 == 1 && i%11 == 1) {
			terminalConsumed += 2
		}
	}
	activeHeld := int64(activeCount * 3)
	initial := activeHeld + terminalConsumed

	if _, err := db.ExecContext(ctx, `INSERT INTO billing_credit_units (unit_code, unit_scale) VALUES ('credits', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO billing_lots (
			account_id, lot_id, unit_code, unit_scale, scope, source, source_ref,
			valid_from, expires_at, granted_at, revoked_at, initial, available,
			held, consumed, expired, revoked, pending_revocation
		) VALUES ('acct-batch-a', 'lot-batch', 'credits', 1, '', 'fixture', 'batch-a',
			$1, NULL, $1, NULL, $2, 0, $3, $4, 0, 0, 0)`,
		now, initial, activeHeld, terminalConsumed); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO billing_lots (
			account_id, lot_id, unit_code, unit_scale, scope, source, source_ref,
			valid_from, expires_at, granted_at, revoked_at, initial, available,
			held, consumed, expired, revoked, pending_revocation
		) VALUES ('acct-batch-b', 'lot-batch', 'credits', 1, '', 'fixture', 'batch-b',
			$1, NULL, $1, NULL, 3, 0, 3, 0, 0, 0, 0)`, now); err != nil {
		t.Fatal(err)
	}

	if _, err := db.ExecContext(ctx, `
		INSERT INTO billing_journal (
			account_id, sequence, operation_id, lot_id, reservation_id, kind, reason,
			recorded_at, effective_at, available_delta, held_delta,
			consumed_delta, expired_delta, revoked_delta, pending_revocation_delta
		) VALUES ('acct-batch-a', 1, 'grant-batch', 'lot-batch', NULL, 'grant', 'fixture',
			$1, $1, $2, 0, 0, 0, 0, 0)`, now, initial); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO billing_journal (
			account_id, sequence, operation_id, lot_id, reservation_id, kind, reason,
			recorded_at, effective_at, available_delta, held_delta,
			consumed_delta, expired_delta, revoked_delta, pending_revocation_delta
		) VALUES ('acct-batch-a', 2, 'reserve-batch', 'lot-batch', NULL, 'reserve', 'fixture',
			$1, $1, $2, $3, 0, 0, 0, 0)`, now, -activeHeld, activeHeld); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO billing_journal (
			account_id, sequence, operation_id, lot_id, reservation_id, kind, reason,
			recorded_at, effective_at, available_delta, held_delta,
			consumed_delta, expired_delta, revoked_delta, pending_revocation_delta
		) VALUES ('acct-batch-a', 3, 'settle-batch', 'lot-batch', NULL, 'settle', 'fixture',
			$1, $1, $2, 0, $3, 0, 0, 0)`, now, -terminalConsumed, terminalConsumed); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE billing_accounts SET next_journal_sequence = 3 WHERE account_id = 'acct-batch-a'`); err != nil {
		t.Fatal(err)
	}

	if _, err := db.ExecContext(ctx, `
		INSERT INTO billing_reservations (
			account_id, reservation_id, actor, unit, scope, created_at, deadline,
			state, authorized, consumed, limit_period_start, limit_period_end, evidence
		)
		SELECT 'acct-batch-a', 'res-' || lpad(i::text, 4, '0'), 'actor-a', 'credits', 'scope-a',
			$1, $2,
			CASE WHEN i % 2 = 1 AND i % 11 = 1 THEN 'released'
			     WHEN i % 2 = 0 THEN 'held'
			     ELSE 'settled' END,
			CASE WHEN i % 2 = 1 AND i % 11 = 1 THEN 0
			     WHEN i % 2 = 0 THEN 3
			     ELSE 2 END,
			CASE WHEN i % 2 = 1 AND i % 11 = 1 THEN 0
			     WHEN i % 2 = 0 THEN 0
			     ELSE 2 END,
			NULL, NULL, '{}'::jsonb
		FROM generate_series(0, $3) AS values(i)`, now, deadline, totalReservations-1); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO billing_reservation_allocations (account_id, reservation_id, position, lot_id, amount)
		SELECT 'acct-batch-a', 'res-' || lpad(i::text, 4, '0'), positions.position, 'lot-batch',
			CASE WHEN i % 2 = 0 THEN CASE WHEN positions.position = 0 THEN 1 ELSE 2 END ELSE 1 END
		FROM generate_series(0, $1) AS values(i)
		CROSS JOIN generate_series(0, 1) AS positions(position)
		WHERE NOT (i % 2 = 1 AND i % 11 = 1)`, totalReservations-1); err != nil {
		t.Fatal(err)
	}

	// Reuse the reservation and lot identifiers in another account to prove that
	// every allocation batch and point lookup remains account-scoped.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO billing_reservations (
			account_id, reservation_id, actor, unit, scope, created_at, deadline,
			state, authorized, consumed, limit_period_start, limit_period_end, evidence
		) VALUES ('acct-batch-b', 'res-0500', 'actor-b', 'credits', 'scope-b',
			$1, $2, 'held', 3, 0, NULL, NULL, '{}'::jsonb)`, now, deadline); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO billing_reservation_allocations
			(account_id, reservation_id, position, lot_id, amount)
		VALUES ('acct-batch-b', 'res-0500', 0, 'lot-batch', 1),
		       ('acct-batch-b', 'res-0500', 1, 'lot-batch', 2)`); err != nil {
		t.Fatal(err)
	}

	var activeSeen int
	if err := store.WithinAccount(ctx, "acct-batch-a", func(tx credit.Tx) error {
		all, err := tx.Reservations()
		if err != nil {
			return err
		}
		if len(all) != totalReservations {
			t.Fatalf("all reservations=%d, want %d", len(all), totalReservations)
		}
		if all[0].ID != "res-0000" || all[totalReservations-1].ID != "res-1001" {
			t.Fatalf("reservation order=%q .. %q", all[0].ID, all[totalReservations-1].ID)
		}
		for _, reservation := range all {
			if reservation.State == "held" {
				activeSeen++
			}
		}

		active, err := tx.ActiveReservations()
		if err != nil {
			return err
		}
		if len(active) != activeCount {
			t.Fatalf("active reservations=%d, want %d", len(active), activeCount)
		}
		for _, reservation := range active {
			if reservation.State != "held" {
				t.Fatalf("terminal reservation returned as active: %+v", reservation)
			}
		}

		point, found, err := tx.Reservation("res-0500")
		if err != nil {
			return err
		}
		if !found || point.State != "held" || len(point.Allocations) != 2 {
			t.Fatalf("point reservation=%+v, found=%v", point, found)
		}
		wantAllocations := []credit.Allocation{{LotID: "lot-batch", Amount: 1}, {LotID: "lot-batch", Amount: 2}}
		for i, want := range wantAllocations {
			if point.Allocations[i] != want {
				t.Fatalf("allocation %d=%+v, want %+v", i, point.Allocations[i], want)
			}
		}
		point.Allocations[0].Amount = 999
		pointAgain, found, err := tx.Reservation("res-0500")
		if err != nil {
			return err
		}
		if !found || pointAgain.Allocations[0].Amount != 1 {
			t.Fatalf("point lookup exposed mutable allocation state: %+v", pointAgain)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if activeSeen != activeCount {
		t.Fatalf("held reservations in audit read=%d, want %d", activeSeen, activeCount)
	}

	audit, err := credit.New(store, func() time.Time { return now }).VerifyLedger(ctx, "acct-batch-a")
	if err != nil {
		t.Fatal(err)
	}
	if audit.Lots != 1 || audit.Reservations != totalReservations || audit.Entries != 3 {
		t.Fatalf("audit=%+v, want one lot, %d reservations, three entries", audit, totalReservations)
	}

	if err := store.WithinAccount(ctx, "acct-batch-b", func(tx credit.Tx) error {
		reservation, found, err := tx.Reservation("res-0500")
		if err != nil {
			return err
		}
		if !found || reservation.Actor != "actor-b" || reservation.Scope != "scope-b" {
			t.Fatalf("cross-account point lookup=%+v, found=%v", reservation, found)
		}
		if len(reservation.Allocations) != 2 || reservation.Allocations[0].Amount != 1 || reservation.Allocations[1].Amount != 2 {
			t.Fatalf("cross-account allocations=%+v", reservation.Allocations)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
