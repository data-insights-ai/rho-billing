package credit

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
)

var errActiveReservationsRead = errors.New("active reservation read is forbidden")

// guardedReservationRepository catches accidental regressions to the old
// full held-reservation scan. The engine must use the bounded due query and
// point-load reservations needed by a command.
type guardedReservationRepository struct {
	Repository
	dueCalls atomic.Int64
	maxLimit atomic.Int64
}

func (r *guardedReservationRepository) WithinAccount(ctx context.Context, account billing.AccountID, fn func(Tx) error) error {
	return r.Repository.WithinAccount(ctx, account, func(tx Tx) error {
		return fn(guardedReservationTx{Tx: tx, repo: r})
	})
}

type guardedReservationTx struct {
	Tx
	repo *guardedReservationRepository
}

func (guardedReservationTx) ActiveReservations() ([]Reservation, error) {
	return nil, errActiveReservationsRead
}

func (t guardedReservationTx) DueReservations(at time.Time, limit int) ([]Reservation, error) {
	t.repo.dueCalls.Add(1)
	for {
		old := t.repo.maxLimit.Load()
		if int64(limit) <= old || t.repo.maxLimit.CompareAndSwap(old, int64(limit)) {
			break
		}
	}
	return t.Tx.DueReservations(at, limit)
}

func newGuardedReservationEngine() (*Engine, Repository, *testClock, *guardedReservationRepository) {
	clock := newTestClock()
	base := NewMemoryRepository(testAccount)
	repo := &guardedReservationRepository{Repository: base}
	return New(repo, clock.Now), repo, clock, repo
}

