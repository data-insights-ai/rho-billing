package credit

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
)

const testUnit = "credits"

var testAccount = billing.AccountID("acct")

type testClock struct {
	mu  sync.RWMutex
	now time.Time
}

func newTestClock() *testClock {
	return &testClock{now: time.Date(2026, time.January, 15, 12, 0, 0, 0, time.UTC)}
}

func (c *testClock) Now() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.now
}

func (c *testClock) Set(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = at
}

func (c *testClock) Advance(delta time.Duration) { c.Set(c.Now().Add(delta)) }

func newTestEngine() (*Engine, Repository, *testClock) {
	clock := newTestClock()
	repo := NewMemoryRepository(testAccount)
	return New(repo, clock.Now), repo, clock
}

func grant(t *testing.T, engine *Engine, clock *testClock, lotID string, amount int64, expiresAfter time.Duration, scope string) Result {
	t.Helper()
	validFrom := clock.Now().Add(-time.Hour)
	var expiresAt time.Time
	if expiresAfter != 0 {
		expiresAt = clock.Now().Add(expiresAfter)
	}
	result, err := engine.Grant(context.Background(), GrantInput{
		Account:   testAccount,
		Operation: billing.OperationID("grant-" + lotID),
		LotID:     lotID,
		Unit:      billing.Unit{Code: testUnit, Scale: 1000},
		Amount:    amount,
		Scope:     scope,
		Source:    "source-" + lotID,
		SourceRef: "ref-" + lotID,
		ValidFrom: validFrom,
		ExpiresAt: expiresAt,
	})
	if err != nil {
		t.Fatalf("grant %s: %v", lotID, err)
	}
	return result
}

func reserve(t *testing.T, engine *Engine, clock *testClock, op, reservationID string, amount int64, deadlineAfter time.Duration, actor, scope string) Result {
	t.Helper()
	result, err := engine.Reserve(context.Background(), ReserveInput{
		Account:       testAccount,
		Operation:     billing.OperationID(op),
		ReservationID: reservationID,
		Actor:         actor,
		Unit:          testUnit,
		Scope:         scope,
		Amount:        amount,
		Deadline:      clock.Now().Add(deadlineAfter),
	})
	if err != nil {
		t.Fatalf("reserve %s: %v", reservationID, err)
	}
	return result
}

func settle(t *testing.T, engine *Engine, reservationID, op, usageID string, actual int64) Result {
	t.Helper()
	result, err := engine.Settle(context.Background(), SettleInput{
		Account:       testAccount,
		Operation:     billing.OperationID(op),
		ReservationID: reservationID,
		Actual:        actual,
		Evidence: Evidence{
			UsageID:       usageID,
			RatingVersion: "rating-v1",
			Metrics:       []Metric{{Name: "units", Quantity: actual}},
		},
	})
	if err != nil {
		t.Fatalf("settle %s: %v", reservationID, err)
	}
	return result
}

