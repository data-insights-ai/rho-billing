package pg

import (
	"errors"
	"reflect"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/purchase"
)

func mergedCollectionIntent(t *testing.T, store *Store, account billing.AccountID, suffix string, scope billing.Scope) purchase.Intent {
	t.Helper()
	service := purchase.New(store.Purchases(), testTime)
	offer := purchase.Offer{Account: account, Revision: purchase.Revision{ID: "merged-offer-" + suffix, Version: 1}, Name: "Merged", PublishedAt: testTime()}
	if _, err := service.PublishOffer(t.Context(), offer); err != nil {
		t.Fatal(err)
	}
	price := purchase.Price{Account: account, Revision: purchase.Revision{ID: "merged-price-" + suffix, Version: 1}, Offer: offer.Revision, Currency: "USD", UnitAmount: 100, TaxTreatment: purchase.TaxExclusive, PublishedAt: testTime()}
	if _, err := service.PublishPrice(t.Context(), price); err != nil {
		t.Fatal(err)
	}
	quote, err := service.CreateQuote(t.Context(), purchase.QuoteInput{Account: account, ID: "merged-quote-" + suffix, ValidUntil: testTime().Add(time.Hour), Lines: []purchase.QuoteLineInput{
		{ID: "merged-line-a-" + suffix, Price: price.Revision, Quantity: 2},
		{ID: "merged-line-b-" + suffix, Price: price.Revision, Quantity: 3},
	}})
	if err != nil {
		t.Fatal(err)
	}
	intent, err := service.CreateIntent(t.Context(), purchase.IntentInput{Account: account, ID: "merged-intent-" + suffix, Operation: "merged-operation-" + suffix, QuoteID: quote.ID, QuoteFingerprint: quote.Fingerprint(), Scope: scope, Actor: "operator", Reason: "merged binding", ExpiresAt: testTime().Add(30 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	return intent
}

func mergedCollectionInput(intent purchase.Intent, suffix, transactionID string) purchase.CollectionInput {
	return purchase.CollectionInput{
		Account: intent.Account, Scope: intent.Scope, TransactionID: transactionID, IntentID: intent.ID,
		QuoteFingerprint: intent.QuoteFingerprint, CustomerID: "customer", Actor: "operator", Reason: "merged binding", EvidenceReference: "evidence",
		Lines: []purchase.CollectionLine{{ProviderLineID: "provider-line-" + suffix, ProviderPriceID: "provider-price", Quantity: 5, Allocations: []purchase.CollectionAllocation{
			{QuoteLineID: "merged-line-a-" + suffix, Quantity: 2},
			{QuoteLineID: "merged-line-b-" + suffix, Quantity: 3},
		}}},
	}
}

func collectionInput(intent purchase.Intent, transactionID string) purchase.CollectionInput {
	return purchase.CollectionInput{
		Account: intent.Account, Scope: intent.Scope, TransactionID: transactionID,
		IntentID: intent.ID, QuoteFingerprint: intent.QuoteFingerprint, CustomerID: "customer",
		Lines: []purchase.CollectionLine{{ProviderLineID: "provider-line", Quantity: 1, Allocations: []purchase.CollectionAllocation{{QuoteLineID: "line-" + transactionID, Quantity: 1}}}},
		Actor: "operator", Reason: "verified collection", EvidenceReference: "evidence",
	}
}

func TestPostgresCollectionBindingReplayQuantityAndGlobalOwner(t *testing.T) {
	store, _, intent := purchaseLifecycleFixture(t, "collection-owner", "collection-owner")
	ctx := t.Context()
	service := purchase.New(store.Purchases(), testTime)
	in := collectionInput(intent, "collection-owner")

	first, err := service.BindCollection(ctx, in)
	if err != nil || first.Account != intent.Account || first.Lines[0].Quantity != 1 || first.QuoteFingerprint != intent.QuoteFingerprint {
		t.Fatalf("binding=%+v err=%v", first, err)
	}
	replay, err := service.BindCollection(ctx, in)
	if err != nil || replay.Fingerprint() != first.Fingerprint() || !replay.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	changed := in
	changed.Reason = "changed evidence"
	if out, err := service.BindCollection(ctx, changed); !errors.Is(err, billing.ErrConflict) || !reflect.DeepEqual(out, purchase.CollectionBinding{}) {
		t.Fatalf("changed replay=%+v err=%v", out, err)
	}
	wrongQuantity := in
	wrongQuantity.TransactionID = "wrong-quantity"
	wrongQuantity.Lines = []purchase.CollectionLine{{ProviderLineID: "wrong-provider-line", Quantity: 2, Allocations: []purchase.CollectionAllocation{{QuoteLineID: "line-collection-owner", Quantity: 2}}}}
	if out, err := service.BindCollection(ctx, wrongQuantity); !errors.Is(err, billing.ErrConflict) || !reflect.DeepEqual(out, purchase.CollectionBinding{}) {
		t.Fatalf("wrong quantity=%+v err=%v", out, err)
	}
	if err := store.Purchases().WithinAccount(ctx, intent.Account, func(tx purchase.Tx) error {
		return tx.InsertCollectionBinding(ctx, purchase.CollectionBinding{CollectionInput: wrongQuantity, CreatedAt: testTime()})
	}); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("repository wrong quantity error=%v", err)
	}

	foreign := billing.AccountID("collection-foreign-owner")
	if err := store.CreateAccount(ctx, foreign, string(foreign)); err != nil {
		t.Fatal(err)
	}
	if out, err := service.CollectionBinding(ctx, foreign, in.Scope, in.TransactionID); !errors.Is(err, billing.ErrConflict) || !reflect.DeepEqual(out, purchase.CollectionBinding{}) {
		t.Fatalf("foreign owner=%+v err=%v", out, err)
	}
}

func TestPostgresCollectionBindingMergedAllocationsAndDetachedRead(t *testing.T) {
	store, db := testStore(t)
	account := billing.AccountID("collection-merged")
	if err := store.CreateAccount(t.Context(), account, string(account)); err != nil {
		t.Fatal(err)
	}
	scope := billing.Scope{Provider: "test", Merchant: "merchant", Environment: "sandbox"}
	intent := mergedCollectionIntent(t, store, account, "merged", scope)
	service := purchase.New(store.Purchases(), testTime)
	in := mergedCollectionInput(intent, "merged", "merged-transaction")
	bound, err := service.BindCollection(t.Context(), in)
	if err != nil || len(bound.Lines) != 1 || bound.Lines[0].Quantity != 5 || len(bound.Lines[0].Allocations) != 2 {
		t.Fatalf("bound=%+v err=%v", bound, err)
	}
	var nested, legacy bool
	if err := db.QueryRowContext(t.Context(), `SELECT binding #> '{Lines,0,Allocations}' IS NOT NULL,binding #> '{Lines,0,QuoteLineID}' IS NOT NULL FROM billing_purchase_collection_bindings WHERE account_id=$1`, account).Scan(&nested, &legacy); err != nil || !nested || legacy {
		t.Fatalf("stored nested=%v legacy=%v err=%v", nested, legacy, err)
	}
	bound.Lines[0].Allocations[0].Quantity = 99
	read, err := service.CollectionBinding(t.Context(), account, scope, in.TransactionID)
	if err != nil || read.Lines[0].Allocations[0].Quantity != 2 {
		t.Fatalf("detached read=%+v err=%v", read, err)
	}
	reordered := in
	reordered.Lines[0].Allocations = []purchase.CollectionAllocation{in.Lines[0].Allocations[1], in.Lines[0].Allocations[0]}
	if replay, err := service.BindCollection(t.Context(), reordered); err != nil || replay.Fingerprint() != read.Fingerprint() {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	changed := in
	changed.Lines[0].Allocations = []purchase.CollectionAllocation{{QuoteLineID: "merged-line-a-merged", Quantity: 1}, {QuoteLineID: "merged-line-b-merged", Quantity: 4}}
	if out, err := service.BindCollection(t.Context(), changed); !errors.Is(err, billing.ErrConflict) || !reflect.DeepEqual(out, purchase.CollectionBinding{}) {
		t.Fatalf("changed allocation=%+v err=%v", out, err)
	}
}

func TestPostgresCollectionBindingRejectsStoredIntegrityDamage(t *testing.T) {
	t.Run("malformed JSON", func(t *testing.T) {
		store, db, intent := purchaseLifecycleFixture(t, "collection-json", "collection-json")
		service := purchase.New(store.Purchases(), testTime)
		in := collectionInput(intent, "collection-json")
		if _, err := service.BindCollection(t.Context(), in); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(t.Context(), `UPDATE billing_purchase_collection_bindings SET binding='"broken"'::jsonb WHERE account_id=$1`, intent.Account); err != nil {
			t.Fatal(err)
		}
		if out, err := service.CollectionBinding(t.Context(), intent.Account, intent.Scope, in.TransactionID); err == nil || !reflect.DeepEqual(out, purchase.CollectionBinding{}) {
			t.Fatalf("malformed JSON read=%+v err=%v", out, err)
		}
	})

	t.Run("scalar mismatch", func(t *testing.T) {
		store, db, intent := purchaseLifecycleFixture(t, "collection-scalar", "collection-scalar")
		service := purchase.New(store.Purchases(), testTime)
		in := collectionInput(intent, "collection-scalar")
		if _, err := service.BindCollection(t.Context(), in); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(t.Context(), `UPDATE billing_purchase_collection_bindings SET quote_fingerprint=$2 WHERE account_id=$1`, intent.Account, "changed-fingerprint"); err != nil {
			t.Fatal(err)
		}
		if out, err := service.CollectionBinding(t.Context(), intent.Account, intent.Scope, in.TransactionID); !errors.Is(err, billing.ErrConflict) || !reflect.DeepEqual(out, purchase.CollectionBinding{}) {
			t.Fatalf("scalar mismatch read=%+v err=%v", out, err)
		}
	})

	t.Run("oversize JSON", func(t *testing.T) {
		store, db, intent := purchaseLifecycleFixture(t, "collection-oversize", "collection-oversize")
		service := purchase.New(store.Purchases(), testTime)
		in := collectionInput(intent, "collection-oversize")
		if _, err := service.BindCollection(t.Context(), in); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(t.Context(), `ALTER TABLE billing_purchase_collection_bindings DROP CONSTRAINT billing_purchase_collection_binding_size`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(t.Context(), `UPDATE billing_purchase_collection_bindings SET binding=jsonb_build_object('padding',repeat('x',1048577)) WHERE account_id=$1`, intent.Account); err != nil {
			t.Fatal(err)
		}
		if out, err := service.CollectionBinding(t.Context(), intent.Account, intent.Scope, in.TransactionID); !errors.Is(err, billing.ErrInvalid) || !reflect.DeepEqual(out, purchase.CollectionBinding{}) {
			t.Fatalf("oversize JSON read=%+v err=%v", out, err)
		}
	})
}

func TestPostgresCollectionBindingPropagatesDatabaseFailure(t *testing.T) {
	store, db, intent := purchaseLifecycleFixture(t, "collection-db-error", "collection-db-error")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := purchase.New(store.Purchases(), testTime).CollectionBinding(t.Context(), intent.Account, intent.Scope, "missing")
	if err == nil || errors.Is(err, billing.ErrNotFound) || !reflect.DeepEqual(out, purchase.CollectionBinding{}) {
		t.Fatalf("database failure read=%+v err=%v", out, err)
	}
}

func TestPostgresCollectionBindingDeferredCommitFailureRollsBack(t *testing.T) {
	store, db, intent := purchaseLifecycleFixture(t, "collection-commit", "collection-commit")
	ctx := t.Context()
	if _, err := db.ExecContext(ctx, `CREATE FUNCTION fail_collection_binding_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'collection binding commit failure'; END $$; CREATE CONSTRAINT TRIGGER fail_collection_binding_commit AFTER INSERT ON billing_purchase_collection_bindings DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION fail_collection_binding_commit()`); err != nil {
		t.Fatal(err)
	}
	service := purchase.New(store.Purchases(), testTime)
	in := collectionInput(intent, "collection-commit")
	if out, err := service.BindCollection(ctx, in); err == nil || !reflect.DeepEqual(out, purchase.CollectionBinding{}) {
		t.Fatalf("failed commit=%+v err=%v", out, err)
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM billing_purchase_collection_bindings WHERE account_id=$1`, intent.Account).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rollback count=%d err=%v", count, err)
	}
}

func TestPostgresCollectionBindingLegacyPaidTransactionFence(t *testing.T) {
	store, db := testStore(t)
	account := billing.AccountID("collection-legacy")
	seedLegacyPaymentRows(t, store, db, account)
	service := purchase.New(store.Purchases(), testTime)
	intent := seedLifecycleIntent(t, service, account, "collection-legacy-new", providerScope("stripe", "merchant", "sandbox"), testTime())
	in := collectionInput(intent, "legacy-transaction")
	in.Lines[0].Allocations[0].QuoteLineID = "line-collection-legacy-new"
	if out, err := service.BindCollection(t.Context(), in); !errors.Is(err, billing.ErrConflict) || !reflect.DeepEqual(out, purchase.CollectionBinding{}) {
		t.Fatalf("legacy fence=%+v err=%v", out, err)
	}
	var count int
	if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM billing_purchase_collection_bindings`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("legacy fence count=%d err=%v", count, err)
	}
}

func providerScope(providerName, merchant, environment string) billing.Scope {
	return billing.Scope{Provider: providerName, Merchant: merchant, Environment: environment}
}

// Re-keying replaces the provider line ids in place: the row keeps its
// intent, quote fingerprint and creation time, the stored fingerprint follows
// the new content, and a stale fingerprint (someone else re-keyed first) is a
// conflict rather than a silent overwrite.
func TestPostgresRekeyCollectionLinesReplacesIDsInPlace(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("collection-rekey")
	if err := store.CreateAccount(ctx, account, string(account)); err != nil {
		t.Fatal(err)
	}
	scope := billing.Scope{Provider: "test", Merchant: "merchant", Environment: "sandbox"}
	intent := mergedCollectionIntent(t, store, account, "rekey", scope)
	service := purchase.New(store.Purchases(), testTime)
	input := mergedCollectionInput(intent, "rekey", "rekey-transaction")
	bound, err := service.BindCollection(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	renamed := []purchase.CollectionLine{{ProviderLineID: "provider-line-rekey-2", ProviderPriceID: "provider-price", Quantity: 5, Allocations: []purchase.CollectionAllocation{
		{QuoteLineID: "merged-line-a-rekey", Quantity: 2},
		{QuoteLineID: "merged-line-b-rekey", Quantity: 3},
	}}}
	rekeyed, err := service.RekeyCollectionLines(ctx, purchase.RekeyInput{Account: account, Scope: scope, TransactionID: "rekey-transaction", Lines: renamed, Actor: "worker", Reason: "provider re-issued line ids"})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := service.CollectionBinding(ctx, account, scope, "rekey-transaction")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Lines[0].ProviderLineID != "provider-line-rekey-2" || stored.Fingerprint() != rekeyed.Fingerprint() || !stored.CreatedAt.Equal(bound.CreatedAt) || stored.IntentID != bound.IntentID {
		t.Fatalf("stored after rekey = %+v", stored)
	}
	var rows int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM billing_purchase_collection_bindings WHERE account_id=$1 AND transaction_id=$2`, account, "rekey-transaction").Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("rows=%d err=%v; a rekey must update the row, not add one", rows, err)
	}
	if _, err := service.RekeyCollectionLines(ctx, purchase.RekeyInput{Account: account, Scope: scope, TransactionID: "rekey-transaction", Lines: bound.Lines, Actor: "worker", Reason: "back"}); err != nil {
		t.Fatalf("rekeying back to the original ids: %v", err)
	}
	changed := renamed
	changed[0].Quantity = 4
	if _, err := service.RekeyCollectionLines(ctx, purchase.RekeyInput{Account: account, Scope: scope, TransactionID: "rekey-transaction", Lines: changed, Actor: "worker", Reason: "x"}); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("changed quantity: %v", err)
	}
}
