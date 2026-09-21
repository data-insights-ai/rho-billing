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

// The rule that every gap between quote and collection must be explained
// is the reason this check exists at all, so relaxing it for discounts
// must not relax it for anything else.
func TestUnexplainedShortfallIsStillRefused(t *testing.T) {
	cases := []struct {
		name            string
		gross, discount int64
	}{
		{name: "collected less than quoted, no discount", gross: 60, discount: 0},
		{name: "discount does not cover the gap", gross: 20, discount: 30},
		{name: "collected and discounted more than quoted", gross: 100, discount: 50},
		{name: "nothing collected and nothing discounted", gross: 0, discount: 0},
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
			if err == nil && result.Applied {
				t.Fatalf("a %s was accepted", c.name)
			}
			paid, intentErr := s.Intent(t.Context(), "acct", intent.ID)
			if intentErr != nil {
				t.Fatal(intentErr)
			}
			if paid.Payment == PaymentPaid || paid.Fulfillment == FulfillmentComplete {
				t.Fatalf("a refused payment still funded the intent: %+v", paid)
			}
		})
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
