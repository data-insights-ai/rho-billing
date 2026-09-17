package pg

import (
	"encoding/json"
	"errors"
	"testing"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/purchase"
)

func TestPostgresCollectionBindingReadsLegacyWithoutRewritingHistory(t *testing.T) {
	store, db, intent := purchaseLifecycleFixture(t, "collection-old-json", "collection-old-json")
	old := legacyCollectionBinding{legacyCollectionInput: legacyCollectionInput{
		Account: intent.Account, Scope: intent.Scope, TransactionID: "collection-old-json", IntentID: intent.ID,
		QuoteFingerprint: intent.QuoteFingerprint, CustomerID: "customer",
		Lines: []legacyCollectionLine{{ProviderLineID: "provider-line", QuoteLineID: "line-collection-old-json", ProviderPriceID: "provider-price", Quantity: 1}},
		Actor: "operator", Reason: "verified collection", EvidenceReference: "evidence",
	}, CreatedAt: testTime()}
	raw, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := legacyCollectionFingerprint(old)
	if _, err := db.ExecContext(t.Context(), `INSERT INTO billing_purchase_collection_bindings(account_id,provider,merchant,environment,transaction_id,intent_id,quote_fingerprint,binding,fingerprint) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, intent.Account, intent.Scope.Provider, intent.Scope.Merchant, intent.Scope.Environment, old.TransactionID, intent.ID, intent.QuoteFingerprint, raw, fingerprint); err != nil {
		t.Fatal(err)
	}
	var beforeRaw, beforeFingerprint string
	if err := db.QueryRowContext(t.Context(), `SELECT binding::text,fingerprint FROM billing_purchase_collection_bindings WHERE account_id=$1`, intent.Account).Scan(&beforeRaw, &beforeFingerprint); err != nil {
		t.Fatal(err)
	}
	service := purchase.New(store.Purchases(), testTime)
	read, err := service.CollectionBinding(t.Context(), intent.Account, intent.Scope, old.TransactionID)
	if err != nil || len(read.Lines) != 1 || len(read.Lines[0].Allocations) != 1 || read.Lines[0].Allocations[0].QuoteLineID != old.Lines[0].QuoteLineID || read.Lines[0].Allocations[0].Quantity != 1 {
		t.Fatalf("legacy read=%+v err=%v", read, err)
	}
	replay := collectionInput(intent, old.TransactionID)
	replay.Lines[0].ProviderPriceID = old.Lines[0].ProviderPriceID
	if out, err := service.BindCollection(t.Context(), replay); err != nil || out.Fingerprint() != read.Fingerprint() {
		t.Fatalf("legacy replay=%+v err=%v", out, err)
	}
	var afterRaw, afterFingerprint string
	if err := db.QueryRowContext(t.Context(), `SELECT binding::text,fingerprint FROM billing_purchase_collection_bindings WHERE account_id=$1`, intent.Account).Scan(&afterRaw, &afterFingerprint); err != nil {
		t.Fatal(err)
	}
	if afterRaw != beforeRaw || afterFingerprint != beforeFingerprint || afterFingerprint != fingerprint {
		t.Fatalf("legacy history changed raw=%v fingerprint=%v", afterRaw != beforeRaw, afterFingerprint)
	}
	if _, err := db.ExecContext(t.Context(), `UPDATE billing_purchase_collection_bindings SET fingerprint=$2 WHERE account_id=$1`, intent.Account, "changed-fingerprint"); err != nil {
		t.Fatal(err)
	}
	if out, err := service.CollectionBinding(t.Context(), intent.Account, intent.Scope, old.TransactionID); !errors.Is(err, billing.ErrConflict) || out.Account != "" {
		t.Fatalf("corrupt legacy read=%+v err=%v", out, err)
	}
}

func TestPostgresCollectionBindingRejectsMixedStoredShape(t *testing.T) {
	store, db, intent := purchaseLifecycleFixture(t, "collection-mixed-json", "collection-mixed-json")
	service := purchase.New(store.Purchases(), testTime)
	in := collectionInput(intent, "collection-mixed-json")
	if _, err := service.BindCollection(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `UPDATE billing_purchase_collection_bindings SET binding=jsonb_set(binding,'{Lines,0,QuoteLineID}',to_jsonb('line-collection-mixed-json'::text)) WHERE account_id=$1`, intent.Account); err != nil {
		t.Fatal(err)
	}
	if out, err := service.CollectionBinding(t.Context(), intent.Account, intent.Scope, in.TransactionID); !errors.Is(err, billing.ErrInvalid) || out.Account != "" {
		t.Fatalf("mixed shape read=%+v err=%v", out, err)
	}
}

func TestPostgresCollectionBindingRejectsInvalidLegacyWithMatchingFingerprint(t *testing.T) {
	store, db, intent := purchaseLifecycleFixture(t, "collection-invalid-old", "collection-invalid-old")
	old := legacyCollectionBinding{legacyCollectionInput: legacyCollectionInput{
		Account: intent.Account, Scope: intent.Scope, TransactionID: "collection-invalid-old", IntentID: intent.ID,
		QuoteFingerprint: intent.QuoteFingerprint, Lines: []legacyCollectionLine{{ProviderLineID: "provider-line", QuoteLineID: "line-collection-invalid-old", Quantity: 0}},
		Actor: "operator", Reason: "invalid legacy", EvidenceReference: "evidence",
	}, CreatedAt: testTime()}
	raw, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO billing_purchase_collection_bindings(account_id,provider,merchant,environment,transaction_id,intent_id,quote_fingerprint,binding,fingerprint) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, intent.Account, intent.Scope.Provider, intent.Scope.Merchant, intent.Scope.Environment, old.TransactionID, intent.ID, intent.QuoteFingerprint, raw, legacyCollectionFingerprint(old)); err != nil {
		t.Fatal(err)
	}
	out, err := purchase.New(store.Purchases(), testTime).CollectionBinding(t.Context(), intent.Account, intent.Scope, old.TransactionID)
	if !errors.Is(err, billing.ErrInvalid) || out.Account != "" {
		t.Fatalf("invalid legacy read=%+v err=%v", out, err)
	}
}
