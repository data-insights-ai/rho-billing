package purchase

import (
	"errors"
	"math"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
)

func TestPaymentEvidenceValidatesExactAllocationTotals(t *testing.T) {
	f := PaymentFact{Account: "account", Scope: billing.Scope{Provider: "example", Merchant: "merchant", Environment: "sandbox"}, EventID: "event", TransactionID: "tx", IntentID: "intent", Status: FactPaid, Currency: "USD", Gross: 120, Tax: 20, Lines: []PaidLine{{LineID: "one", Gross: 60, Tax: 10}, {LineID: "two", Gross: 60, Tax: 10}}, OccurredAt: time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC), CollectedAt: time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)}
	if err := f.Validate(); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		change func(*PaymentFact)
		want   error
	}{
		{"duplicate line", func(v *PaymentFact) { v.Lines[1].LineID = "one" }, billing.ErrInvalid},
		{"wrong total", func(v *PaymentFact) { v.Gross++ }, billing.ErrInvalid},
		{"tax exceeds gross", func(v *PaymentFact) { v.Lines[0].Tax = 61 }, billing.ErrInvalid},
		{"overflow", func(v *PaymentFact) { v.Gross = math.MaxInt64; v.Lines[0].Gross = math.MaxInt64 }, billing.ErrOverflow},
		{"unpaid money", func(v *PaymentFact) { v.Status = FactActionRequired }, billing.ErrInvalid},
		{"unbounded payload", func(v *PaymentFact) { v.Payload = make([]byte, (64<<10)+1) }, billing.ErrInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := clonePaymentFact(f)
			tc.change(&v)
			if err := v.Validate(); !errors.Is(err, tc.want) {
				t.Fatalf("error=%v want=%v", err, tc.want)
			}
		})
	}
	v := clonePaymentFact(f)
	v.Lines[0], v.Lines[1] = v.Lines[1], v.Lines[0]
	v.OccurredAt = v.OccurredAt.In(time.FixedZone("offset", 7200))
	if f.Fingerprint() != v.Fingerprint() {
		t.Fatal("line order/timezone changed evidence identity")
	}
	v.Lines[0].Tax++
	if f.Fingerprint() == v.Fingerprint() {
		t.Fatal("changed allocation preserved identity")
	}
}

func TestCommandAndPaymentProjectionsRemainIndependent(t *testing.T) {
	now := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)
	in := Intent{IntentInput: IntentInput{Account: "account", ID: "intent", Operation: "create", QuoteID: "quote", QuoteFingerprint: digest("quote"), Scope: billing.Scope{Provider: "example", Merchant: "merchant", Environment: "sandbox"}, Actor: "owner", Reason: "buy", ExpiresAt: now.Add(time.Hour)}, Currency: "USD", TaxTreatment: TaxExclusive, Amount: 100, Command: CommandAccepted, Payment: PaymentPending, Fulfillment: FulfillmentPending, Revision: 2, CreatedAt: now, UpdatedAt: now}
	if err := in.Validate(); err != nil {
		t.Fatal(err)
	}
	accepted := in
	accepted.Fulfillment = FulfillmentComplete
	if err := accepted.Validate(); err == nil {
		t.Fatal("accepted command permitted fulfillment without payment")
	}
	paid := in
	paid.Payment = PaymentPaid
	paid.TransactionID = "tx"
	paid.PaidAt = now
	paid.LastPaymentAt = now
	paid.LastPaymentEventID = "event"
	paid.Revision++
	if err := paid.Validate(); err != nil {
		t.Fatal(err)
	}
	if paid.Fingerprint() != in.Fingerprint() {
		t.Fatal("state transition changed approved commercial fingerprint")
	}
	paid.Amount++
	if paid.Fingerprint() == in.Fingerprint() {
		t.Fatal("commercial change preserved fingerprint")
	}
	unknown := CommandInput{Account: in.Account, IntentID: in.ID, Operation: "reconcile", ExpectedRevision: 2, State: CommandReconciled, ProviderReference: "tx", OccurredAt: now}
	if err := unknown.Validate(); err == nil {
		t.Fatal("reconciled without authoritative evidence")
	}
	unknown.EvidenceReference = "lookup-1"
	if err := unknown.Validate(); err != nil {
		t.Fatal(err)
	}
}
