package purchase

import (
	"testing"
	"time"
)

// A provider-side discount makes what was collected differ from what was
// quoted. That difference used to be indistinguishable from the provider
// charging the wrong amount, so it was refused, and a fully discounted
// purchase could not be recorded at all: the customer had an active
// subscription at the provider and nothing here.
func TestDiscountedPaymentIsFundedAndFulfilled(t *testing.T) {
	cases := []struct {
		name            string
		gross, discount int64
	}{
		{name: "half off", gross: 50, discount: 50},
		{name: "free", gross: 0, discount: 100},
		{name: "nothing off", gross: 100, discount: 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, now, scope, quote, intent := lifecycleFixture(t)
			fact := PaymentFact{
				Account: "acct", Scope: scope, EventID: "event", TransactionID: "transaction",
				IntentID: intent.ID, Status: FactPaid, Currency: quote.Currency,
				Gross: c.gross, Discount: c.discount,
				Lines:      []PaidLine{{LineID: "line", Gross: c.gross, Discount: c.discount}},
				OccurredAt: now.Add(time.Minute), CollectedAt: now.Add(time.Minute),
			}
			result, err := s.ApplyPayment(t.Context(), fact)
			if err != nil {
				t.Fatal(err)
			}
			if !result.Applied || result.Rejection != "" {
				t.Fatalf("result = %+v", result)
			}
			paid, err := s.Intent(t.Context(), "acct", intent.ID)
			if err != nil {
				t.Fatal(err)
			}
			if paid.Payment != PaymentPaid || paid.Fulfillment != FulfillmentComplete {
				t.Fatalf("intent = %+v", paid)
			}
			// The record says what actually moved, and what did not.
			funding, err := s.Funding(t.Context(), "acct", scope, fact.TransactionID)
			if err != nil {
				t.Fatal(err)
			}
			if funding.Gross != c.gross || funding.Discount != c.discount {
				t.Fatalf("funding gross %d discount %d, want %d and %d",
					funding.Gross, funding.Discount, c.gross, c.discount)
			}
		})
	}
}

// The provider owns the money: the price, the tax, the discount and the
// collection are all its, and the customer agreed to its figures on its
// checkout. What they bought is known from the intent, not from the
// amount. So a total that disagrees with our quote is applied and
// reported, never refused: refusing takes a paid customer's purchase away
// over our own bookkeeping, which is what happened in production.
func TestATotalThatDisagreesWithTheQuoteIsApplied(t *testing.T) {
	cases := []struct {
		name            string
		gross, discount int64
	}{
		{name: "collected less than quoted, no discount", gross: 60, discount: 0},
		{name: "discount does not cover the gap", gross: 20, discount: 30},
		{name: "collected and discounted more than quoted", gross: 100, discount: 50},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, now, scope, quote, intent := lifecycleFixture(t)
			fact := PaymentFact{
				Account: "acct", Scope: scope, EventID: "event", TransactionID: "transaction",
				IntentID: intent.ID, Status: FactPaid, Currency: quote.Currency,
				Gross: c.gross, Discount: c.discount,
				Lines:      []PaidLine{{LineID: "line", Gross: c.gross, Discount: c.discount}},
				OccurredAt: now.Add(time.Minute), CollectedAt: now.Add(time.Minute),
			}
			result, err := s.ApplyPayment(t.Context(), fact)
			if err != nil || !result.Applied {
				t.Fatalf("a %s was refused: %+v %v", c.name, result, err)
			}
			// The customer gets what they bought.
			paid, err := s.Intent(t.Context(), "acct", intent.ID)
			if err != nil {
				t.Fatal(err)
			}
			if paid.Payment != PaymentPaid || paid.Fulfillment != FulfillmentComplete {
				t.Fatalf("the purchase was not completed: %+v", paid)
			}

		})
	}
}

// A purchase that cost the customer nothing is still a purchase. A full
// discount and a credit that covers the whole price both settle at zero,
// and the provider decides that. Refusing one takes away a subscription
// the provider has already started.
func TestAPaymentOfNothingIsApplied(t *testing.T) {
	s, now, scope, quote, intent := lifecycleFixture(t)
	fact := PaymentFact{
		Account: "acct", Scope: scope, EventID: "event", TransactionID: "transaction",
		IntentID: intent.ID, Status: FactPaid, Currency: quote.Currency,
		Lines:      []PaidLine{{LineID: "line"}},
		OccurredAt: now.Add(time.Minute), CollectedAt: now.Add(time.Minute),
	}
	result, err := s.ApplyPayment(t.Context(), fact)
	if err != nil || !result.Applied {
		t.Fatalf("a settled purchase of nothing was refused: %+v %v", result, err)
	}
	paid, err := s.Intent(t.Context(), "acct", intent.ID)
	if err != nil || paid.Payment != PaymentPaid || paid.Fulfillment != FulfillmentComplete {
		t.Fatalf("intent = %+v, err = %v", paid, err)
	}
}

