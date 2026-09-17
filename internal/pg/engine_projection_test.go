package pg

import (
	"errors"
	"math"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
)

// A missing migration projection is a retryable setup condition, not a
// permanently recorded rejection of a customer's grant operation.
func TestLegacyProjectionReadinessDoesNotPoisonOperation(t *testing.T) {
	store, db := testStoreWithMigrations(t, releaseMigrationSnapshot(t))
	ctx := t.Context()
	if err := store.CreateAccount(ctx, "legacy-projection", "legacy-subject"); err != nil {
		t.Fatal(err)
	}
	markProjectionUnready(t, db, "legacy-projection")
	if err := store.WithinAccount(ctx, "legacy-projection", func(tx credit.Tx) error { return tx.PutLot(testLot("legacy-lot", 1)) }); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := testTime()
	engine := credit.New(store, func() time.Time { return now })
	input := credit.GrantInput{Account: "legacy-projection", Operation: "new-grant", LotID: "new-lot", Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 3, Source: "test", SourceRef: "new-lot", ValidFrom: now}
	if _, err := engine.Grant(ctx, input); !errors.Is(err, billing.ErrState) {
		t.Fatalf("unready error=%v", err)
	}
	if err := store.WithinAccount(ctx, input.Account, func(tx credit.Tx) error {
		_, found, err := tx.Outcome(input.Operation)
		if found {
			t.Fatal("unready projection persisted terminal grant outcome")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for {
		done, err := store.BuildCreditBalanceProjection(ctx, input.Account, 1)
		if err != nil {
			t.Fatal(err)
		}
		if done {
			break
		}
	}
	result, err := engine.Grant(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if result.Balance.Available != 13 {
		t.Fatalf("available=%d want legacy10+new3", result.Balance.Available)
	}
	retry, err := engine.Grant(ctx, input)
	if err != nil || retry != result {
		t.Fatalf("retry=%+v error=%v", retry, err)
	}
}

func TestPostgresFutureCreditTotalsDoNotOverflowCurrentBalance(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	if err := store.CreateAccount(ctx, "future-projection", "future-projection"); err != nil {
		t.Fatal(err)
	}
	now := testTime()
	engine := credit.New(store, func() time.Time { return now })
	for _, id := range []string{"future-a", "future-b"} {
		result, err := engine.Grant(ctx, credit.GrantInput{Account: "future-projection", Operation: billing.OperationID(id), LotID: id, Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: math.MaxInt64, Source: "test", SourceRef: id, ValidFrom: now.Add(time.Hour)})
		if err != nil || result.Balance.Available != 0 {
			t.Fatalf("future grant=%+v error=%v", result, err)
		}
	}
	if _, err := store.StoredCreditBalance(ctx, "future-projection", "credits", ""); !errors.Is(err, billing.ErrOverflow) {
		t.Fatalf("raw projection error=%v", err)
	}
	current, err := engine.Balance(ctx, "future-projection", "credits", "")
	if err != nil || current.Available != 0 {
		t.Fatalf("current=%+v error=%v", current, err)
	}
	now = now.Add(time.Hour)
	if _, err := engine.Balance(ctx, "future-projection", "credits", ""); !errors.Is(err, billing.ErrOverflow) {
		t.Fatalf("eligible overflow error=%v", err)
	}
}
