package pg

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/billingtest"
	"github.com/data-insights-ai/rho-billing/credit"
)

func testStore(t testing.TB) (*Store, *sql.DB) {
	t.Helper()
	return testStorePool(t, 1)
}

func testLoadStore(t testing.TB) (*Store, *sql.DB) {
	t.Helper()
	return testStorePool(t, billingtest.WorkloadDatabasePool)
}

func testStorePool(t testing.TB, pool int) (*Store, *sql.DB) {
	t.Helper()
	store, db := testStoreWithMigrations(t, migrationFiles)
	if pool < 1 {
		pool = 1
	}
	db.SetMaxOpenConns(pool)
	db.SetMaxIdleConns(pool)
	return store, db
}

func testStoreWithMigrations(t testing.TB, source fs.FS) (*Store, *sql.DB) {
	t.Helper()
	dsn := os.Getenv("BILLING_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("BILLING_TEST_DATABASE_URL is not set")
	}
	ctx := t.Context()
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.PingContext(ctx); err != nil {
		admin.Close()
		t.Fatalf("configured PostgreSQL is unavailable: %v", err)
	}
	schema := fmt.Sprintf("billing_test_%d", time.Now().UnixNano())
	if _, err := admin.ExecContext(ctx, `CREATE SCHEMA `+schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+schema+` CASCADE`)
		_ = admin.Close()
	})
	db := openSearchPathPool(t, dsn, schema, 1)
	store := New(db)
	if err := store.migrate(ctx, source); err != nil {
		t.Fatal(err)
	}
	return store, db
}

