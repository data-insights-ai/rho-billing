package host_test

import (
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
)

func TestHostRedeemsPromotionAndCarriesIncludedCredits(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	account := billing.AccountID("tenant-credits")
	engine := credit.New(credit.NewMemoryRepository(account), func() time.Time { return now })
	unit := billing.Unit{Code: "credits", Scale: 1}
	if _, err := engine.Grant(t.Context(), credit.GrantInput{Account: account, Operation: "included", LotID: "included", Unit: unit, Amount: 10, Source: "included", SourceRef: "period", ValidFrom: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	got, err := engine.Carryover(t.Context(), credit.CarryoverInput{Account: account, Operation: "carry", FromLotID: "included", ToLotID: "next", PolicyVersion: "rollover-v1", Cap: 2, ExpiresAt: now.Add(2 * time.Hour)})
	if err != nil || got.LotID != "next" {
		t.Fatalf("carryover=%+v err=%v", got, err)
	}
	promo, err := engine.Redeem(t.Context(), credit.RedeemInput{Account: account, Operation: "promo", PromotionID: "launch", RedemptionKey: "host-key", Amount: 3, Ceiling: 3, Eligible: true, Unit: unit, ValidFrom: now})
	if err != nil || promo.LotID == "" {
		t.Fatalf("redeem=%+v err=%v", promo, err)
	}
}
