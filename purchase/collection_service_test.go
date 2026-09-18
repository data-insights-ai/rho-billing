package purchase

import (
	"context"
	"errors"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
)

func zeroCollectionBinding(in CollectionBinding) bool {
	return in.Account == "" && in.Scope == (billing.Scope{}) && in.TransactionID == "" && in.IntentID == "" && in.QuoteFingerprint == "" && in.CustomerID == "" && in.Lines == nil && in.Actor == "" && in.Reason == "" && in.EvidenceReference == "" && in.CreatedAt.IsZero()
}

func collectionFixture(t *testing.T, service *Service, account billing.AccountID, suffix string, now time.Time, scope billing.Scope) (Quote, Intent) {
	t.Helper()
	offer, err := service.PublishOffer(t.Context(), Offer{Account: account, Revision: Revision{ID: "money-" + suffix, Version: 1}, Name: "Money", Effects: nil})
	if err != nil {
		t.Fatal(err)
	}
	price, err := service.PublishPrice(t.Context(), Price{Account: account, Revision: Revision{ID: "price-" + suffix, Version: 1}, Offer: offer.Revision, Currency: "USD", UnitAmount: 125, TaxTreatment: TaxInclusive})
	if err != nil {
		t.Fatal(err)
	}
	quote, err := service.CreateQuote(t.Context(), QuoteInput{Account: account, ID: "quote-" + suffix, ValidUntil: now.Add(time.Hour), Lines: []QuoteLineInput{
		{ID: "line-a-" + suffix, Price: price.Revision, Quantity: 2},
		{ID: "line-b-" + suffix, Price: price.Revision, Quantity: 3},
	}})
	if err != nil {
		t.Fatal(err)
	}
	intent, err := service.CreateIntent(t.Context(), IntentInput{Account: account, ID: "intent-" + suffix, Operation: "create-" + suffix, QuoteID: quote.ID, QuoteFingerprint: quote.Fingerprint(), Scope: scope, Actor: "owner", Reason: "purchase", ExpiresAt: quote.ValidUntil})
	if err != nil {
		t.Fatal(err)
	}
	return quote, intent
}

func TestCollectionBindingReplayIsolationAndNoEffects(t *testing.T) {
	now := billing.CanonicalTime(time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC))
	scope := billing.Scope{Provider: "test", Merchant: "merchant", Environment: "sandbox"}
	repo := NewMemoryRepository(ReferenceAccount{Account: "acct"})
	service := New(repo, func() time.Time { return now })
	quote, intent := collectionFixture(t, service, "acct", "one", now, scope)
	in := CollectionInput{Account: "acct", Scope: scope, TransactionID: "transaction", IntentID: intent.ID, QuoteFingerprint: quote.Fingerprint(), CustomerID: "customer", Lines: []CollectionLine{
		{ProviderLineID: "provider-b", ProviderPriceID: "provider-price", Quantity: 3, Allocations: []CollectionAllocation{{QuoteLineID: quote.Lines[1].ID, Quantity: 3}}},
		{ProviderLineID: "provider-a", Quantity: 2, Allocations: []CollectionAllocation{{QuoteLineID: quote.Lines[0].ID, Quantity: 2}}},
	}, Actor: "owner", Reason: "verified-checkout", EvidenceReference: "evidence"}
	first, err := service.BindCollection(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	in.Lines[0].Quantity = 99
	in.Lines[0].Allocations[0].Quantity = 99
	reordered := in
	reordered.Lines = []CollectionLine{
		{ProviderLineID: "provider-a", Quantity: 2, Allocations: []CollectionAllocation{{QuoteLineID: quote.Lines[0].ID, Quantity: 2}}},
		{ProviderLineID: "provider-b", ProviderPriceID: "provider-price", Quantity: 3, Allocations: []CollectionAllocation{{QuoteLineID: quote.Lines[1].ID, Quantity: 3}}},
	}
	replay, err := service.BindCollection(t.Context(), reordered)
	if err != nil || replay.Fingerprint() != first.Fingerprint() {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	replay.Lines[0].Quantity = 99
	replay.Lines[0].Allocations[0].Quantity = 99
	stored, err := service.CollectionBinding(t.Context(), "acct", scope, in.TransactionID)
	if err != nil || stored.Lines[0].Quantity != 2 || stored.Lines[0].Allocations[0].Quantity != 2 {
		t.Fatalf("stored=%+v err=%v", stored, err)
	}
	stored.Lines[0].Quantity = 88
	stored.Lines[0].Allocations[0].Quantity = 88
	again, err := service.CollectionBinding(t.Context(), "acct", scope, in.TransactionID)
	if err != nil || again.Lines[0].Quantity != 2 || again.Lines[0].Allocations[0].Quantity != 2 {
		t.Fatalf("aliased stored=%+v err=%v", again, err)
	}
	if _, err := service.Funding(t.Context(), "acct", scope, in.TransactionID); !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("binding created funding: %v", err)
	}
	current, err := service.Intent(t.Context(), "acct", intent.ID)
	if err != nil || current.Payment != PaymentPending || current.Fulfillment != FulfillmentPending {
		t.Fatalf("binding changed intent=%+v err=%v", current, err)
	}
}