func balance(t *testing.T, engine *Engine, unit, scope string) Balance {
	t.Helper()
	result, err := engine.Balance(context.Background(), testAccount, unit, scope)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func reservation(t *testing.T, engine *Engine, id string) Reservation {
	t.Helper()
	result, err := engine.Reservation(context.Background(), testAccount, id)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestFEFOAllocationAndScopeFiltering(t *testing.T) {
	engine, _, clock := newTestEngine()
	grant(t, engine, clock, "late", 3, 3*time.Hour, "")
	grant(t, engine, clock, "early", 4, time.Hour, "")
	grant(t, engine, clock, "never", 3, 0, "")

	reserve(t, engine, clock, "reserve-fefo", "res-fefo", 6, time.Hour, "actor", "")
	got := reservation(t, engine, "res-fefo")
	if len(got.Allocations) != 2 || got.Allocations[0] != (Allocation{LotID: "early", Amount: 4}) || got.Allocations[1] != (Allocation{LotID: "late", Amount: 2}) {
		t.Fatalf("FEFO allocations = %#v", got.Allocations)
	}

	engine, _, clock = newTestEngine()
	grant(t, engine, clock, "global", 2, 0, "")
	grant(t, engine, clock, "project-a", 2, 0, "project-a")
	grant(t, engine, clock, "project-b", 4, 0, "project-b")
	reserve(t, engine, clock, "reserve-a", "res-a", 4, time.Hour, "actor", "project-a")
	if got := balance(t, engine, testUnit, "project-a"); got.Available != 0 || got.Held != 4 {
		t.Fatalf("scope-a balance = %+v", got)
	}
	if got := balance(t, engine, testUnit, "project-b"); got.Available != 4 || got.Held != 2 {
		t.Fatalf("scope-b balance = %+v", got)
	}
	if _, err := engine.Reserve(context.Background(), ReserveInput{
		Account:       testAccount,
		Operation:     "reserve-bad-scope",
		ReservationID: "res-bad-scope",
		Actor:         "actor",
		Unit:          testUnit,
		Scope:         "project-a",
		Amount:        1,
		Deadline:      clock.Now().Add(time.Hour),
	}); err != nil {
		// The global lot is exhausted and project-a is held. project-b must
		// not be eligible for a project-a reservation.
		if !errors.Is(err, billing.ErrInsufficient) {
			t.Fatalf("scope mismatch error = %v", err)
		}
	} else {
		t.Fatal("scope mismatch unexpectedly reserved credits")
	}

	engine, _, clock = newTestEngine()
	grant(t, engine, clock, "lot-b", 2, time.Hour, "")
	grant(t, engine, clock, "lot-a", 2, time.Hour, "")
	reserve(t, engine, clock, "reserve-tie", "res-tie", 3, time.Hour, "actor", "")
	got = reservation(t, engine, "res-tie")
	if len(got.Allocations) != 2 || got.Allocations[0] != (Allocation{LotID: "lot-a", Amount: 2}) || got.Allocations[1] != (Allocation{LotID: "lot-b", Amount: 1}) {
		t.Fatalf("FEFO stable ID tie break allocations = %#v", got.Allocations)
	}
}

func TestExpiryAtExactBoundaryMaterializesWithoutWorker(t *testing.T) {
	engine, repo, clock := newTestEngine()
	grant(t, engine, clock, "expiring", 5, time.Hour, "")
	clock.Advance(time.Hour - time.Nanosecond)
	if got := balance(t, engine, testUnit, ""); got.Available != 5 || got.Expired != 0 {
		t.Fatalf("before expiry balance = %+v", got)
	}
	clock.Advance(time.Nanosecond)
	if got := balance(t, engine, testUnit, ""); got.Available != 0 || got.Expired != 5 {
		t.Fatalf("at expiry balance = %+v", got)
	}
	entries, err := repo.History(context.Background(), testAccount, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[1].Kind != "expiry" || !entries[1].EffectiveAt.Equal(clock.Now().UTC()) {
		t.Fatalf("expiry journal = %#v", entries)
	}
}

func TestDurableIdempotencyAndRejectedRetry(t *testing.T) {
	engine, repo, clock := newTestEngine()
	first := grant(t, engine, clock, "lot", 5, 0, "")
	entriesBefore, err := repo.History(context.Background(), testAccount, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	second, err := engine.Grant(context.Background(), GrantInput{
		Account:   testAccount,
		Operation: "grant-lot",
		LotID:     "lot",
		Unit:      billing.Unit{Code: testUnit, Scale: 1000},
		Amount:    5,
		Source:    "source-lot",
		SourceRef: "ref-lot",
		ValidFrom: clock.Now().Add(-time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.LotID != second.LotID || first.Balance != second.Balance {
		t.Fatalf("retry result changed: first=%+v second=%+v", first, second)
	}
	entriesAfter, err := repo.History(context.Background(), testAccount, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(entriesAfter) != len(entriesBefore) {
		t.Fatalf("idempotent retry appended journal: before=%d after=%d", len(entriesBefore), len(entriesAfter))
	}
	_, err = engine.Grant(context.Background(), GrantInput{
		Account:   testAccount,
		Operation: "grant-lot",
		LotID:     "lot",
		Unit:      billing.Unit{Code: testUnit, Scale: 1000},
		Amount:    6,
		Source:    "source-lot",
		SourceRef: "ref-lot",
		ValidFrom: clock.Now().Add(-time.Hour),
	})
	if !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("changed idempotency payload error = %v", err)
	}
	if got := balance(t, engine, testUnit, ""); got.Available != 5 {
		t.Fatalf("changed payload mutated balance: %+v", got)
	}

	_, err = engine.Reserve(context.Background(), ReserveInput{Account: testAccount, Operation: "reserve-rejected", ReservationID: "res-rejected", Unit: testUnit, Amount: 8, Deadline: clock.Now().Add(time.Hour)})
	if !errors.Is(err, billing.ErrInsufficient) {
		t.Fatalf("initial rejected reserve error = %v", err)
	}
	_, err = engine.Reserve(context.Background(), ReserveInput{Account: testAccount, Operation: "reserve-rejected", ReservationID: "res-rejected", Unit: testUnit, Amount: 7, Deadline: clock.Now().Add(time.Hour)})
	if !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("changed rejected payload error = %v", err)
	}
	grant(t, engine, clock, "later", 8, 0, "")
	_, err = engine.Reserve(context.Background(), ReserveInput{Account: testAccount, Operation: "reserve-rejected", ReservationID: "res-rejected", Unit: testUnit, Amount: 8, Deadline: clock.Now().Add(time.Hour)})
	if !errors.Is(err, billing.ErrInsufficient) {
		t.Fatalf("rejected retry after funding error = %v", err)
	}
	reserve(t, engine, clock, "reserve-new", "res-new", 8, time.Hour, "", "")
}

func TestReservationSettlesAfterLotExpiryAndReleasesExpiredRemainder(t *testing.T) {
	engine, _, clock := newTestEngine()
	grant(t, engine, clock, "held-expiry", 10, time.Hour, "")
	reserve(t, engine, clock, "reserve-held", "res-held", 10, 2*time.Hour, "actor", "")
	clock.Advance(time.Hour + time.Nanosecond)
	settle(t, engine, "res-held", "settle-held", "usage-held", 4)
	got := reservation(t, engine, "res-held")
	if got.State != "settled" || got.Consumed != 4 || got.Evidence.UsageID != "usage-held" {
		t.Fatalf("settled reservation = %+v", got)
	}
	if got := balance(t, engine, testUnit, ""); got.Available != 0 || got.Held != 0 || got.Consumed != 4 || got.Expired != 6 {
		t.Fatalf("post-expiry settlement balance = %+v", got)
	}
}

func TestReservationReleaseAfterLotExpiryRetiresRemainder(t *testing.T) {
	engine, _, clock := newTestEngine()
	grant(t, engine, clock, "release-expiry", 5, time.Hour, "")
	reserve(t, engine, clock, "reserve-release", "res-release", 5, 2*time.Hour, "actor", "")
	clock.Advance(time.Hour + time.Nanosecond)
	result, err := engine.Release(context.Background(), ReleaseInput{Account: testAccount, Operation: "release-expiry", ReservationID: "res-release", Reason: "cancelled"})
	if err != nil {
		t.Fatal(err)
	}
	if result.ReservationID != "res-release" {
		t.Fatalf("release result = %+v", result)
	}
	if got := reservation(t, engine, "res-release"); got.State != "released" || got.Consumed != 0 {
		t.Fatalf("released reservation = %+v", got)
	}
	if got := balance(t, engine, testUnit, ""); got.Available != 0 || got.Held != 0 || got.Expired != 5 {
		t.Fatalf("post-expiry release balance = %+v", got)
	}
}

func TestReservationDeadlineAndOverrun(t *testing.T) {
	engine, _, clock := newTestEngine()
	grant(t, engine, clock, "deadline", 5, 0, "")
	reserve(t, engine, clock, "reserve-deadline", "res-deadline", 5, time.Hour, "actor", "")
	clock.Advance(time.Hour)
	if _, err := engine.Settle(context.Background(), SettleInput{Account: testAccount, Operation: "settle-after-deadline", ReservationID: "res-deadline", Actual: 1, Evidence: Evidence{UsageID: "usage-late", RatingVersion: "rating-v1", Metrics: []Metric{{Name: "units", Quantity: 1}}}}); !errors.Is(err, billing.ErrExpired) {
		t.Fatalf("deadline settlement error = %v", err)
	}
	if got := reservation(t, engine, "res-deadline"); got.State != "timed_out" {
		t.Fatalf("deadline state = %q", got.State)
	}
	if got := balance(t, engine, testUnit, ""); got.Available != 5 {
		t.Fatalf("deadline timeout balance = %+v", got)
	}

	engine, _, clock = newTestEngine()
	grant(t, engine, clock, "overrun", 5, 0, "")
	reserve(t, engine, clock, "reserve-overrun", "res-overrun", 3, time.Hour, "actor", "")
	bad, err := engine.Settle(context.Background(), SettleInput{Account: testAccount, Operation: "settle-overrun", ReservationID: "res-overrun", Actual: 4, Evidence: Evidence{UsageID: "usage-overrun", RatingVersion: "rating-v1", Metrics: []Metric{{Name: "units", Quantity: 4}}}})
	if !errors.Is(err, billing.ErrInvalid) || bad != (Result{}) {
		t.Fatalf("overrun result/error = %+v / %v", bad, err)
	}
	if got := reservation(t, engine, "res-overrun"); got.State != "held" || got.Consumed != 0 {
		t.Fatalf("overrun changed reservation = %+v", got)
	}
	settle(t, engine, "res-overrun", "settle-within-bound", "usage-within-bound", 3)
}

func TestDuplicateUsageIDAndRevocationOfHeldCredits(t *testing.T) {
	engine, _, clock := newTestEngine()
	grant(t, engine, clock, "usage-lot", 10, 0, "")
	reserve(t, engine, clock, "reserve-one", "res-one", 3, time.Hour, "actor", "")
	reserve(t, engine, clock, "reserve-two", "res-two", 3, time.Hour, "actor", "")
	settle(t, engine, "res-one", "settle-one", "same-usage", 2)
	if _, err := engine.Settle(context.Background(), SettleInput{Account: testAccount, Operation: "settle-two", ReservationID: "res-two", Actual: 2, Evidence: Evidence{UsageID: "same-usage", RatingVersion: "rating-v1", Metrics: []Metric{{Name: "units", Quantity: 2}}}}); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("duplicate usage ID error = %v", err)
	}
	if got := reservation(t, engine, "res-two"); got.State != "held" {
		t.Fatalf("duplicate usage changed reservation = %+v", got)
	}

	engine, _, clock = newTestEngine()
	grant(t, engine, clock, "revoked-lot", 10, 0, "")
	reserve(t, engine, clock, "reserve-revoked", "res-revoked", 6, time.Hour, "actor", "")
	result, err := engine.Revoke(context.Background(), RevokeInput{Account: testAccount, Operation: "revoke-lot", LotID: "revoked-lot", Reason: "refund"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Exposure != 0 {
		t.Fatalf("revoke exposure before settlement = %d", result.Exposure)
	}
	if got := balance(t, engine, testUnit, ""); got.Available != 0 || got.Held != 6 || got.Revoked != 4 {
		t.Fatalf("held revocation balance = %+v", got)
	}
	settle(t, engine, "res-revoked", "settle-revoked", "usage-revoked", 2)
	if got := balance(t, engine, testUnit, ""); got.Available != 0 || got.Held != 0 || got.Consumed != 2 || got.Revoked != 8 {
		t.Fatalf("post-revocation settlement balance = %+v", got)
	}
}

func TestActorLimitSerializesConcurrentReservations(t *testing.T) {
	engine, _, clock := newTestEngine()
	grant(t, engine, clock, "cap-lot", 10, 0, "")
	period := billing.Period{Start: clock.Now().Add(-time.Hour), End: clock.Now().Add(time.Hour)}
	if _, err := engine.SetLimit(context.Background(), LimitInput{Account: testAccount, Operation: "limit-actor", Limit: Limit{Actor: "actor", Unit: testUnit, Period: period, Amount: 3}}); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for i := range 2 {
		i := i
		wg.Go(func() {
			<-start
			_, err := engine.Reserve(context.Background(), ReserveInput{Account: testAccount, Operation: billing.OperationID("reserve-cap-" + string(rune('a'+i))), ReservationID: "res-cap-" + string(rune('a'+i)), Actor: "actor", Unit: testUnit, Amount: 2, Deadline: clock.Now().Add(time.Hour)})
			results <- err
		})
	}
	close(start)
	wg.Wait()
	close(results)
	var succeeded, limited int
	for err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, billing.ErrLimit):
			limited++
		default:
			t.Fatalf("concurrent reservation error = %v", err)
		}
	}
	if succeeded != 1 || limited != 1 {
		t.Fatalf("concurrent actor cap outcomes: succeeded=%d limited=%d", succeeded, limited)
	}
	if got := balance(t, engine, testUnit, ""); got.Available != 8 || got.Held != 2 {
		t.Fatalf("concurrent actor cap balance = %+v", got)
	}
}

func TestExtendRejectsNewAllocationFragmentAfterBudget(t *testing.T) {
	engine, repo, clock := newTestEngine()
	grant(t, engine, clock, "extend-budget", 1002, 0, "")
	reserve(t, engine, clock, "extend-budget-reserve", "extend-budget-reservation", 1, time.Hour, "actor", "")
	for i := range 999 {
		if _, err := engine.Extend(context.Background(), ExtendInput{Account: testAccount, Operation: billing.OperationID("extend-budget-" + strconv.Itoa(i)), ReservationID: "extend-budget-reservation", Additional: 1}); err != nil {
			t.Fatalf("extend %d: %v", i, err)
		}
	}
	if _, err := engine.Extend(context.Background(), ExtendInput{Account: testAccount, Operation: "extend-budget-overflow", ReservationID: "extend-budget-reservation", Additional: 1}); !errors.Is(err, ErrAllocationBudget) {
		t.Fatalf("overflow extension error=%v", err)
	}
	if err := repo.WithinAccount(context.Background(), testAccount, func(tx Tx) error {
		_, found, err := tx.Outcome("extend-budget-overflow")
		if err != nil {
			return err
		}
		if found {
			t.Fatal("allocation budget error persisted as outcome")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestJournalReconstructsConservation(t *testing.T) {
	engine, repo, clock := newTestEngine()
	grant(t, engine, clock, "journal-a", 10, 10*time.Hour, "")
	grant(t, engine, clock, "journal-b", 5, 0, "")
	reserve(t, engine, clock, "journal-reserve", "journal-res", 6, time.Hour, "actor", "")
	settle(t, engine, "journal-res", "journal-settle", "journal-usage", 2)
	if _, err := engine.Revoke(context.Background(), RevokeInput{Account: testAccount, Operation: "journal-revoke", LotID: "journal-b", Reason: "refund"}); err != nil {
		t.Fatal(err)
	}

	entries, err := repo.History(context.Background(), testAccount, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	type totals struct{ available, held, consumed, expired, revoked int64 }
	perLot := map[string]totals{}
	for _, entry := range entries {
		current := perLot[entry.LotID]
		current.available += entry.Available
		current.held += entry.Held
		current.consumed += entry.Consumed
		current.expired += entry.Expired
		current.revoked += entry.Revoked
		perLot[entry.LotID] = current
	}
	if perLot["journal-a"] != (totals{available: 8, consumed: 2}) {
		t.Fatalf("journal-a reconstruction = %+v", perLot["journal-a"])
	}
	if perLot["journal-b"] != (totals{revoked: 5}) {
		t.Fatalf("journal-b reconstruction = %+v", perLot["journal-b"])
	}
	got := balance(t, engine, testUnit, "")
	if got != (Balance{Available: 8, Consumed: 2, Revoked: 5}) {
		t.Fatalf("projected balance = %+v", got)
	}
	// The journal must reconstruct the lot's own recorded Initial. Deriving
	// `initial` from the journal's grant entries instead would compare the
	// journal against itself: those same entries are already summed into
	// perLot, so the identity would hold even if a grant journalled the wrong
	// amount.
	granted := map[string]int64{"journal-a": 10, "journal-b": 5}
	for lotID, total := range perLot {
		var stored Lot
		if err := repo.WithinAccount(context.Background(), testAccount, func(tx Tx) error {
			lot, found, err := tx.Lot(lotID)
			if err != nil {
				return err
			}
			if !found {
				t.Fatalf("lot %s missing from the store", lotID)
			}
			stored = lot
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if stored.Initial != granted[lotID] {
			t.Errorf("lot %s stored Initial=%d, want the granted %d", lotID, stored.Initial, granted[lotID])
		}
		if total.available+total.held+total.consumed+total.expired+total.revoked != stored.Initial {
			t.Errorf("lot %s violates conservation: journal total=%+v stored initial=%d", lotID, total, stored.Initial)
		}
	}
}

// A spend cap introduced after a reservation must still bound extensions of it.
// Evaluating the limit as of the reservation's CreatedAt skipped the check
// entirely, because a period starting later never matched that instant.
func TestExtendHonoursALimitCreatedAfterTheReservation(t *testing.T) {
	engine, _, clock := newTestEngine()
	grant(t, engine, clock, "extend-limit-lot", 1000, 0, "")
	reserve(t, engine, clock, "extend-limit-reserve", "extend-limit-res", 10, time.Hour, "actor", "")
	// The cap is introduced strictly after the reservation, so a check keyed on
	// the reservation's CreatedAt would not find it at all.
	clock.Advance(time.Minute)
	now := clock.Now()
	if _, err := engine.SetLimit(context.Background(), LimitInput{
		Account: testAccount, Operation: "extend-limit-set",
		Limit: Limit{Actor: "actor", Unit: testUnit, Period: billing.Period{Start: now, End: now.Add(30 * 24 * time.Hour)}, Amount: 20},
	}); err != nil {
		t.Fatalf("set limit: %v", err)
	}
	_, err := engine.Extend(context.Background(), ExtendInput{
		Account: testAccount, Operation: "extend-limit-over", ReservationID: "extend-limit-res", Additional: 500,
	})
	var rejection *Rejection
	if !errors.As(err, &rejection) || !errors.Is(err, billing.ErrLimit) {
		t.Fatalf("extension past the cap err=%v, want a limit rejection", err)
	}
	if _, err := engine.Extend(context.Background(), ExtendInput{
		Account: testAccount, Operation: "extend-limit-ok", ReservationID: "extend-limit-res", Additional: 5,
	}); err != nil {
		t.Fatalf("extension within the cap: %v", err)
	}
}
