package pg

import (
	"fmt"
	"sync"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
)

func TestPostgresConcurrentSweepTimesOutEachDueReservationOnce(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	const account = billing.AccountID("sweep-concurrency")
	if err := store.CreateAccount(ctx, account, "sweep-concurrency-subject"); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	engine := credit.New(store, func() time.Time { return now.Add(-2 * time.Hour) })
	if _, err := engine.Grant(ctx, credit.GrantInput{
		Account: account, Operation: "sweep-grant", LotID: "sweep-lot",
		Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 1001,
		Source: "fixture", SourceRef: "sweep-grant", ValidFrom: now.Add(-2 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	const reservationCount = 1001
	if err := store.WithinAccount(ctx, account, func(tx credit.Tx) error {
		lot, found, err := tx.Lot("sweep-lot")
		if err != nil {
			return err
		}
		if !found {
			t.Fatal("seed grant lot was not found")
		}
		lot.Available = 0
		lot.Held = reservationCount
		if err := tx.PutLot(lot); err != nil {
			return err
		}

		reservations := make([]credit.Reservation, 0, reservationCount)
		entries := make([]credit.Entry, 0, reservationCount)
		for i := range reservationCount {
			id := billing.OperationID("sweep-reserve-" + formatIndex(i))
			reservationID := "sweep-reservation-" + formatIndex(i)
			reservations = append(reservations, credit.Reservation{
				ID: reservationID, Actor: "worker", Unit: "credits", CreatedAt: now.Add(-time.Hour),
				Deadline: now.Add(-time.Minute), State: "held", Authorized: 1,
				Allocations: []credit.Allocation{{LotID: "sweep-lot", Amount: 1}},
			})
			entries = append(entries, credit.Entry{
				OperationID: id, LotID: "sweep-lot", ReservationID: reservationID,
				Kind: "reserve", Reason: "fixture", RecordedAt: now.Add(-time.Hour), EffectiveAt: now.Add(-time.Hour),
				Available: -1, Held: 1,
			})
		}
		for _, reservation := range reservations {
			if err := tx.PutReservation(reservation); err != nil {
				return err
			}
		}
		return tx.AppendEntries(entries)
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := engine.VerifyLedger(ctx, account); err != nil {
		t.Fatalf("seed ledger: %v", err)
	}
	second := New(secondQueueDB(t, db))
	firstEngine := credit.New(store, func() time.Time { return now })
	secondEngine := credit.New(second, func() time.Time { return now })
	start := make(chan struct{})
	type sweepResult struct {
		result credit.SweepResult
		err    error
	}
	results := make(chan sweepResult, 2)
	var wg sync.WaitGroup
	wg.Go(func() {
		<-start
		result, err := firstEngine.Sweep(ctx, account)
		results <- sweepResult{result: result, err: err}
	})
	wg.Go(func() {
		<-start
		result, err := secondEngine.Sweep(ctx, account)
		results <- sweepResult{result: result, err: err}
	})
	close(start)
	wg.Wait()
	close(results)

	totalReported := 0
	moreCount := 0
	completeCount := 0
	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent sweep: %v", result.err)
		}
		totalReported += result.result.Reservations
		if result.result.HasMore {
			if result.result.Reservations != 1000 {
				t.Fatalf("first page=%+v", result.result)
			}
			moreCount++
		} else {
			if result.result.Reservations != 1 {
				t.Fatalf("last page=%+v", result.result)
			}
			completeCount++
		}
	}
	if totalReported != reservationCount || moreCount != 1 || completeCount != 1 {
		t.Fatalf("sweep reports total=%d more=%d complete=%d, want total=%d one page each", totalReported, moreCount, completeCount, reservationCount)
	}

	if err := store.WithinAccount(ctx, account, func(tx credit.Tx) error {
		rows, err := tx.Reservations()
		if err != nil {
			return err
		}
		if len(rows) != reservationCount {
			t.Fatalf("reservation rows=%d, want %d", len(rows), reservationCount)
		}
		for _, row := range rows {
			if row.State != "timed_out" || row.Consumed != 0 || row.Authorized != 1 {
				t.Fatalf("reservation was not timed out exactly once: %+v", row)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	balance, err := firstEngine.Balance(ctx, account, "credits", "")
	if err != nil {
		t.Fatal(err)
	}
	if balance.Available != reservationCount || balance.Held != 0 || balance.Consumed != 0 {
		t.Fatalf("final balance=%+v, want available=%d and no held/consumed", balance, reservationCount)
	}
	if _, err := firstEngine.VerifyLedger(ctx, account); err != nil {
		t.Fatalf("ledger verification after concurrent sweep: %v", err)
	}
}

func formatIndex(index int) string {
	return fmt.Sprintf("%04d", index)
}