func TestCollectionBindingRejectsChangedAssociation(t *testing.T) {
	now := billing.CanonicalTime(time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC))
	scope := billing.Scope{Provider: "test", Merchant: "merchant", Environment: "sandbox"}
	repo := NewMemoryRepository(ReferenceAccount{Account: "acct"})
	service := New(repo, func() time.Time { return now })
	quote, intent := collectionFixture(t, service, "acct", "mismatch", now, scope)
	base := CollectionInput{Account: "acct", Scope: scope, TransactionID: "transaction", IntentID: intent.ID, QuoteFingerprint: quote.Fingerprint(), CustomerID: "customer", Lines: []CollectionLine{
		{ProviderLineID: "provider-a", Quantity: 2, Allocations: []CollectionAllocation{{QuoteLineID: quote.Lines[0].ID, Quantity: 2}}},
		{ProviderLineID: "provider-b", Quantity: 3, Allocations: []CollectionAllocation{{QuoteLineID: quote.Lines[1].ID, Quantity: 3}}},
	}, Actor: "owner", Reason: "verified-checkout", EvidenceReference: "evidence"}
	if _, err := service.BindCollection(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		change func(*CollectionInput)
	}{
		{"quantity", func(in *CollectionInput) { in.Lines[0].Quantity++ }},
		{"line", func(in *CollectionInput) { in.Lines[0].Allocations[0].QuoteLineID = quote.Lines[1].ID }},
		{"customer", func(in *CollectionInput) { in.CustomerID = "other-customer" }},
		{"quote", func(in *CollectionInput) {
			in.QuoteFingerprint = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changed := base
			changed.Lines = collectionLines(base.Lines)
			test.change(&changed)
			if out, err := service.BindCollection(t.Context(), changed); !errors.Is(err, billing.ErrConflict) || !zeroCollectionBinding(out) {
				t.Fatalf("out=%+v err=%v", out, err)
			}
		})
	}
}

func TestCollectionBindingGlobalOwnershipAndRollback(t *testing.T) {
	now := billing.CanonicalTime(time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC))
	scope := billing.Scope{Provider: "test", Merchant: "merchant", Environment: "sandbox"}
	repo := NewMemoryRepository(ReferenceAccount{Account: "one"}, ReferenceAccount{Account: "two"})
	service := New(repo, func() time.Time { return now })
	quoteOne, intentOne := collectionFixture(t, service, "one", "one", now, scope)
	quoteTwo, intentTwo := collectionFixture(t, service, "two", "two", now, scope)
	input := func(account billing.AccountID, quote Quote, intent Intent, transaction string) CollectionInput {
		return CollectionInput{Account: account, Scope: scope, TransactionID: transaction, IntentID: intent.ID, QuoteFingerprint: quote.Fingerprint(), Lines: []CollectionLine{
			{ProviderLineID: "provider-a", Quantity: 2, Allocations: []CollectionAllocation{{QuoteLineID: quote.Lines[0].ID, Quantity: 2}}},
			{ProviderLineID: "provider-b", Quantity: 3, Allocations: []CollectionAllocation{{QuoteLineID: quote.Lines[1].ID, Quantity: 3}}},
		}, Actor: "owner", Reason: "verified-checkout", EvidenceReference: "evidence"}
	}
	if _, err := service.BindCollection(t.Context(), input("one", quoteOne, intentOne, "shared-transaction")); err != nil {
		t.Fatal(err)
	}
	if out, err := service.BindCollection(t.Context(), input("two", quoteTwo, intentTwo, "shared-transaction")); !errors.Is(err, billing.ErrConflict) || !zeroCollectionBinding(out) {
		t.Fatalf("cross-account out=%+v err=%v", out, err)
	}

	rollback := errors.New("rollback")
	pending := input("two", quoteTwo, intentTwo, "rolled-back")
	binding := CollectionBinding{CollectionInput: pending, CreatedAt: now}
	err := repo.WithinAccount(t.Context(), "two", func(tx Tx) error {
		if err := tx.InsertCollectionBinding(t.Context(), binding); err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatalf("rollback err=%v", err)
	}
	if out, err := service.CollectionBinding(t.Context(), "two", scope, pending.TransactionID); !errors.Is(err, billing.ErrNotFound) || !zeroCollectionBinding(out) {
		t.Fatalf("rolled-back binding=%+v err=%v", out, err)
	}
}

