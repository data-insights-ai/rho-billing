package credit

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
)

var (
	errFullHistoryRead = errors.New("full credit history read is not allowed")
	errLiveLotRead     = errors.New("unbounded live lot materialization is not allowed")
)

type liveStateRepository struct{ Repository }

func (r liveStateRepository) WithinAccount(ctx context.Context, account billing.AccountID, fn func(Tx) error) error {
	return r.Repository.WithinAccount(ctx, account, func(tx Tx) error {
		return fn(liveStateTx{Tx: tx})
	})
}

type liveStateTx struct{ Tx }

func (liveStateTx) Lots() ([]Lot, error)     { return nil, errFullHistoryRead }
func (liveStateTx) LiveLots() ([]Lot, error) { return nil, errLiveLotRead }

func newLiveStateEngine() (*Engine, Repository, *testClock) {
	_, repo, clock := newTestEngine()
	return New(liveStateRepository{Repository: repo}, clock.Now), repo, clock
}

func TestEngineBalanceNeverMaterializesLiveLots(t *testing.T) {
	engine, _, clock := newLiveStateEngine()
	grant(t, engine, clock, "current", 4, 0, "")
	grant(t, engine, clock, "expiring", 3, time.Hour, "")
	clock.Advance(time.Hour)
	if got := balance(t, engine, testUnit, ""); got != (Balance{Available: 4, Expired: 3}) {
		t.Fatalf("balance after expiry = %+v", got)
	}
}

func TestLiveStateEngineNeverReadsFullHistory(t *testing.T) {
	engine, repo, clock := newLiveStateEngine()
	grant(t, engine, clock, "retired", 5, 0, "")
	reserve(t, engine, clock, "reserve-retired", "res-retired", 5, time.Hour, "actor", "")
	settle(t, engine, "res-retired", "settle-retired", "usage-retired", 5)

	if got := balance(t, engine, testUnit, ""); got != (Balance{Consumed: 5}) {
		t.Fatalf("retired lifetime balance = %+v", got)
	}
	// The wrapper would fail every operation if the normal path called Lots.
	if err := repo.WithinAccount(t.Context(), testAccount, func(tx Tx) error {
		lots, err := tx.LiveLots()
		if err != nil {
			return err
		}
		if len(lots) != 0 {
			t.Fatalf("retired lot remained in live state: %+v", lots)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRetiredLotPointLookupSupportsRevoke(t *testing.T) {
	engine, _, clock := newLiveStateEngine()
	grant(t, engine, clock, "retired-revoke", 5, 0, "")
	reserve(t, engine, clock, "reserve-revoke", "res-revoke", 5, time.Hour, "actor", "")
	settle(t, engine, "res-revoke", "settle-revoke", "usage-revoke", 5)

	result, err := engine.Revoke(t.Context(), RevokeInput{
		Account:   testAccount,
		Operation: "revoke-retired",
		LotID:     "retired-revoke",
		Reason:    "customer-refund",
	})
	if err != nil {
		t.Fatalf("revoke retired lot: %v", err)
	}
	if result.LotID != "retired-revoke" || result.Exposure != 5 {
		t.Fatalf("retired revoke result = %+v", result)
	}
	if got := balance(t, engine, testUnit, ""); got != (Balance{Consumed: 5}) {
		t.Fatalf("post-revoke lifetime balance = %+v", got)
	}
}

func TestDuplicateSourceOfRetiredLotIsRejected(t *testing.T) {
	engine, _, clock := newLiveStateEngine()
	grant(t, engine, clock, "source-retired", 2, 0, "")
	reserve(t, engine, clock, "reserve-source", "res-source", 2, time.Hour, "actor", "")
	settle(t, engine, "res-source", "settle-source", "usage-source", 2)

	_, err := engine.Grant(t.Context(), GrantInput{
		Account:   testAccount,
		Operation: "grant-duplicate-retired-source",
		LotID:     "source-new-lot",
		Unit:      billing.Unit{Code: testUnit, Scale: 1000},
		Amount:    3,
		Source:    "source-source-retired",
		SourceRef: "ref-source-retired",
		ValidFrom: clock.Now().Add(-time.Hour),
	})
	if !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("duplicate source error = %v", err)
	}
	if !IsRejection(err) {
		t.Fatalf("duplicate source was not durable rejection: %v", err)
	}
}

func TestFutureAvailableOverflowDoesNotCorruptCurrentBalance(t *testing.T) {
	engine, _, clock := newLiveStateEngine()
	for _, lotID := range []string{"future-a", "future-b"} {
		_, err := engine.Grant(t.Context(), GrantInput{
			Account:   testAccount,
			Operation: billing.OperationID("grant-" + lotID),
			LotID:     lotID,
			Unit:      billing.Unit{Code: testUnit, Scale: 1000},
			Amount:    math.MaxInt64,
			Source:    "source-" + lotID,
			SourceRef: "ref-" + lotID,
			ValidFrom: clock.Now().Add(time.Hour),
		})
		if err != nil {
			t.Fatalf("future grant %s: %v", lotID, err)
		}
	}
	grant(t, engine, clock, "current", 1, 0, "")

	if got := balance(t, engine, testUnit, ""); got != (Balance{Available: 1}) {
		t.Fatalf("balance with future raw availability overflow = %+v", got)
	}
}

func TestScopedGlobalTotalsAndHeldExpiredSemantics(t *testing.T) {
	engine, _, clock := newLiveStateEngine()
	grant(t, engine, clock, "global", 4, 0, "")
	grant(t, engine, clock, "project-expiring", 6, time.Hour, "project")
	grant(t, engine, clock, "other", 7, 0, "other")

	if got := balance(t, engine, testUnit, "project"); got.Available != 10 {
		t.Fatalf("project global-plus-scoped balance = %+v", got)
	}
	if got := balance(t, engine, testUnit, "other"); got.Available != 11 {
		t.Fatalf("other global-plus-scoped balance = %+v", got)
	}
	if got := balance(t, engine, testUnit, ""); got.Available != 17 {
		t.Fatalf("global balance = %+v", got)
	}

	reserve(t, engine, clock, "reserve-expiring", "res-expiring", 6, 2*time.Hour, "actor", "project")
	clock.Advance(time.Hour)
	if got := balance(t, engine, testUnit, "project"); got.Available != 4 || got.Held != 6 || got.Expired != 0 {
		t.Fatalf("expired held balance = %+v", got)
	}
	if got := balance(t, engine, testUnit, "other"); got.Available != 11 || got.Held != 0 || got.Expired != 0 {
		t.Fatalf("other scope after project expiry = %+v", got)
	}

	if _, err := engine.Release(t.Context(), ReleaseInput{
		Account:       testAccount,
		Operation:     "release-expiring",
		ReservationID: "res-expiring",
		Reason:        "cancelled",
	}); err != nil {
		t.Fatalf("release expired hold: %v", err)
	}
	if got := balance(t, engine, testUnit, "project"); got.Available != 4 || got.Held != 0 || got.Expired != 6 {
		t.Fatalf("released expired hold balance = %+v", got)
	}
}
