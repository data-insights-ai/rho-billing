package pg

import (
	"errors"
	"sync"
	"testing"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/purchase"
)

func TestPostgresCollectionBindingConcurrentAccountsCannotShareTransaction(t *testing.T) {
	store, db := testStore(t)
	firstAccount := billing.AccountID("binding-race-first")
	if err := store.CreateAccount(t.Context(), firstAccount, string(firstAccount)); err != nil {
		t.Fatal(err)
	}
	first := purchase.New(store.Purchases(), testTime)
	secondStore := secondStore(t, db)
	second := purchase.New(secondStore.Purchases(), testTime)
	secondAccount := billing.AccountID("binding-race-second")
	if err := store.CreateAccount(t.Context(), secondAccount, string(secondAccount)); err != nil {
		t.Fatal(err)
	}
	scope := billing.Scope{Provider: "test", Merchant: "merchant", Environment: "sandbox"}
	firstIntent := mergedCollectionIntent(t, store, firstAccount, "binding-race-first", scope)
	secondIntent := mergedCollectionIntent(t, secondStore, secondAccount, "binding-race-second", scope)
	left := mergedCollectionInput(firstIntent, "binding-race-first", "shared-transaction")
	right := mergedCollectionInput(secondIntent, "binding-race-second", "shared-transaction")
	type result struct {
		binding purchase.CollectionBinding
		err     error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, call := range []struct {
		service *purchase.Service
		input   purchase.CollectionInput
	}{{first, left}, {second, right}} {
		wg.Go(func() {
			<-start
			out, err := call.service.BindCollection(t.Context(), call.input)
			results <- result{out, err}
		})
	}
	close(start)
	wg.Wait()
	close(results)
	var successes, conflicts int
	for got := range results {
		if got.err == nil {
			if got.binding.TransactionID != "shared-transaction" {
				t.Fatalf("invalid success: %+v", got.binding)
			}
			successes++
		} else {
			if !errors.Is(got.err, billing.ErrConflict) || got.binding.Account != "" || len(got.binding.Lines) != 0 {
				t.Fatalf("unexpected failure: %+v %v", got.binding, got.err)
			}
			conflicts++
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("successes=%d conflicts=%d", successes, conflicts)
	}
	var count int
	if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM billing_purchase_collection_bindings WHERE transaction_id='shared-transaction'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("bindings=%d error=%v", count, err)
	}
}
