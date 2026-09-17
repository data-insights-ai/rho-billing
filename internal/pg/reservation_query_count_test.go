package pg

import (
	"context"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/data-insights-ai/rho-billing/credit"
)

type allocationQueryCounter struct {
	count   atomic.Int64
	inserts atomic.Int64
}

func (c *allocationQueryCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "FROM billing_reservation_allocations") {
		c.count.Add(1)
	}
	if strings.Contains(data.SQL, "INSERT INTO billing_reservation_allocations") {
		c.inserts.Add(1)
	}
	return ctx
}
func (*allocationQueryCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// Trace the actual driver requests so correct-looking results cannot hide a
// regression to one allocation query per reservation.
func TestReservationAllocationQueriesGrowByBatch(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	if err := store.CreateAccount(ctx, "query-account", "query-subject"); err != nil {
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
	counter := new(allocationQueryCounter)
	config.Tracer = counter
	traced := stdlib.OpenDB(*config)
	t.Cleanup(func() { _ = traced.Close() })
	tracedStore := New(traced)
	// Empty released authorizations are valid storage records with no allocation
	// rows. They isolate query amplification from data transfer costs.
	if _, err := db.ExecContext(ctx, `
 INSERT INTO billing_reservations(account_id,reservation_id,actor,unit,scope,
 created_at,deadline,state,authorized,consumed,evidence)
 SELECT 'query-account','r-'||n,'actor','credits','',now(),now()+interval '1 hour',
 'released',0,0,'{}'::jsonb FROM generate_series(1,1200) n`); err != nil {
		t.Fatal(err)
	}
	if err := tracedStore.WithinAccount(ctx, "query-account", func(tx credit.Tx) error {
		rows, err := tx.Reservations()
		if err == nil && len(rows) != 1200 {
			t.Fatalf("read %d reservations", len(rows))
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	t.Logf("allocation reads for 1200 reservations: %d", counter.count.Load())
	if queries := counter.count.Load(); queries < 1 || queries > 4 {
		t.Fatalf("allocation queries=%d for 1200 records; expected batched, not per-record reads", queries)
	}
}

func TestAllocationInsertPreservesRepeatedPositionsInOneStatement(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	if err := store.CreateAccount(ctx, "write-account", "write-subject"); err != nil {
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
	counter := new(allocationQueryCounter)
	config.Tracer = counter
	traced := stdlib.OpenDB(*config)
	t.Cleanup(func() { _ = traced.Close() })
	const count = 1200
	if err := New(traced).WithinAccount(ctx, "write-account", func(tx credit.Tx) error {
		lot := testLot("shared-lot", 1)
		lot.Initial, lot.Available, lot.Held = count, 0, count
		if err := tx.PutLot(lot); err != nil {
			return err
		}
		allocations := make([]credit.Allocation, count)
		for i := range allocations {
			allocations[i] = credit.Allocation{LotID: lot.ID, Amount: 1}
		}
		reservation := credit.Reservation{ID: "large-hold", Actor: "actor", Unit: "credits", CreatedAt: lot.GrantedAt, Deadline: lot.GrantedAt.Add(time.Hour), State: "held", Authorized: count, Allocations: allocations}
		if err := tx.PutReservation(reservation); err != nil {
			return err
		}
		read, found, err := tx.Reservation(reservation.ID)
		if err != nil {
			return err
		}
		if !found || len(read.Allocations) != count {
			t.Fatalf("found=%v allocations=%d", found, len(read.Allocations))
		}
		for _, allocation := range read.Allocations {
			if allocation.LotID != lot.ID || allocation.Amount != 1 {
				t.Fatalf("altered allocation: %+v", allocation)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if writes := counter.inserts.Load(); writes != 1 {
		t.Fatalf("allocation insert statements=%d, want one set insertion", writes)
	}
}
