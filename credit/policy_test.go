package credit

import (
	"errors"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
)

func TestFEFOSpansPurchasedIncludedPromotionalLots(t *testing.T) {
	engine, repo, clock := newTestEngine()
	now := clock.Now()
	unit := billing.Unit{Code: testUnit, Scale: 1000}
	grants := []struct {
		id, source string
		amount     int64
		expires    time.Duration
	}{
		{"included", "included", 3, time.Hour},
		{"promo", "promotion", 4, 2 * time.Hour},
		{"purchased", "purchase", 5, 0},
	}
	for _, g := range grants {
		var expires time.Time
		if g.expires != 0 {
			expires = now.Add(g.expires)
		}
		if _, err := engine.Grant(t.Context(), GrantInput{Account: testAccount, Operation: billing.OperationID("grant-" + g.id), LotID: g.id, Unit: unit, Amount: g.amount, Source: g.source, SourceRef: "cause-" + g.id, ValidFrom: now.Add(-time.Minute), ExpiresAt: expires}); err != nil {
			t.Fatal(err)
		}
	}
	reserve(t, engine, clock, "reserve-fefo-sources", "res-fefo-sources", 8, 3*time.Hour, "actor", "")
	got := reservation(t, engine, "res-fefo-sources")
	if len(got.Allocations) != 3 || got.Allocations[0] != (Allocation{LotID: "included", Amount: 3}) || got.Allocations[1] != (Allocation{LotID: "promo", Amount: 4}) || got.Allocations[2] != (Allocation{LotID: "purchased", Amount: 1}) {
		t.Fatalf("FEFO provenance allocations=%#v", got.Allocations)
	}
	if err := repo.WithinAccount(t.Context(), testAccount, func(tx Tx) error {
		want := map[string]string{"included": "included", "promo": "promotion", "purchased": "purchase"}
		for id, source := range want {
			lot, ok, err := tx.Lot(id)
			if err != nil || !ok || lot.Source != source || lot.SourceRef != "cause-"+id {
				t.Fatalf("lot %s %+v err=%v", id, lot, err)
			}
		}
		inc, _, _ := tx.Lot("included")
		pro, _, _ := tx.Lot("promo")
		pur, _, _ := tx.Lot("purchased")
		if inc.ExpiresAt.Equal(pro.ExpiresAt) || inc.ExpiresAt.Equal(pur.ExpiresAt) || !pur.ExpiresAt.IsZero() {
			t.Fatalf("expiry collapsed included=%v promo=%v purchased=%v", inc.ExpiresAt, pro.ExpiresAt, pur.ExpiresAt)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRolloverCappedAndOffByDefault(t *testing.T) {
	engine, _, clock := newTestEngine()
	now := clock.Now()
	unit := billing.Unit{Code: testUnit, Scale: 1000}
	if _, err := engine.Grant(t.Context(), GrantInput{Account: testAccount, Operation: "grant-included", LotID: "included", Unit: unit, Amount: 10, Source: "included", SourceRef: "period-1", ValidFrom: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Hour)
	bal, err := engine.Balance(t.Context(), testAccount, testUnit, "")
	if err != nil || bal.Expired != 10 || bal.Available != 0 {
		t.Fatalf("default no rollover %+v err=%v", bal, err)
	}

	engine, repo, clock := newTestEngine()
	now = clock.Now()
	if _, err := engine.Grant(t.Context(), GrantInput{Account: testAccount, Operation: "grant-included", LotID: "included", Unit: unit, Amount: 10, Source: "included", SourceRef: "period-1", ValidFrom: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	carried, err := engine.Carryover(t.Context(), CarryoverInput{Account: testAccount, Operation: "carry-v1", FromLotID: "included", ToLotID: "included-next", PolicyVersion: "rollover-v1", Cap: 3, ExpiresAt: now.Add(2 * time.Hour)})
	if err != nil || carried.LotID != "included-next" {
		t.Fatalf("carryover=%+v err=%v", carried, err)
	}
	if err := repo.WithinAccount(t.Context(), testAccount, func(tx Tx) error {
		from, _, err := tx.Lot("included")
		if err != nil {
			return err
		}
		to, _, err := tx.Lot("included-next")
		if err != nil {
			return err
		}
		if from.Available != 0 || from.Expired != 10 || to.Available != 3 || to.Source != "rollover" {
			t.Fatalf("from=%+v to=%+v", from, to)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Carryover(t.Context(), CarryoverInput{Account: testAccount, Operation: "carry-v1-dup", FromLotID: "included", ToLotID: "included-dup", PolicyVersion: "rollover-v1", Cap: 3, ExpiresAt: now.Add(2 * time.Hour)}); err == nil {
		t.Fatal("duplicate carryover minted a second successor")
	}
	if err := repo.WithinAccount(t.Context(), testAccount, func(tx Tx) error {
		if _, ok, err := tx.Lot("included-dup"); err != nil || ok {
			t.Fatalf("second successor exists ok=%v err=%v", ok, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	replay, err := engine.Carryover(t.Context(), CarryoverInput{Account: testAccount, Operation: "carry-v1", FromLotID: "included", ToLotID: "included-next", PolicyVersion: "rollover-v1", Cap: 3, ExpiresAt: now.Add(2 * time.Hour)})
	if err != nil || replay.LotID != carried.LotID {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
}

func TestPromotionRedemptionAndMemberScope(t *testing.T) {
	engine, _, clock := newTestEngine()
	now := clock.Now()
	unit := billing.Unit{Code: testUnit, Scale: 1000}
	if _, err := engine.Redeem(t.Context(), RedeemInput{Account: testAccount, Operation: "promo-no", PromotionID: "spring", RedemptionKey: "key-a", Amount: 5, Ceiling: 5, Eligible: false, Unit: unit, ValidFrom: now}); !errors.Is(err, billing.ErrInvalid) {
		t.Fatalf("ineligible err=%v", err)
	}
	if _, err := engine.Redeem(t.Context(), RedeemInput{Account: testAccount, Operation: "promo-cap", PromotionID: "spring", RedemptionKey: "key-b", Amount: 9, Ceiling: 5, Eligible: true, Unit: unit, ValidFrom: now}); !errors.Is(err, billing.ErrLimit) {
		t.Fatalf("ceiling err=%v", err)
	}
	first, err := engine.Redeem(t.Context(), RedeemInput{Account: testAccount, Operation: "promo-ok", PromotionID: "spring", RedemptionKey: "key-c", Amount: 5, Ceiling: 5, Eligible: true, Member: "member-1", Unit: unit, ValidFrom: now, ExpiresAt: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	replay, err := engine.Redeem(t.Context(), RedeemInput{Account: testAccount, Operation: "promo-ok", PromotionID: "spring", RedemptionKey: "key-c", Amount: 5, Ceiling: 5, Eligible: true, Member: "member-1", Unit: unit, ValidFrom: now, ExpiresAt: now.Add(time.Hour)})
	if err != nil || replay.LotID != first.LotID {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	if _, err := engine.Redeem(t.Context(), RedeemInput{Account: testAccount, Operation: "promo-dup", PromotionID: "spring", RedemptionKey: "key-c", Amount: 5, Ceiling: 5, Eligible: true, Member: "member-1", Unit: unit, ValidFrom: now, ExpiresAt: now.Add(time.Hour)}); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("duplicate key err=%v", err)
	}
	if _, err := engine.Reserve(t.Context(), ReserveInput{Account: testAccount, Operation: "reserve-other", ReservationID: "res-other", Actor: "actor", Unit: testUnit, Scope: "member-2", Amount: 1, Deadline: now.Add(time.Hour)}); !errors.Is(err, billing.ErrInsufficient) {
		t.Fatalf("foreign member spent grant err=%v", err)
	}
	if _, err := engine.Reserve(t.Context(), ReserveInput{Account: testAccount, Operation: "reserve-member", ReservationID: "res-member", Actor: "actor", Unit: testUnit, Scope: "member-1", Amount: 5, Deadline: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
}

func TestFailedSettleAndTimeoutReleaseOnlyOwnHold(t *testing.T) {
	engine, _, clock := newTestEngine()
	grant(t, engine, clock, "shared", 10, 0, "")
	reserve(t, engine, clock, "reserve-a", "res-a", 4, time.Hour, "actor", "")
	reserve(t, engine, clock, "reserve-b", "res-b", 4, 2*time.Hour, "actor", "")
	if _, err := engine.Settle(t.Context(), SettleInput{Account: testAccount, Operation: "settle-over", ReservationID: "res-a", Actual: 5, Evidence: Evidence{UsageID: "too-much", RatingVersion: "v1", Metrics: []Metric{{Name: "u", Quantity: 5}}}}); !errors.Is(err, billing.ErrInvalid) {
		t.Fatalf("overrun err=%v", err)
	}
	if got := reservation(t, engine, "res-a"); got.State != "held" {
		t.Fatalf("failed settle released a=%+v", got)
	}
	if got := reservation(t, engine, "res-b"); got.State != "held" || got.Authorized != 4 {
		t.Fatalf("failed settle touched b=%+v", got)
	}
	clock.Advance(time.Hour)
	if _, err := engine.Settle(t.Context(), SettleInput{Account: testAccount, Operation: "settle-late-a", ReservationID: "res-a", Actual: 1, Evidence: Evidence{UsageID: "late", RatingVersion: "v1", Metrics: []Metric{{Name: "u", Quantity: 1}}}}); !errors.Is(err, billing.ErrExpired) {
		t.Fatalf("deadline err=%v", err)
	}
	if got := reservation(t, engine, "res-a"); got.State != "timed_out" {
		t.Fatalf("timeout a=%+v", got)
	}
	if got := reservation(t, engine, "res-b"); got.State != "held" {
		t.Fatalf("timeout released b=%+v", got)
	}
}

func TestRevokeIncludedLeavesPurchasedAndRaceConserves(t *testing.T) {
	engine, repo, clock := newTestEngine()
	now := clock.Now()
	unit := billing.Unit{Code: testUnit, Scale: 1000}
	if _, err := engine.Grant(t.Context(), GrantInput{Account: testAccount, Operation: "grant-included", LotID: "included", Unit: unit, Amount: 6, Source: "included", SourceRef: "allowance", ValidFrom: now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Grant(t.Context(), GrantInput{Account: testAccount, Operation: "grant-purchased", LotID: "purchased", Unit: unit, Amount: 8, Source: "purchase", SourceRef: "payment", ValidFrom: now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	reserve(t, engine, clock, "reserve-work", "res-work", 4, time.Hour, "actor", "")
	if _, err := engine.Revoke(t.Context(), RevokeInput{Account: testAccount, Operation: "cancel-included", LotID: "included", Reason: "subscription canceled"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.WithinAccount(t.Context(), testAccount, func(tx Tx) error {
		inc, _, _ := tx.Lot("included")
		pur, _, _ := tx.Lot("purchased")
		if inc.RevokedAt.IsZero() || !pur.RevokedAt.IsZero() {
			t.Fatalf("cancellation revoked purchased inc=%+v pur=%+v", inc, pur)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Release(t.Context(), ReleaseInput{Account: testAccount, Operation: "cancel-work", ReservationID: "res-work", Reason: "canceled"}); err != nil {
		t.Fatal(err)
	}
	bal, err := engine.Balance(t.Context(), testAccount, testUnit, "")
	if err != nil || bal.Available != 8 || bal.Held != 0 {
		t.Fatalf("purchased remaining %+v err=%v", bal, err)
	}
}