func TestReservationCommandsUseBoundedDueReadsAndPointLoads(t *testing.T) {
	engine, repo, clock, guarded := newGuardedReservationEngine()
	grant(t, engine, clock, "live-lot", 30, 0, "")

	if _, err := engine.Reserve(t.Context(), ReserveInput{
		Account: testAccount, Operation: "live-reserve", ReservationID: "live-hold",
		Actor: "actor", Unit: testUnit, Amount: 10, Deadline: clock.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Extend(t.Context(), ExtendInput{Account: testAccount, Operation: "live-extend", ReservationID: "live-hold", Additional: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Release(t.Context(), ReleaseInput{Account: testAccount, Operation: "live-release", ReservationID: "live-hold", Reason: "cancelled"}); err != nil {
		t.Fatal(err)
	}

	if _, err := engine.Reserve(t.Context(), ReserveInput{
		Account: testAccount, Operation: "live-reserve-settle", ReservationID: "live-settle-hold",
		Actor: "actor", Unit: testUnit, Amount: 4, Deadline: clock.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Settle(t.Context(), SettleInput{
		Account: testAccount, Operation: "live-settle", ReservationID: "live-settle-hold", Actual: 3,
		Evidence: Evidence{UsageID: "live-usage", RatingVersion: "live-rating", Metrics: []Metric{{Name: "units", Quantity: 3}}},
	}); err != nil {
		t.Fatal(err)
	}

	if guarded.dueCalls.Load() == 0 || guarded.maxLimit.Load() > 1001 {
		t.Fatalf("due query calls=%d max limit=%d", guarded.dueCalls.Load(), guarded.maxLimit.Load())
	}
	if err := repo.WithinAccount(t.Context(), testAccount, func(tx Tx) error {
		got, found, err := tx.Reservation("live-settle-hold")
		if err != nil {
			return err
		}
		if !found || got.State != "settled" || got.Consumed != 3 {
			t.Fatalf("settled reservation=%+v found=%v", got, found)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestTimedOutHeldReservationDoesNotConsumeLimit(t *testing.T) {
	engine, _, clock, guarded := newGuardedReservationEngine()
	grant(t, engine, clock, "timeout-lot", 20, 0, "")
	period := billing.Period{Start: clock.Now().Add(-time.Hour), End: clock.Now().Add(2 * time.Hour)}
	if _, err := engine.SetLimit(t.Context(), LimitInput{Account: testAccount, Operation: "timeout-limit", Limit: Limit{Actor: "actor", Unit: testUnit, Period: period, Amount: 10}}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Reserve(t.Context(), ReserveInput{
		Account: testAccount, Operation: "timeout-first", ReservationID: "timeout-hold", Actor: "actor", Unit: testUnit,
		Amount: 10, Deadline: clock.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	clock.Advance(time.Hour)
	if _, err := engine.Reserve(t.Context(), ReserveInput{
		Account: testAccount, Operation: "timeout-second", ReservationID: "timeout-second-hold", Actor: "actor", Unit: testUnit,
		Amount: 10, Deadline: clock.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("timed-out hold counted toward cap: %v", err)
	}
	if guarded.maxLimit.Load() > 1001 {
		t.Fatalf("unbounded due query limit=%d", guarded.maxLimit.Load())
	}
}

func TestExistingHeldReservationCountsTowardLimit(t *testing.T) {
	engine, _, clock, _ := newGuardedReservationEngine()
	grant(t, engine, clock, "cap-lot", 20, 0, "")
	period := billing.Period{Start: clock.Now().Add(-time.Hour), End: clock.Now().Add(time.Hour)}
	if _, err := engine.SetLimit(t.Context(), LimitInput{Account: testAccount, Operation: "cap-limit", Limit: Limit{Actor: "actor", Unit: testUnit, Period: period, Amount: 10}}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Reserve(t.Context(), ReserveInput{Account: testAccount, Operation: "cap-first", ReservationID: "cap-first-hold", Actor: "actor", Unit: testUnit, Amount: 7, Deadline: clock.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Reserve(t.Context(), ReserveInput{Account: testAccount, Operation: "cap-second", ReservationID: "cap-second-hold", Actor: "actor", Unit: testUnit, Amount: 4, Deadline: clock.Now().Add(time.Hour)}); !errors.Is(err, billing.ErrLimit) {
		t.Fatalf("second hold error=%v, want ErrLimit", err)
	}
}

func TestMaintenanceRequiredForMoreThanOneDuePage(t *testing.T) {
	engine, repo, clock, guarded := newGuardedReservationEngine()
	const dueCount = 1001
	if err := repo.WithinAccount(t.Context(), testAccount, func(tx Tx) error {
		for i := range dueCount {
			if err := tx.PutReservation(Reservation{
				ID: fmt.Sprintf("due-%04d", i), Actor: "maintenance", Unit: testUnit,
				CreatedAt: clock.Now().Add(-time.Hour), Deadline: clock.Now().Add(-time.Minute),
				State: "held", Authorized: 0,
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	_, err := engine.Grant(t.Context(), GrantInput{
		Account: testAccount, Operation: "maintenance-grant", LotID: "maintenance-lot",
		Unit: billing.Unit{Code: testUnit, Scale: 1000}, Amount: 1, Source: "maintenance", SourceRef: "maintenance-grant", ValidFrom: clock.Now(),
	})
	if !errors.Is(err, ErrMaintenanceRequired) || IsRejection(err) {
		t.Fatalf("grant error=%v, rejection=%v; want non-rejection maintenance error", err, IsRejection(err))
	}
	if err := repo.WithinAccount(t.Context(), testAccount, func(tx Tx) error {
		_, found, err := tx.Outcome("maintenance-grant")
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

	first, err := engine.Sweep(t.Context(), testAccount)
	if err != nil {
		t.Fatal(err)
	}
	if first.Reservations != 1000 || !first.HasMore {
		t.Fatalf("first sweep=%+v, want 1000 and more", first)
	}
	second, err := engine.Sweep(t.Context(), testAccount)
	if err != nil {
		t.Fatal(err)
	}
	if second.Reservations != 1 || second.HasMore {
		t.Fatalf("second sweep=%+v, want 1 and complete", second)
	}
	if _, err := engine.Grant(t.Context(), GrantInput{
		Account: testAccount, Operation: "maintenance-grant", LotID: "maintenance-lot",
		Unit: billing.Unit{Code: testUnit, Scale: 1000}, Amount: 1, Source: "maintenance", SourceRef: "maintenance-grant", ValidFrom: clock.Now(),
	}); err != nil {
		t.Fatalf("grant after maintenance: %v", err)
	}
	if guarded.maxLimit.Load() > 1001 {
		t.Fatalf("due query limit=%d", guarded.maxLimit.Load())
	}
}