func openSearchPathPool(t testing.TB, dsn, schema string, pool int) *sql.DB {
	t.Helper()
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if schema != "" {
		cfg.RuntimeParams["search_path"] = schema
	}
	db := stdlib.OpenDB(*cfg)
	if pool < 1 {
		pool = 1
	}
	db.SetMaxOpenConns(pool)
	db.SetMaxIdleConns(pool)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func testStoreAt(t testing.TB, now time.Time) (*Store, *sql.DB) {
	t.Helper()
	_, db := testStore(t)
	return NewWithClock(db, func() time.Time { return now }), db
}

func testLot(id string, scale int64) credit.Lot {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	return credit.Lot{
		ID: id, Unit: billing.Unit{Code: "credits", Scale: scale},
		Source: "test", SourceRef: id, ValidFrom: now, ExpiresAt: now.Add(time.Hour), GrantedAt: now,
		Initial: 10, Available: 10,
	}
}

func TestMigrationRepeatAndAccountIsolation(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateAccount(ctx, "acct-a", "subject-a"); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateAccount(ctx, "acct-b", "subject-b"); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateAccount(ctx, "acct-a", "subject-a"); err != nil {
		t.Fatal(err)
	}
	if err := store.WithinAccount(ctx, "acct-a", func(tx credit.Tx) error {
		if err := tx.PutLot(testLot("lot-a", 100)); err != nil {
			return err
		}
		return tx.Append(credit.Entry{OperationID: "op-a", LotID: "lot-a", Kind: "grant", RecordedAt: time.Now().UTC(), EffectiveAt: time.Now().UTC(), Available: 10})
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.WithinAccount(ctx, "acct-b", func(tx credit.Tx) error {
		lots, err := tx.Lots()
		if err != nil {
			return err
		}
		if len(lots) != 0 {
			t.Fatalf("account B saw account A lots: %+v", lots)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	history, err := store.History(ctx, "acct-a", 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || history[0].Sequence != 1 {
		t.Fatalf("unexpected history: %+v", history)
	}
	if _, err := store.History(ctx, "acct-a", 0, 0); !errors.Is(err, billing.ErrInvalid) {
		t.Fatalf("expected invalid limit, got %v", err)
	}
}

func TestRollbackAndImmutableOutcome(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	if err := store.CreateAccount(ctx, "acct-a", "subject-a"); err != nil {
		t.Fatal(err)
	}
	want := errors.New("abort transaction")
	err := store.WithinAccount(ctx, "acct-a", func(tx credit.Tx) error {
		if err := tx.PutLot(testLot("lot-a", 100)); err != nil {
			return err
		}
		if err := tx.Append(credit.Entry{OperationID: "op-a", LotID: "lot-a", Kind: "grant"}); err != nil {
			return err
		}
		return want
	})
	if !errors.Is(err, want) {
		t.Fatalf("got %v, want callback error", err)
	}
	if err := store.WithinAccount(ctx, "acct-a", func(tx credit.Tx) error {
		lots, err := tx.Lots()
		if err != nil {
			return err
		}
		if len(lots) != 0 {
			t.Fatalf("rollback retained lots: %+v", lots)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	outcome := credit.Outcome{Fingerprint: "fingerprint-a", Result: credit.Result{LotID: "lot-a"}}
	if err := store.WithinAccount(ctx, "acct-a", func(tx credit.Tx) error {
		return tx.PutOutcome("op-a", outcome)
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.WithinAccount(ctx, "acct-a", func(tx credit.Tx) error {
		return tx.PutOutcome("op-a", credit.Outcome{Fingerprint: "different"})
	}); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("expected immutable outcome conflict, got %v", err)
	}
	if err := store.WithinAccount(ctx, "acct-a", func(tx credit.Tx) error {
		got, found, err := tx.Outcome("op-a")
		if err != nil {
			return err
		}
		if !found || got.Fingerprint != outcome.Fingerprint || got.Result.LotID != "lot-a" {
			t.Fatalf("outcome changed: found=%v outcome=%+v", found, got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUnitScaleAndAllocationForeignKey(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	if err := store.CreateAccount(ctx, "acct-a", "subject-a"); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateAccount(ctx, "acct-b", "subject-b"); err != nil {
		t.Fatal(err)
	}
	if err := store.WithinAccount(ctx, "acct-a", func(tx credit.Tx) error {
		return tx.PutLot(testLot("lot-a", 100))
	}); err != nil {
		t.Fatal(err)
	}
	reservation := credit.Reservation{
		ID: "reservation-a", Actor: "actor-a", Unit: "credits", Scope: "project-a",
		CreatedAt: time.Now().UTC(), Deadline: time.Now().UTC().Add(time.Hour), State: "held",
		Authorized: 3, Allocations: []credit.Allocation{{LotID: "lot-a", Amount: 3}},
		Evidence: credit.Evidence{UsageID: "usage-a", RatingVersion: "rule-a", Metrics: []credit.Metric{{Name: "tokens", Quantity: 3}}},
	}
	if err := store.WithinAccount(ctx, "acct-a", func(tx credit.Tx) error {
		return tx.PutReservation(reservation)
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.WithinAccount(ctx, "acct-a", func(tx credit.Tx) error {
		reservations, err := tx.Reservations()
		if err != nil {
			return err
		}
		if len(reservations) != 1 || len(reservations[0].Allocations) != 1 || reservations[0].Allocations[0] != reservation.Allocations[0] || reservations[0].Evidence.UsageID != reservation.Evidence.UsageID {
			t.Fatalf("reservation projection changed: %+v", reservations)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.WithinAccount(ctx, "acct-b", func(tx credit.Tx) error {
		return tx.PutLot(testLot("lot-b", 200))
	}); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("expected immutable unit scale conflict, got %v", err)
	}
	err := store.WithinAccount(ctx, "acct-b", func(tx credit.Tx) error {
		return tx.PutReservation(credit.Reservation{
			ID: "reservation-b", CreatedAt: time.Now().UTC(), Deadline: time.Now().UTC().Add(time.Hour),
			State: "held", Authorized: 1, Allocations: []credit.Allocation{{LotID: "lot-a", Amount: 1}},
		})
	})
	if err == nil {
		t.Fatal("foreign allocation unexpectedly succeeded")
	}
}

func TestConcurrentAccountLocking(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	if err := store.CreateAccount(ctx, "acct-a", "subject-a"); err != nil {
		t.Fatal(err)
	}
	if err := store.WithinAccount(ctx, "acct-a", func(tx credit.Tx) error {
		return tx.PutLot(testLot("lot-a", 100))
	}); err != nil {
		t.Fatal(err)
	}
	// Use a second one-connection pool so both operations have independent
	// PostgreSQL sessions while retaining the fixture's isolated search path.
	// The schema is configured on db's single session and is copied into the
	// second session before it is used.
	db2, err := sql.Open("pgx", os.Getenv("BILLING_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	db2.SetMaxOpenConns(1)
	db2.SetMaxIdleConns(1)
	defer db2.Close()
	// Discover the fixture schema from the current connection and configure the
	// second pool with the same search path via the backend setting.
	var schema string
	if err := db.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	if _, err := db2.ExecContext(ctx, `SET search_path TO `+schema); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- store.WithinAccount(ctx, "acct-a", func(tx credit.Tx) error {
			if err := tx.PutLot(testLot("lot-b", 100)); err != nil {
				return err
			}
			close(started)
			<-release
			return nil
		})
	}()
	<-started
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- New(db2).WithinAccount(ctx, "acct-a", func(tx credit.Tx) error {
			lots, err := tx.Lots()
			if err != nil {
				return err
			}
			if len(lots) != 2 {
				return fmt.Errorf("second transaction observed %d lots, want 2", len(lots))
			}
			return nil
		})
	}()
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
}

func TestEngineGrantReserveSettleWithPostgres(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	if err := store.CreateAccount(ctx, "acct-engine", "subject-engine"); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	unit := billing.Unit{Code: "credits", Scale: 100}
	engine := credit.New(store, func() time.Time { return now })
	if _, err := engine.Grant(ctx, credit.GrantInput{
		Account: "acct-engine", Operation: "grant-engine", LotID: "lot-engine", Unit: unit,
		Amount: 10, Source: "purchase", SourceRef: "payment-engine",
		ValidFrom: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Reserve(ctx, credit.ReserveInput{
		Account: "acct-engine", Operation: "reserve-engine", ReservationID: "reservation-engine",
		Actor: "actor-engine", Unit: unit.Code, Amount: 6, Deadline: now.Add(30 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	result, err := engine.Settle(ctx, credit.SettleInput{
		Account: "acct-engine", Operation: "settle-engine", ReservationID: "reservation-engine",
		Actual: 4, Evidence: credit.Evidence{
			UsageID: "usage-engine", RatingVersion: "rating-engine",
			Metrics: []credit.Metric{{Name: "units", Quantity: 4}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Balance.Available != 6 || result.Balance.Held != 0 || result.Balance.Consumed != 4 {
		t.Fatalf("unexpected settlement balance: %+v", result.Balance)
	}
	if err := store.WithinAccount(ctx, "acct-engine", func(tx credit.Tx) error {
		reservations, err := tx.Reservations()
		if err != nil {
			return err
		}
		if len(reservations) != 1 || reservations[0].State != "settled" || reservations[0].Consumed != 4 {
			t.Fatalf("unexpected reservation projection: %+v", reservations)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	entries, err := store.History(ctx, "acct-engine", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 4 {
		t.Fatalf("journal entries=%d, want 4", len(entries))
	}
}

func TestEnginePreservesAllocationOrderAndRepeatedLotAllocations(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	if err := store.CreateAccount(ctx, "acct-order", "subject-order"); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	unit := billing.Unit{Code: "credits", Scale: 100}
	engine := credit.New(store, func() time.Time { return now })
	if _, err := engine.Grant(ctx, credit.GrantInput{
		Account: "acct-order", Operation: "grant-z", LotID: "lot-z", Unit: unit,
		Amount: 5, Source: "purchase", SourceRef: "payment-z",
		ValidFrom: now, ExpiresAt: now.Add(2 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Grant(ctx, credit.GrantInput{
		Account: "acct-order", Operation: "grant-a", LotID: "lot-a", Unit: unit,
		Amount: 5, Source: "purchase", SourceRef: "payment-a",
		ValidFrom: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Reserve(ctx, credit.ReserveInput{
		Account: "acct-order", Operation: "reserve-order", ReservationID: "reservation-order",
		Actor: "actor-order", Unit: unit.Code, Amount: 7, Deadline: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Extend(ctx, credit.ExtendInput{
		Account: "acct-order", Operation: "extend-order", ReservationID: "reservation-order", Additional: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.WithinAccount(ctx, "acct-order", func(tx credit.Tx) error {
		reservations, err := tx.Reservations()
		if err != nil {
			return err
		}
		if len(reservations) != 1 {
			t.Fatalf("reservations=%d, want 1", len(reservations))
		}
		allocations := reservations[0].Allocations
		if len(allocations) != 3 {
			t.Fatalf("allocations=%+v, want three ordered entries", allocations)
		}
		want := []credit.Allocation{{LotID: "lot-a", Amount: 5}, {LotID: "lot-z", Amount: 2}, {LotID: "lot-z", Amount: 1}}
		for i := range want {
			if allocations[i] != want[i] {
				t.Fatalf("allocation %d=%+v, want %+v", i, allocations[i], want[i])
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// Raising or lowering an existing spend cap must work against the real store.
// SetLimit canonicalizes its period to UTC while the driver returns a non-nil
// location, so a time.Time struct comparison treated the identical stored
// period as a conflicting overlap and made every update impossible. The memory
// repository returns the value it was handed, so only Postgres shows it.
func TestSetLimitUpdatesAnExistingPeriodInPostgres(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("set-limit-update")
	if err := store.CreateAccount(ctx, account, "set-limit-update-subject"); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	engine := credit.New(store, func() time.Time { return now })
	period := billing.Period{Start: now.Add(-time.Hour), End: now.Add(time.Hour)}
	if _, err := engine.SetLimit(ctx, credit.LimitInput{
		Account: account, Operation: "limit-initial",
		Limit: credit.Limit{Actor: "worker", Unit: "credits", Period: period, Amount: 10},
	}); err != nil {
		t.Fatalf("initial limit: %v", err)
	}
	for _, amount := range []int64{25, 5} {
		if _, err := engine.SetLimit(ctx, credit.LimitInput{
			Account: account, Operation: billing.OperationID("limit-" + strconv.FormatInt(amount, 10)),
			Limit: credit.Limit{Actor: "worker", Unit: "credits", Period: period, Amount: amount},
		}); err != nil {
			t.Fatalf("update to %d: %v", amount, err)
		}
	}
}
