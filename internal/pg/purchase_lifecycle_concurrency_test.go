package pg

import (
	"database/sql"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/purchase"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestPostgresPurchaseLifecycleFundingOwnershipAcrossAccounts(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	scope := billing.Scope{Provider: "stripe", Merchant: "merchant", Environment: "test"}
	accountA := billing.AccountID("purchase-life-a")
	accountB := billing.AccountID("purchase-life-b")
	for _, account := range []billing.AccountID{accountA, accountB} {
		if err := store.CreateAccount(ctx, account, string(account)); err != nil {
			t.Fatal(err)
		}
	}
	service := purchase.New(store.Purchases(), func() time.Time { return now })
	intentA := seedLifecycleIntent(t, service, accountA, "a", scope, now)
	intentB := seedLifecycleIntent(t, service, accountB, "b", scope, now)
	var schema string
	if err := db.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	secondDB, err := sql.Open("pgx", os.Getenv("BILLING_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	secondDB.SetMaxOpenConns(1)
	secondDB.SetMaxIdleConns(1)
	defer secondDB.Close()
	if _, err := secondDB.ExecContext(ctx, `SET search_path TO `+schema); err != nil {
		t.Fatal(err)
	}
	serviceB := purchase.New(New(secondDB).Purchases(), func() time.Time { return now })

	results := make(chan struct {
		result purchase.PaymentResult
		err    error
	}, 2)
	var wg sync.WaitGroup
	facts := []purchase.PaymentFact{
		{Account: accountA, Scope: scope, EventID: "event-owner-a", TransactionID: "transaction-shared", IntentID: intentA.ID, Status: purchase.FactPaid, Currency: "USD", Gross: 100, Lines: []purchase.PaidLine{{LineID: "line-a", Gross: 100}}, OccurredAt: now.Add(time.Minute), CollectedAt: now.Add(time.Minute)},
		{Account: accountB, Scope: scope, EventID: "event-owner-b", TransactionID: "transaction-shared", IntentID: intentB.ID, Status: purchase.FactPaid, Currency: "USD", Gross: 100, Lines: []purchase.PaidLine{{LineID: "line-b", Gross: 100}}, OccurredAt: now.Add(time.Minute), CollectedAt: now.Add(time.Minute)},
	}
	wg.Go(func() {
		result, err := service.ApplyPayment(ctx, facts[0])
		results <- struct {
			result purchase.PaymentResult
			err    error
		}{result, err}
	})
	wg.Go(func() {
		result, err := serviceB.ApplyPayment(ctx, facts[1])
		results <- struct {
			result purchase.PaymentResult
			err    error
		}{result, err}
	})
	wg.Wait()
	close(results)
	var applied int
	for outcome := range results {
		if outcome.err != nil && !errors.Is(outcome.err, billing.ErrConflict) {
			t.Fatalf("cross-account payment error=%v", outcome.err)
		}
		if outcome.result.Applied {
			applied++
		}
	}
	if applied != 1 {
		t.Fatalf("cross-account payment applied=%d, want exactly one", applied)
	}
	var fundingCount int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM billing_purchase_funding WHERE provider=$1 AND merchant=$2 AND environment=$3 AND transaction_id=$4`, scope.Provider, scope.Merchant, scope.Environment, "transaction-shared").Scan(&fundingCount); err != nil {
		t.Fatal(err)
	}
	if fundingCount != 1 {
		t.Fatalf("shared transaction funding rows=%d, want one", fundingCount)
	}
}

func TestPostgresPurchaseLifecycleConcurrentSameIntentFundsOnce(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	account := billing.AccountID("purchase-life-same")
	scope := billing.Scope{Provider: "stripe", Merchant: "merchant", Environment: "test"}
	if err := store.CreateAccount(ctx, account, string(account)); err != nil {
		t.Fatal(err)
	}
	service := purchase.New(store.Purchases(), func() time.Time { return now })
	intent := seedLifecycleIntent(t, service, account, "same", scope, now)

	var schema string
	if err := db.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	secondDB, err := sql.Open("pgx", os.Getenv("BILLING_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	secondDB.SetMaxOpenConns(1)
	secondDB.SetMaxIdleConns(1)
	defer secondDB.Close()
	if _, err := secondDB.ExecContext(ctx, `SET search_path TO `+schema); err != nil {
		t.Fatal(err)
	}
	second := purchase.New(New(secondDB).Purchases(), func() time.Time { return now })
	facts := []purchase.PaymentFact{
		{Account: account, Scope: scope, EventID: "event-same-a", TransactionID: "transaction-same", IntentID: intent.ID, Status: purchase.FactPaid, Currency: "USD", Gross: 100, Lines: []purchase.PaidLine{{LineID: "line-same", Gross: 100}}, OccurredAt: now.Add(time.Minute), CollectedAt: now.Add(time.Minute)},
		{Account: account, Scope: scope, EventID: "event-same-b", TransactionID: "transaction-same", IntentID: intent.ID, Status: purchase.FactPaid, Currency: "USD", Gross: 100, Lines: []purchase.PaidLine{{LineID: "line-same", Gross: 100}}, OccurredAt: now.Add(2 * time.Minute), CollectedAt: now.Add(time.Minute)},
	}
	results := make(chan struct {
		result purchase.PaymentResult
		err    error
	}, 2)
	var wg sync.WaitGroup
	wg.Go(func() {
		result, err := service.ApplyPayment(ctx, facts[0])
		results <- struct {
			result purchase.PaymentResult
			err    error
		}{result, err}
	})
	wg.Go(func() {
		result, err := second.ApplyPayment(ctx, facts[1])
		results <- struct {
			result purchase.PaymentResult
			err    error
		}{result, err}
	})
	wg.Wait()
	var applied int
	for range 2 {
		outcome := <-results
		if outcome.err != nil {
			t.Fatalf("same-intent payment error=%v", outcome.err)
		}
		if outcome.result.Applied {
			applied++
		} else {
			t.Fatalf("same-intent payment was not applied: %+v", outcome.result)
		}
	}
	if applied != 2 {
		t.Fatalf("same-intent payments applied=%d, want both observations applied", applied)
	}
	var fundingCount int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM billing_purchase_funding WHERE account_id=$1 AND transaction_id=$2`, account, "transaction-same").Scan(&fundingCount); err != nil {
		t.Fatal(err)
	}
	if fundingCount != 1 {
		t.Fatalf("same-intent funding rows=%d, want one", fundingCount)
	}
	var eventCount int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM billing_purchase_payment_events WHERE account_id=$1 AND intent_id=$2`, account, intent.ID).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if eventCount != 2 {
		t.Fatalf("same-intent payment event rows=%d, want two", eventCount)
	}
}

func seedLifecycleIntent(t *testing.T, service *purchase.Service, account billing.AccountID, suffix string, scope billing.Scope, now time.Time) purchase.Intent {
	t.Helper()
	offer := purchase.Offer{Account: account, Revision: purchase.Revision{ID: "offer-life-" + suffix, Version: 1}, Name: "Lifecycle"}
	if _, err := service.PublishOffer(t.Context(), offer); err != nil {
		t.Fatal(err)
	}
	price := purchase.Price{Account: account, Revision: purchase.Revision{ID: "price-life-" + suffix, Version: 1}, Offer: offer.Revision, Currency: "USD", UnitAmount: 100, TaxTreatment: purchase.TaxExclusive}
	if _, err := service.PublishPrice(t.Context(), price); err != nil {
		t.Fatal(err)
	}
	quote, err := service.CreateQuote(t.Context(), purchase.QuoteInput{Account: account, ID: "quote-life-" + suffix, ValidUntil: now.Add(time.Hour), Lines: []purchase.QuoteLineInput{{ID: "line-" + suffix, Price: price.Revision, Quantity: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	intent, err := service.CreateIntent(t.Context(), purchase.IntentInput{Account: account, ID: "intent-life-" + suffix, Operation: "operation-life-" + suffix, QuoteID: quote.ID, QuoteFingerprint: quote.Fingerprint(), Scope: scope, Actor: "actor-life", Reason: "concurrency test", ExpiresAt: now.Add(30 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	return intent
}