func TestCollectionBindingCancellationReturnsZero(t *testing.T) {
	now := billing.CanonicalTime(time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC))
	scope := billing.Scope{Provider: "test", Merchant: "merchant", Environment: "sandbox"}
	repo := NewMemoryRepository(ReferenceAccount{Account: "acct"})
	service := New(repo, func() time.Time { return now })
	quote, intent := collectionFixture(t, service, "acct", "cancel", now, scope)
	ctx := t.Context()
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	out, err := service.BindCollection(canceled, CollectionInput{Account: "acct", Scope: scope, TransactionID: "transaction", IntentID: intent.ID, QuoteFingerprint: quote.Fingerprint(), Lines: []CollectionLine{{ProviderLineID: "a", Quantity: 2, Allocations: []CollectionAllocation{{QuoteLineID: quote.Lines[0].ID, Quantity: 2}}}}, Actor: "owner", Reason: "verified", EvidenceReference: "evidence"})
	if !errors.Is(err, context.Canceled) || !zeroCollectionBinding(out) {
		t.Fatalf("out=%+v err=%v", out, err)
	}
}

// A provider may re-issue its line ids when it recomputes a transaction while
// the lines themselves stay what was bound. Re-keying accepts exactly that and
// nothing else: the same content under new ids, never a changed purchase.
func TestRekeyCollectionLinesAcceptsNewIDsForSameContent(t *testing.T) {
	now := billing.CanonicalTime(time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC))
	scope := billing.Scope{Provider: "test", Merchant: "merchant", Environment: "sandbox"}
	repo := NewMemoryRepository(ReferenceAccount{Account: "rekey"})
	service := New(repo, func() time.Time { return now })
	quote, intent := collectionFixture(t, service, "rekey", "rekey", now, scope)
	lines := []CollectionLine{
		{ProviderLineID: "provider-a", ProviderPriceID: "provider-price", Quantity: 2, Allocations: []CollectionAllocation{{QuoteLineID: quote.Lines[0].ID, Quantity: 2}}},
		{ProviderLineID: "provider-b", ProviderPriceID: "provider-price", Quantity: 3, Allocations: []CollectionAllocation{{QuoteLineID: quote.Lines[1].ID, Quantity: 3}}},
	}
	bound, err := service.BindCollection(t.Context(), CollectionInput{
		Account: "rekey", Scope: scope, TransactionID: "rekey-transaction", IntentID: intent.ID,
		QuoteFingerprint: quote.Fingerprint(), CustomerID: "customer", Lines: lines,
		Actor: "owner", Reason: "verified-checkout", EvidenceReference: "evidence",
	})
	if err != nil {
		t.Fatal(err)
	}
	rekey := func(lines []CollectionLine) (CollectionBinding, error) {
		return service.RekeyCollectionLines(t.Context(), RekeyInput{Account: "rekey", Scope: scope, TransactionID: "rekey-transaction", Lines: lines, Actor: "worker", Reason: "provider re-issued line ids"})
	}

	// Same ids: a no-op that returns the stored binding.
	same, err := rekey(lines)
	if err != nil || same.Fingerprint() != bound.Fingerprint() {
		t.Fatalf("replay with current ids: %+v, %v", same, err)
	}

	// New ids for the same content, in a different order: accepted, and what
	// is stored afterwards carries the new ids and nothing else changed.
	renamed := []CollectionLine{
		{ProviderLineID: "provider-b2", ProviderPriceID: "provider-price", Quantity: 3, Allocations: []CollectionAllocation{{QuoteLineID: quote.Lines[1].ID, Quantity: 3}}},
		{ProviderLineID: "provider-a2", ProviderPriceID: "provider-price", Quantity: 2, Allocations: []CollectionAllocation{{QuoteLineID: quote.Lines[0].ID, Quantity: 2}}},
	}
	rekeyed, err := rekey(renamed)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := service.CollectionBinding(t.Context(), "rekey", scope, "rekey-transaction")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Fingerprint() != rekeyed.Fingerprint() || len(stored.Lines) != 2 || stored.Lines[0].ProviderLineID != "provider-a2" || stored.Lines[1].ProviderLineID != "provider-b2" {
		t.Fatalf("stored after rekey = %+v", stored.Lines)
	}
	if stored.IntentID != bound.IntentID || stored.QuoteFingerprint != bound.QuoteFingerprint || stored.CustomerID != bound.CustomerID || !stored.CreatedAt.Equal(bound.CreatedAt) {
		t.Fatalf("rekey changed more than the line ids: %+v", stored.CollectionInput)
	}

	// Anything commercial that differs is a different purchase.
	for name, bad := range map[string][]CollectionLine{
		"quantity": {
			{ProviderLineID: "x1", ProviderPriceID: "provider-price", Quantity: 1, Allocations: []CollectionAllocation{{QuoteLineID: quote.Lines[0].ID, Quantity: 2}}},
			{ProviderLineID: "x2", ProviderPriceID: "provider-price", Quantity: 3, Allocations: []CollectionAllocation{{QuoteLineID: quote.Lines[1].ID, Quantity: 3}}},
		},
		"price": {
			{ProviderLineID: "x1", ProviderPriceID: "other-price", Quantity: 2, Allocations: []CollectionAllocation{{QuoteLineID: quote.Lines[0].ID, Quantity: 2}}},
			{ProviderLineID: "x2", ProviderPriceID: "provider-price", Quantity: 3, Allocations: []CollectionAllocation{{QuoteLineID: quote.Lines[1].ID, Quantity: 3}}},
		},
		"allocation": {
			{ProviderLineID: "x1", ProviderPriceID: "provider-price", Quantity: 2, Allocations: []CollectionAllocation{{QuoteLineID: quote.Lines[1].ID, Quantity: 2}}},
			{ProviderLineID: "x2", ProviderPriceID: "provider-price", Quantity: 3, Allocations: []CollectionAllocation{{QuoteLineID: quote.Lines[0].ID, Quantity: 3}}},
		},
		"fewer lines": {
			{ProviderLineID: "x1", ProviderPriceID: "provider-price", Quantity: 2, Allocations: []CollectionAllocation{{QuoteLineID: quote.Lines[0].ID, Quantity: 2}}},
		},
	} {
		if _, err := rekey(bad); !errors.Is(err, billing.ErrConflict) {
			t.Errorf("%s changed: got %v, want ErrConflict", name, err)
		}
	}
	if _, err := rekey([]CollectionLine{{ProviderLineID: "dup", ProviderPriceID: "provider-price", Quantity: 2}, {ProviderLineID: "dup", ProviderPriceID: "provider-price", Quantity: 3}}); !errors.Is(err, billing.ErrInvalid) {
		t.Errorf("duplicate ids: got %v, want ErrInvalid", err)
	}
	if _, err := service.RekeyCollectionLines(t.Context(), RekeyInput{Account: "rekey", Scope: scope, TransactionID: "unknown-transaction", Lines: renamed, Actor: "worker", Reason: "r"}); !errors.Is(err, billing.ErrNotFound) {
		t.Errorf("unknown transaction: got %v, want ErrNotFound", err)
	}
	after, err := service.CollectionBinding(t.Context(), "rekey", scope, "rekey-transaction")
	if err != nil || after.Fingerprint() != rekeyed.Fingerprint() {
		t.Fatalf("rejected rekeys must leave the binding untouched: %+v, %v", after, err)
	}
}
