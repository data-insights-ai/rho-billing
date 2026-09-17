package purchase

import (
	"errors"
	"math"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
)

func TestRecoveredChargebackAllowsNewDebitWithoutRepeatingBenefits(t *testing.T) {
	now := billing.CanonicalTime(time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC))
	service, repo, scope, intent, quote := adjustmentPurchaseFixture(t, []Effect{{Key: "credit", Credit: &CreditBenefit{Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 5}}}, now)
	debit := AdjustmentInput{Account: intent.Account, ID: "debit-original", IntentID: intent.ID, ProviderAdjustmentID: "provider-debit-original", TransactionID: "adjustment-tx", Scope: scope, Kind: AdjustmentChargeback, Currency: quote.Currency, Lines: []PaidLine{{LineID: "line", Gross: 100}}, PolicyVersion: "policy", CreditPolicy: CreditRefundFullOnly, Actor: "provider", Reason: "chargeback", OccurredAt: now.Add(2 * time.Minute)}
	first, err := service.ApplyAdjustment(t.Context(), debit)
	if err != nil || !first.Applied || len(first.Effects) != 1 {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	pending := debit
	pending.ID = "debit-new"
	pending.ProviderAdjustmentID = "provider-debit-new"
	if result, err := service.ApplyAdjustment(t.Context(), pending); !errors.Is(err, billing.ErrConflict) || result.Applied {
		t.Fatalf("debit before recovery=%+v err=%v", result, err)
	}
	fact := DisputeFact{Account: intent.Account, Scope: scope, EventID: "recovered", DisputeID: "original-case", IntentID: intent.ID, TransactionID: debit.TransactionID, Currency: quote.Currency, Amount: 100, Status: DisputeWon, OccurredAt: debit.OccurredAt, EvidenceReference: "recovery-evidence", DebitAdjustmentID: debit.ID, Recovery: &DisputeRecovery{ID: "recovery", AdjustmentID: debit.ID, EvidenceReference: "recovery-evidence", Lines: debit.Lines}}
	for range 2 {
		if result, err := service.ApplyDispute(t.Context(), fact); err != nil || !result.Applied {
			t.Fatalf("recovery=%+v err=%v", result, err)
		}
	}
	debit.ID = "debit-new"
	debit.ProviderAdjustmentID = "provider-debit-new"
	result, err := service.ApplyAdjustment(t.Context(), debit)
	if err != nil || !result.Applied || len(result.Effects) != 0 {
		t.Fatalf("new debit=%+v err=%v", result, err)
	}
	if err := repo.WithinAccount(t.Context(), intent.Account, func(tx Tx) error {
		state, err := tx.AdjustmentState(t.Context(), intent.ID)
		if err != nil {
			return err
		}
		if len(state.Lines) != 1 || state.Lines[0].ChargebackGross != 200 {
			t.Fatalf("lifetime state=%+v", state)
		}
		recovered, err := tx.ChargebackRecoveries(t.Context(), intent.ID)
		if err != nil {
			return err
		}
		if len(recovered) != 1 || recovered[0].Gross != 100 {
			t.Fatalf("recoveries=%+v", recovered)
		}
		balance, err := credit.New(tx.Credits(), func() time.Time { return now.Add(3 * time.Minute) }).Balance(t.Context(), intent.Account, "credits", "")
		if err != nil {
			return err
		}
		if balance.Available != 0 {
			t.Fatalf("recovery restored credits=%+v", balance)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	debit.ID = "debit-excess"
	debit.ProviderAdjustmentID = "provider-debit-excess"
	debit.Lines = []PaidLine{{LineID: "line", Gross: 1}}
	if result, err := service.ApplyAdjustment(t.Context(), debit); !errors.Is(err, billing.ErrConflict) || result.Applied {
		t.Fatalf("excess debit=%+v err=%v", result, err)
	}
}

func TestRecoveryExposureEnforcesGrossTaxAndNetIndependently(t *testing.T) {
	funding := Funding{Lines: []PaidLine{{LineID: "line", Gross: 100, Tax: 20}}}
	state := AdjustmentState{Lines: []LineAdjustmentTotal{{LineID: "line", ChargebackGross: 100, ChargebackTax: 20}}}
	recovered := []PaidLine{{LineID: "line", Gross: 50, Tax: 10}}
	for _, test := range []struct {
		name     string
		line     PaidLine
		accepted bool
	}{
		{"exact", PaidLine{LineID: "line", Gross: 50, Tax: 10}, true},
		{"gross", PaidLine{LineID: "line", Gross: 51, Tax: 10}, false},
		{"tax", PaidLine{LineID: "line", Gross: 50, Tax: 11}, false},
		{"net", PaidLine{LineID: "line", Gross: 50, Tax: 9}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			next, reason, err := nextAdjustmentState(state, funding, AdjustmentInput{Kind: AdjustmentChargeback, Lines: []PaidLine{test.line}}, recovered)
			if !test.accepted && errors.Is(err, billing.ErrConflict) {
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if (reason == "") != test.accepted {
				t.Fatalf("reason=%s next=%+v", reason, next)
			}
		})
	}
	for _, bad := range []PaidLine{{LineID: "line", Gross: 101, Tax: 20}, {LineID: "line", Gross: 50, Tax: 21}, {LineID: "line", Gross: 90, Tax: 0}} {
		if _, _, err := nextAdjustmentState(state, funding, AdjustmentInput{}, []PaidLine{bad}); err == nil {
			t.Fatalf("invalid recovery accepted=%+v", bad)
		}
	}
}

func TestRecoveryExposureRejectsLifetimeOverflow(t *testing.T) {
	state := AdjustmentState{Lines: []LineAdjustmentTotal{{LineID: "line", ChargebackGross: math.MaxInt64}}}
	funding := Funding{Lines: []PaidLine{{LineID: "line", Gross: math.MaxInt64}}}
	_, _, err := nextAdjustmentState(state, funding, AdjustmentInput{Kind: AdjustmentChargeback, Lines: []PaidLine{{LineID: "line", Gross: 1}}}, []PaidLine{{LineID: "line", Gross: math.MaxInt64}})
	if !errors.Is(err, billing.ErrOverflow) {
		t.Fatalf("overflow=%v", err)
	}
}

func TestRecoveredPartialDebitPreservesProductExposureHighWater(t *testing.T) {
	now := billing.CanonicalTime(time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC))
	service, repo, scope, intent, _ := adjustmentPurchaseFixture(t, []Effect{{Key: "credit", Credit: &CreditBenefit{Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 100}}}, now)
	debit := AdjustmentInput{Account: intent.Account, ID: "partial-original", IntentID: intent.ID, ProviderAdjustmentID: "provider-partial-original", TransactionID: "adjustment-tx", Scope: scope, Kind: AdjustmentChargeback, Currency: "USD", Lines: []PaidLine{{LineID: "line", Gross: 50}}, PolicyVersion: "policy", CreditPolicy: CreditRefundProportional, Actor: "provider", Reason: "partial", OccurredAt: now.Add(2 * time.Minute)}
	if result, err := service.ApplyAdjustment(t.Context(), debit); err != nil || !result.Applied || len(result.Effects) != 1 || result.Effects[0].TargetDelta != 50 {
		t.Fatalf("first=%+v err=%v", result, err)
	}
	recovery := DisputeFact{Account: intent.Account, Scope: scope, EventID: "partial-recovered", DisputeID: "partial-case", IntentID: intent.ID, TransactionID: debit.TransactionID, Currency: "USD", Amount: 50, Status: DisputeWon, OccurredAt: debit.OccurredAt, EvidenceReference: "evidence", DebitAdjustmentID: debit.ID, Recovery: &DisputeRecovery{ID: "partial-recovery", AdjustmentID: debit.ID, EvidenceReference: "evidence", Lines: debit.Lines}}
	if result, err := service.ApplyDispute(t.Context(), recovery); err != nil || !result.RecoveryApplied {
		t.Fatalf("recovery=%+v err=%v", result, err)
	}
	debit.ID = "partial-new"
	debit.ProviderAdjustmentID = "provider-partial-new"
	if result, err := service.ApplyAdjustment(t.Context(), debit); err != nil || !result.Applied || len(result.Effects) != 0 {
		t.Fatalf("repeated peak=%+v err=%v", result, err)
	}
	debit.ID = "partial-higher"
	debit.ProviderAdjustmentID = "provider-partial-higher"
	debit.Lines = []PaidLine{{LineID: "line", Gross: 25}}
	if result, err := service.ApplyAdjustment(t.Context(), debit); err != nil || !result.Applied || len(result.Effects) != 1 || result.Effects[0].TargetDelta != 25 {
		t.Fatalf("new peak=%+v err=%v", result, err)
	}
	if err := repo.WithinAccount(t.Context(), intent.Account, func(tx Tx) error {
		balance, err := credit.New(tx.Credits(), func() time.Time { return now.Add(3 * time.Minute) }).Balance(t.Context(), intent.Account, "credits", "")
		if err == nil && balance.Available != 25 {
			t.Fatalf("balance=%+v", balance)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
