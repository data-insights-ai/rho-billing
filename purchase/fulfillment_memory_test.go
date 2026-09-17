package purchase

import (
	"context"
	"errors"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/catalog"
	"github.com/data-insights-ai/rho-billing/credit"
)

func TestMemoryFulfillmentDomainsRollbackAndRetryTogether(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	account := billing.AccountID("fulfillment-memory")
	plan := catalog.PlanVersion{ID: "plan-memory-v1", PlanID: "plan-memory", Version: 1}
	repo := NewMemoryRepository(ReferenceAccount{Account: account, Plans: []catalog.PlanVersion{plan}})
	ctx := t.Context()
	grant := func(tx Tx) error {
		if _, err := credit.New(tx.Credits(), func() time.Time { return now }).Grant(ctx, credit.GrantInput{
			Account: account, Operation: "grant-memory", LotID: "lot-memory", Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 5,
			Source: "purchase", SourceRef: "effect-memory", ValidFrom: now,
		}); err != nil {
			return err
		}
		_, err := catalog.NewEntitlement(tx.Entitlements(), func() time.Time { return now }).Assign(ctx, catalog.Assignment{
			Account:   account,
			Plan:      catalog.PlanAssignment{ID: "assignment-memory", PlanVersionID: plan.ID, Quantity: 1, Effective: billing.Period{Start: now, End: now.Add(time.Hour)}, Source: catalog.SourcePurchase},
			SourceRef: "effect-memory", Actor: "actor-memory", Reason: "purchase fulfillment",
		})
		return err
	}
	wantErr := errors.New("rollback mixed fulfillment")
	if err := repo.WithinAccount(ctx, account, func(tx Tx) error {
		if err := grant(tx); err != nil {
			return err
		}
		return wantErr
	}); !errors.Is(err, wantErr) {
		t.Fatalf("callback error=%v, want %v", err, wantErr)
	}
	assertMixedStateEmpty(t, repo, account, now)

	cancelCtx, cancel := context.WithCancel(ctx)
	if err := repo.WithinAccount(cancelCtx, account, func(tx Tx) error {
		if err := grant(tx); err != nil {
			return err
		}
		cancel()
		return nil
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled callback error=%v, want canceled", err)
	}
	assertMixedStateEmpty(t, repo, account, now)

	if err := repo.WithinAccount(ctx, account, grant); err != nil {
		t.Fatal(err)
	}
	if err := repo.WithinAccount(ctx, account, func(tx Tx) error {
		balance, err := credit.New(tx.Credits(), func() time.Time { return now }).Balance(ctx, account, "credits", "")
		if err != nil {
			return err
		}
		if balance.Available != 5 {
			t.Fatalf("balance=%+v, want five credits", balance)
		}
		assignment, err := catalog.NewEntitlement(tx.Entitlements(), func() time.Time { return now }).Assignment(ctx, account, "assignment-memory")
		if err != nil {
			return err
		}
		if assignment.Plan.PlanVersionID != plan.ID {
			t.Fatalf("assignment=%+v", assignment)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func assertMixedStateEmpty(t *testing.T, repo Repository, account billing.AccountID, now time.Time) {
	t.Helper()
	if err := repo.WithinAccount(t.Context(), account, func(tx Tx) error {
		balance, err := credit.New(tx.Credits(), func() time.Time { return now }).Balance(t.Context(), account, "credits", "")
		if err != nil {
			return err
		}
		if balance != (credit.Balance{}) {
			t.Fatalf("rolled-back balance=%+v", balance)
		}
		if _, err := catalog.NewEntitlement(tx.Entitlements(), func() time.Time { return now }).Assignment(t.Context(), account, "assignment-memory"); !errors.Is(err, billing.ErrNotFound) {
			return errors.New("rolled-back entitlement still exists")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
