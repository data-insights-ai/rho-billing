package purchase

import (
	"errors"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
)

func TestServicePublishesImmutableTermsAndBuildsQuote(t *testing.T) {
	now := billing.CanonicalTime(time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC))
	s := New(NewMemoryRepository(ReferenceAccount{Account: "acct"}), func() time.Time { return now })
	offer := Offer{Account: "acct", Revision: Revision{ID: "topup", Version: 1}, Name: "Top up", Effects: []Effect{{Key: "credit", Credit: &CreditBenefit{Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 10}}}}
	got, err := s.PublishOffer(t.Context(), offer)
	if err != nil {
		t.Fatal(err)
	}
	if !got.PublishedAt.Equal(now) {
		t.Fatalf("published at %v, want %v", got.PublishedAt, now)
	}
	offer.Effects[0].Credit.Amount = 999
	stored, err := s.Offer(t.Context(), "acct", offer.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Effects[0].Credit.Amount != 10 {
		t.Fatalf("stored offer mutated: %+v", stored)
	}
	price, err := s.PublishPrice(t.Context(), Price{Account: "acct", Revision: Revision{ID: "topup-price", Version: 1}, Offer: offer.Revision, Currency: "EUR", UnitAmount: 250, TaxTreatment: TaxExclusive})
	if err != nil {
		t.Fatal(err)
	}
	q, err := s.CreateQuote(t.Context(), QuoteInput{Account: "acct", ID: "quote-1", ValidUntil: now.Add(time.Hour), Lines: []QuoteLineInput{{ID: "line", Price: price.Revision, Quantity: 2}}})
	if err != nil {
		t.Fatal(err)
	}
	if q.Amount != 500 || q.Currency != "EUR" || len(q.Lines) != 1 {
		t.Fatalf("quote=%+v", q)
	}
	q.Lines[0].Offer.Effects[0].Credit.Amount = 88
	again, err := s.Quote(t.Context(), "acct", q.ID)
	if err != nil {
		t.Fatal(err)
	}
	if again.Lines[0].Offer.Effects[0].Credit.Amount != 10 {
		t.Fatalf("quote snapshot mutated: %+v", again)
	}
}

func TestServiceQuoteReplayAndConflicts(t *testing.T) {
	now := billing.CanonicalTime(time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC))
	s := New(NewMemoryRepository(ReferenceAccount{Account: "acct"}), func() time.Time { return now })
	offer := Offer{Account: "acct", Revision: Revision{ID: "offer", Version: 1}, Name: "Welcome", Effects: []Effect{{Key: "host", Host: &HostBenefit{Kind: "welcome", Payload: []byte(`{"x":1}`)}}}}
	if _, err := s.PublishOffer(t.Context(), offer); err != nil {
		t.Fatal(err)
	}
	price := Price{Account: "acct", Revision: Revision{ID: "price", Version: 1}, Offer: offer.Revision, Currency: "USD", UnitAmount: 1, TaxTreatment: TaxInclusive}
	if _, err := s.PublishPrice(t.Context(), price); err != nil {
		t.Fatal(err)
	}
	in := QuoteInput{Account: "acct", ID: "replay", ValidUntil: now.Add(time.Minute), Lines: []QuoteLineInput{{ID: "line", Price: price.Revision, Quantity: 1}}}
	first, err := s.CreateQuote(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := s.CreateQuote(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	if first.Fingerprint() != replay.Fingerprint() {
		t.Fatal("exact replay changed evidence")
	}
	in.Lines[0].Quantity = 2
	if _, err := s.CreateQuote(t.Context(), in); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("changed replay error=%v", err)
	}
	if _, err := s.CreateQuote(t.Context(), QuoteInput{Account: "acct", ID: "expired-new", ValidUntil: now, Lines: []QuoteLineInput{{ID: "line", Price: price.Revision, Quantity: 1}}}); !errors.Is(err, billing.ErrInvalid) {
		t.Fatalf("expired quote error=%v", err)
	}
}

func TestServiceRejectsInvalidTerms(t *testing.T) {
	if err := (Offer{Account: "acct", Revision: Revision{ID: "o", Version: 1}, Name: "Offer", Effects: []Effect{{Key: "dup", Settlement: &SettlementBenefit{}}, {Key: "dup", Host: &HostBenefit{Kind: "x", Payload: []byte(`{}`)}}}}).Validate(); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("duplicate effect error=%v", err)
	}
	if err := (Price{Account: "acct", Revision: Revision{ID: "p", Version: 1}, Offer: Revision{ID: "o", Version: 1}, Currency: "eur", TaxTreatment: TaxExclusive}).Validate(); !errors.Is(err, billing.ErrInvalid) {
		t.Fatalf("currency error=%v", err)
	}
}

// A settlement effect can only be delivered against a tax-exclusive price. The
// combination used to publish cleanly and fail during fulfilment, which aborted
// the payment transaction after the money was collected: the funding record was
// discarded and every webhook redelivery failed the same way.
func TestSettlementEffectRequiresATaxExclusivePriceAtPublication(t *testing.T) {
	repo := NewMemoryRepository(ReferenceAccount{Account: "settle-price"})
	now := billing.CanonicalTime(time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC))
	service := New(repo, func() time.Time { return now })
	offer, err := service.PublishOffer(t.Context(), Offer{Account: "settle-price", Revision: Revision{ID: "settle-offer", Version: 1}, Name: "Settle", Effects: []Effect{{Key: "settlement", Settlement: &SettlementBenefit{}}}})
	if err != nil {
		t.Fatal(err)
	}
	inclusive := Price{Account: "settle-price", Revision: Revision{ID: "inclusive", Version: 1}, Offer: offer.Revision, Currency: "USD", UnitAmount: 100, TaxTreatment: TaxInclusive}
	if _, err := service.PublishPrice(t.Context(), inclusive); !errors.Is(err, billing.ErrInvalid) {
		t.Fatalf("tax-inclusive settlement price published: err=%v", err)
	}
	exclusive := inclusive
	exclusive.Revision = Revision{ID: "exclusive", Version: 1}
	exclusive.TaxTreatment = TaxExclusive
	if _, err := service.PublishPrice(t.Context(), exclusive); err != nil {
		t.Fatalf("tax-exclusive settlement price rejected: %v", err)
	}
}