// A payment that agrees with the quote is applied like any other; the
// quote decides what the money buys, not whether it is accepted.
func TestAnAgreeingPaymentIsApplied(t *testing.T) {
	s, now, scope, quote, intent := lifecycleFixture(t)
	fact := PaymentFact{
		Account: "acct", Scope: scope, EventID: "event", TransactionID: "transaction",
		IntentID: intent.ID, Status: FactPaid, Currency: quote.Currency,
		Gross:      quote.Amount,
		Lines:      []PaidLine{{LineID: "line", Gross: quote.Amount}},
		OccurredAt: now.Add(time.Minute), CollectedAt: now.Add(time.Minute),
	}
	result, err := s.ApplyPayment(t.Context(), fact)
	if err != nil || !result.Applied {
		t.Fatalf("result %+v err %v", result, err)
	}
}

// A fact whose lines do not add up to its totals is refused whatever the
// discount says, because the totals are what the ledger records.
func TestDiscountTotalsMustMatchTheLines(t *testing.T) {
	s, now, scope, quote, intent := lifecycleFixture(t)
	fact := PaymentFact{
		Account: "acct", Scope: scope, EventID: "event", TransactionID: "transaction",
		IntentID: intent.ID, Status: FactPaid, Currency: quote.Currency,
		Gross: 0, Discount: 100,
		// The line claims a different discount than the total.
		Lines:      []PaidLine{{LineID: "line", Gross: 0, Discount: 40}},
		OccurredAt: now.Add(time.Minute), CollectedAt: now.Add(time.Minute),
	}
	if err := fact.Validate(); err == nil {
		t.Fatal("lines that do not add up were accepted")
	}
	if _, err := s.ApplyPayment(t.Context(), fact); err == nil {
		t.Fatal("lines that do not add up were applied")
	}
	if quote.Amount != 100 {
		t.Fatalf("fixture changed: quote %d", quote.Amount)
	}
}

// The intent expires with the quote, thirty minutes, and that window is
// how long the price we showed stands. It is not how long the customer
// has to finish paying. A card sent to a 3-D Secure challenge while its
// owner looks for their phone, or a bank redirect, settles later than
// that, and refusing it means the provider took the money and we gave
// them nothing.
func TestAPaymentThatSettlesAfterTheQuoteExpiresIsStillApplied(t *testing.T) {
	s, now, scope, quote, _ := lifecycleFixture(t)

	// An intent whose quote window closes almost at once, so the payment
	// below settles after it, the way a 3-D Secure challenge or a bank
	// redirect does in life.
	intent, err := s.CreateIntent(t.Context(), IntentInput{
		Account: "acct", ID: "short", Operation: "short", QuoteID: quote.ID,
		QuoteFingerprint: quote.Fingerprint(), Scope: scope, Actor: "actor",
		Reason: "purchase", ExpiresAt: now.Add(time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	settled := now.Add(time.Minute)
	if !settled.After(intent.ExpiresAt) {
		t.Fatalf("the fixture no longer settles after the intent expires")
	}

	fact := PaymentFact{
		Account: "acct", Scope: scope, EventID: "event", TransactionID: "transaction",
		IntentID: intent.ID, Status: FactPaid, Currency: quote.Currency,
		Gross:       quote.Amount,
		Lines:       []PaidLine{{LineID: "line", Gross: quote.Amount}},
		OccurredAt:  settled,
		CollectedAt: settled,
	}
	result, err := s.ApplyPayment(t.Context(), fact)
	if err != nil || !result.Applied {
		t.Fatalf("a settlement after the quote expired was refused: %+v %v", result, err)
	}
	paid, err := s.Intent(t.Context(), "acct", intent.ID)
	if err != nil || paid.Payment != PaymentPaid || paid.Fulfillment != FulfillmentComplete {
		t.Fatalf("intent = %+v, err = %v", paid, err)
	}
}

// A collection that predates the intent is not a late payment, it is a
// payment for something else, and is still refused.
func TestAPaymentCollectedBeforeTheIntentIsRefused(t *testing.T) {
	s, now, scope, quote, intent := lifecycleFixture(t)
	early := intent.CreatedAt.Add(-time.Hour)
	fact := PaymentFact{
		Account: "acct", Scope: scope, EventID: "event", TransactionID: "transaction",
		IntentID: intent.ID, Status: FactPaid, Currency: quote.Currency,
		Gross:       quote.Amount,
		Lines:       []PaidLine{{LineID: "line", Gross: quote.Amount}},
		OccurredAt:  now.Add(time.Minute),
		CollectedAt: early,
	}
	result, err := s.ApplyPayment(t.Context(), fact)
	if err == nil && result.Applied {
		t.Fatal("a collection predating the intent was accepted")
	}
	if result.Rejection != RejectCollectionTime {
		t.Fatalf("rejection = %q, want %q", result.Rejection, RejectCollectionTime)
	}
}
