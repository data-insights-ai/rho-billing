package pg

import (
	"errors"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
)

func TestPostgresFEFOProvenanceAndCarryover(t *testing.T) {
	for _, backend := range []string{"memory", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			var repo credit.Repository
			if backend == "memory" {
				repo = credit.NewMemoryRepository("acct-a")
			} else {
				store, _ := testStore(t)
				createTestAccounts(t, store)
				repo = store
			}
			now := testTime()
			engine := credit.New(repo, func() time.Time { return now })
			ctx := t.Context()
			unit := billing.Unit{Code: "credits", Scale: 1}
			if _, err := engine.Grant(ctx, credit.GrantInput{Account: "acct-a", Operation: "g-inc", LotID: "included", Unit: unit, Amount: 3, Source: "included", SourceRef: "inc-1", ValidFrom: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
				t.Fatal(err)
			}
			if _, err := engine.Grant(ctx, credit.GrantInput{Account: "acct-a", Operation: "g-pro", LotID: "promo", Unit: unit, Amount: 4, Source: "promotion", SourceRef: "pro-1", ValidFrom: now, ExpiresAt: now.Add(2 * time.Hour)}); err != nil {
				t.Fatal(err)
			}
			if _, err := engine.Grant(ctx, credit.GrantInput{Account: "acct-a", Operation: "g-pur", LotID: "purchased", Unit: unit, Amount: 5, Source: "purchase", SourceRef: "pay-1", ValidFrom: now}); err != nil {
				t.Fatal(err)
			}
			if _, err := engine.Reserve(ctx, credit.ReserveInput{Account: "acct-a", Operation: "reserve", ReservationID: "hold", Actor: "actor", Unit: "credits", Amount: 8, Deadline: now.Add(3 * time.Hour)}); err != nil {
				t.Fatal(err)
			}
			res, err := engine.Reservation(ctx, "acct-a", "hold")
			if err != nil || len(res.Allocations) != 3 || res.Allocations[0].LotID != "included" || res.Allocations[1].LotID != "promo" || res.Allocations[2].LotID != "purchased" {
				t.Fatalf("allocations=%+v err=%v", res.Allocations, err)
			}
		})
	}
}

func TestPostgresCarryoverAndRedeem(t *testing.T) {
	store, _ := testStore(t)
	createTestAccounts(t, store)
	now := testTime()
	engine := credit.New(store, func() time.Time { return now })
	ctx := t.Context()
	unit := billing.Unit{Code: "credits", Scale: 1}
	if _, err := engine.Redeem(ctx, credit.RedeemInput{Account: "acct-a", Operation: "promo", PromotionID: "spring", RedemptionKey: "code-1", Amount: 2, Ceiling: 2, Eligible: true, Member: "member-9", Unit: unit, ValidFrom: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Reserve(ctx, credit.ReserveInput{Account: "acct-a", Operation: "other-member", ReservationID: "hold-other", Actor: "actor", Unit: "credits", Scope: "member-8", Amount: 1, Deadline: now.Add(time.Hour)}); !errors.Is(err, billing.ErrInsufficient) {
		t.Fatalf("member isolation err=%v", err)
	}
	if _, err := engine.Grant(ctx, credit.GrantInput{Account: "acct-a", Operation: "g-inc", LotID: "included", Unit: unit, Amount: 10, Source: "included", SourceRef: "period-1", ValidFrom: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	got, err := engine.Carryover(ctx, credit.CarryoverInput{Account: "acct-a", Operation: "carry", FromLotID: "included", ToLotID: "next", PolicyVersion: "rollover-v1", Cap: 4, ExpiresAt: now.Add(2 * time.Hour)})
	if err != nil || got.LotID != "next" {
		t.Fatalf("carryover=%+v err=%v", got, err)
	}
}

func TestPostgresTimeoutDoesNotReleaseOtherHold(t *testing.T) {
	store, _ := testStore(t)
	createTestAccounts(t, store)
	now := testTime()
	clock := now
	engine := credit.New(store, func() time.Time { return clock })
	ctx := t.Context()
	unit := billing.Unit{Code: "credits", Scale: 1}
	if _, err := engine.Grant(ctx, credit.GrantInput{Account: "acct-a", Operation: "g", LotID: "lot", Unit: unit, Amount: 10, Source: "purchase", SourceRef: "pay", ValidFrom: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Reserve(ctx, credit.ReserveInput{Account: "acct-a", Operation: "ra", ReservationID: "a", Actor: "actor", Unit: "credits", Amount: 3, Deadline: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Reserve(ctx, credit.ReserveInput{Account: "acct-a", Operation: "rb", ReservationID: "b", Actor: "actor", Unit: "credits", Amount: 3, Deadline: now.Add(2 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	clock = now.Add(time.Hour)
	if _, err := engine.Settle(ctx, credit.SettleInput{Account: "acct-a", Operation: "late", ReservationID: "a", Actual: 1, Evidence: credit.Evidence{UsageID: "u", RatingVersion: "v", Metrics: []credit.Metric{{Name: "n", Quantity: 1}}}}); !errors.Is(err, billing.ErrExpired) {
		t.Fatalf("deadline err=%v", err)
	}
	b, err := engine.Reservation(ctx, "acct-a", "b")
	if err != nil || b.State != "held" {
		t.Fatalf("other hold=%+v err=%v", b, err)
	}
}
