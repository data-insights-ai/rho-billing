package pg

import (
	"errors"
	"fmt"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/billingtest"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/integration"
)

// Keep the in-process and durable repositories on one public behavioral
// contract. Backend-specific tests remain separate for SQL constraints and
// transaction details; this suite checks observable credit outcomes.
func TestCreditContractMemoryAndPostgres(t *testing.T) {
	t.Run("memory", func(t *testing.T) {
		billingtest.RunCreditContract(t, credit.NewMemoryRepository("acct-a", "acct-b"), "acct-a", "acct-b")
	})
	t.Run("postgres", func(t *testing.T) {
		store, _ := testStore(t)
		createTestAccounts(t, store)
		billingtest.RunCreditContract(t, store, "acct-a", "acct-b")
	})
}

func TestUnitScaleMismatchPersistsRejectionInComposedSession(t *testing.T) {
	store, _ := testStore(t)
	createTestAccounts(t, store)
	ctx := t.Context()
	now := testTime()

	first := credit.GrantInput{
		Account:   "acct-a",
		Operation: "unit-scale-first",
		LotID:     "unit-scale-first-lot",
		Unit:      billing.Unit{Code: "credits", Scale: 1},
		Amount:    1,
		Source:    "contract",
		SourceRef: "unit-scale-first",
		ValidFrom: now,
	}
	if _, err := credit.New(store, testTime).Grant(ctx, first); err != nil {
		t.Fatal(err)
	}

	mismatch := credit.GrantInput{
		Account:   "acct-b",
		Operation: "unit-scale-composed",
		LotID:     "unit-scale-composed-lot",
		Unit:      billing.Unit{Code: "credits", Scale: 2},
		Amount:    1,
		Source:    "contract",
		SourceRef: "unit-scale-composed",
		ValidFrom: now,
	}
	if err := store.Atomic(ctx, "acct-b", func(session integration.Session) error {
		_, err := credit.New(session.Credits(), testTime).Grant(ctx, mismatch)
		if !errors.Is(err, billing.ErrConflict) || !credit.IsRejection(err) {
			t.Fatalf("composed mismatch: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := credit.New(store, testTime).Grant(ctx, mismatch); !errors.Is(err, billing.ErrConflict) || !credit.IsRejection(err) {
		t.Fatalf("replayed composed mismatch: %v", err)
	}
}

func BenchmarkSettledForLimitQuery(b *testing.B) {
	store, _ := testStore(b)
	if err := store.CreateAccount(b.Context(), "acct-benchmark", "subject-benchmark"); err != nil {
		b.Fatal(err)
	}
	now := testTime()
	if err := store.WithinAccount(b.Context(), "acct-benchmark", func(tx credit.Tx) error {
		for i := 0; i < 1000; i++ {
			if err := tx.PutReservation(credit.Reservation{ID: fmt.Sprintf("settled-%04d", i), Actor: "actor", Unit: "credits", CreatedAt: now, Deadline: now.Add(time.Hour), State: "settled", Authorized: 2, Consumed: 2}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		b.Fatal(err)
	}
	period := billing.Period{Start: now.Add(-time.Hour), End: now.Add(time.Hour)}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := store.WithinAccount(b.Context(), "acct-benchmark", func(tx credit.Tx) error {
			_, err := tx.SettledForLimit("actor", "credits", period)
			return err
		}); err != nil {
			b.Fatal(err)
		}
	}
}
