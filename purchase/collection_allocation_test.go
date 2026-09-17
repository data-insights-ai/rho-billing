package purchase

import (
	"errors"
	"math"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
)

func TestCollectionBindingMergedAllocationsReplayAndIsolation(t *testing.T) {
	now := billing.CanonicalTime(time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC))
	scope := billing.Scope{Provider: "test", Merchant: "merchant", Environment: "sandbox"}
	repo := NewMemoryRepository(ReferenceAccount{Account: "merged"})
	service := New(repo, func() time.Time { return now })
	quote, intent := collectionFixture(t, service, "merged", "allocation", now, scope)
	input := CollectionInput{
		Account: "merged", Scope: scope, TransactionID: "merged-transaction", IntentID: intent.ID,
		QuoteFingerprint: quote.Fingerprint(), CustomerID: "customer",
		Lines: []CollectionLine{{
			ProviderLineID: "provider-merged", ProviderPriceID: "provider-price", Quantity: 5,
			Allocations: []CollectionAllocation{
				{QuoteLineID: quote.Lines[1].ID, Quantity: 3},
				{QuoteLineID: quote.Lines[0].ID, Quantity: 2},
			},
		}},
		Actor: "owner", Reason: "verified-checkout", EvidenceReference: "evidence",
	}
	first, err := service.BindCollection(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	input.Lines[0].Allocations[0].Quantity = 30
	if first.Lines[0].Allocations[0].QuoteLineID != quote.Lines[0].ID || first.Lines[0].Allocations[0].Quantity != 2 {
		t.Fatalf("uncanonical or aliased result: %+v", first.Lines)
	}
	replayInput := first.CollectionInput
	replayInput.Lines = collectionLines(first.Lines)
	replayInput.Lines[0].Allocations[0], replayInput.Lines[0].Allocations[1] = replayInput.Lines[0].Allocations[1], replayInput.Lines[0].Allocations[0]
	replayed, err := service.BindCollection(t.Context(), replayInput)
	if err != nil || replayed.Fingerprint() != first.Fingerprint() {
		t.Fatalf("replay=%+v error=%v", replayed, err)
	}
	replayed.Lines[0].Allocations[0].Quantity = 20
	stored, err := service.CollectionBinding(t.Context(), "merged", scope, input.TransactionID)
	if err != nil || stored.Lines[0].Allocations[0].Quantity != 2 {
		t.Fatalf("stored=%+v error=%v", stored, err)
	}
	stored.Lines[0].Allocations[0].Quantity = 40
	again, err := service.CollectionBinding(t.Context(), "merged", scope, input.TransactionID)
	if err != nil || again.Lines[0].Allocations[0].Quantity != 2 {
		t.Fatalf("aliased read=%+v error=%v", again, err)
	}
	if _, err := service.Funding(t.Context(), "merged", scope, input.TransactionID); !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("binding created funding: %v", err)
	}
}

func TestCollectionBindingAllocationCoverage(t *testing.T) {
	now := billing.CanonicalTime(time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC))
	scope := billing.Scope{Provider: "test", Merchant: "merchant", Environment: "sandbox"}
	repo := NewMemoryRepository(ReferenceAccount{Account: "coverage"})
	service := New(repo, func() time.Time { return now })
	quote, intent := collectionFixture(t, service, "coverage", "allocation", now, scope)
	base := CollectionInput{
		Account: "coverage", Scope: scope, TransactionID: "coverage-transaction", IntentID: intent.ID,
		QuoteFingerprint: quote.Fingerprint(),
		Lines: []CollectionLine{{ProviderLineID: "provider", Quantity: 5, Allocations: []CollectionAllocation{
			{QuoteLineID: quote.Lines[0].ID, Quantity: 2},
			{QuoteLineID: quote.Lines[1].ID, Quantity: 3},
		}}},
		Actor: "owner", Reason: "verified-checkout", EvidenceReference: "evidence",
	}
	tests := []struct {
		name string
		id   string
		edit func(*CollectionInput)
		want error
	}{
		{"missing", "missing", func(in *CollectionInput) {
			in.Lines[0].Quantity = 2
			in.Lines[0].Allocations = in.Lines[0].Allocations[:1]
		}, billing.ErrConflict},
		{"extra", "extra", func(in *CollectionInput) {
			in.Lines[0].Quantity++
			in.Lines[0].Allocations = append(in.Lines[0].Allocations, CollectionAllocation{QuoteLineID: "foreign-line", Quantity: 1})
		}, billing.ErrConflict},
		{"quote quantity", "quote-quantity", func(in *CollectionInput) {
			in.Lines[0].Allocations[0].Quantity--
			in.Lines[0].Allocations[1].Quantity++
		}, billing.ErrConflict},
		{"provider quantity", "provider-quantity", func(in *CollectionInput) { in.Lines[0].Quantity++ }, billing.ErrConflict},
		{"duplicate quote", "duplicate-quote", func(in *CollectionInput) {
			in.Lines[0].Allocations[1].QuoteLineID = in.Lines[0].Allocations[0].QuoteLineID
		}, billing.ErrConflict},
		{"duplicate provider", "duplicate-provider", func(in *CollectionInput) { in.Lines = append(in.Lines, collectionLines(in.Lines)[0]) }, billing.ErrConflict},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := base
			input.TransactionID += "-" + test.id
			input.Lines = collectionLines(base.Lines)
			test.edit(&input)
			out, err := service.BindCollection(t.Context(), input)
			if !errors.Is(err, test.want) || !zeroCollectionBinding(out) {
				t.Fatalf("out=%+v error=%v", out, err)
			}
		})
	}
}

func TestCollectionInputRejectsAllocationOverflow(t *testing.T) {
	input := CollectionInput{
		Account: "account", Scope: billing.Scope{Provider: "test", Merchant: "merchant", Environment: "sandbox"},
		TransactionID: "transaction", IntentID: "intent", QuoteFingerprint: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Lines: []CollectionLine{{ProviderLineID: "provider", Quantity: math.MaxInt64, Allocations: []CollectionAllocation{
			{QuoteLineID: "line-a", Quantity: math.MaxInt64},
			{QuoteLineID: "line-b", Quantity: 1},
		}}},
		Actor: "owner", Reason: "reason", EvidenceReference: "evidence",
	}
	if err := input.Validate(); !errors.Is(err, billing.ErrOverflow) {
		t.Fatalf("error=%v", err)
	}
}
