package purchase

import (
	"bytes"
	"cmp"
	"encoding/hex"
	"encoding/json"
	"math"
	"slices"
	"strings"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/internal/checked"
)

func validDigest(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == 32 && s == strings.ToLower(s)
}
func validCurrency(s string) bool {
	return len(s) == 3 && strings.IndexFunc(s, func(r rune) bool { return r < 'A' || r > 'Z' }) < 0
}

func (in IntentInput) Validate() error {
	if !billing.ValidID(string(in.Account)) || !billing.ValidID(in.ID) || !billing.ValidID(in.Operation) || !billing.ValidID(in.QuoteID) || !validDigest(in.QuoteFingerprint) || !in.Scope.Valid() || !billing.ValidID(in.Actor) || strings.TrimSpace(in.Reason) == "" || strings.IndexByte(in.Reason, 0) >= 0 || len(in.Reason) > 2000 || in.ExpiresAt.IsZero() {
		return billing.ErrInvalid
	}
	return nil
}

func (in IntentInput) Fingerprint() string {
	in.ExpiresAt = billing.CanonicalTime(in.ExpiresAt)
	return digest(in)
}

func (in Intent) Validate() error {
	if err := in.IntentInput.Validate(); err != nil {
		return err
	}
	if !validCurrency(in.Currency) || (in.TaxTreatment != TaxInclusive && in.TaxTreatment != TaxExclusive) || in.Amount <= 0 || in.Revision < 1 || in.CreatedAt.IsZero() || in.UpdatedAt.Before(in.CreatedAt) || !in.ExpiresAt.After(in.CreatedAt) {
		return billing.ErrInvalid
	}
	switch in.Command {
	case CommandPlanned, CommandDispatched, CommandAccepted, CommandRejected, CommandUnknown, CommandReconciled:
	default:
		return billing.ErrInvalid
	}
	switch in.Payment {
	case PaymentPending, PaymentActionRequired, PaymentFailed, PaymentPaid:
	default:
		return billing.ErrInvalid
	}
	if in.Fulfillment != FulfillmentPending && in.Fulfillment != FulfillmentComplete {
		return billing.ErrInvalid
	}
	if in.Payment == PaymentPaid {
		if !billing.ValidID(in.TransactionID) || in.PaidAt.Before(in.CreatedAt) || !in.PaidAt.Before(in.ExpiresAt) || in.LastPaymentAt.IsZero() || in.LastPaymentAt.Before(in.PaidAt) || !billing.ValidID(in.LastPaymentEventID) {
			return billing.ErrInvalid
		}
	} else if in.TransactionID != "" || !in.PaidAt.IsZero() || in.Fulfillment != FulfillmentPending {
		return billing.ErrInvalid
	}
	if in.LastPaymentAt.IsZero() != (in.LastPaymentEventID == "") {
		return billing.ErrInvalid
	}
	if !in.LastPaymentAt.IsZero() && (in.LastPaymentAt.Before(in.CreatedAt) || !billing.ValidID(in.LastPaymentEventID)) {
		return billing.ErrInvalid
	}
	if _, err := json.Marshal(in); err != nil {
		return billing.ErrInvalid
	}
	return nil
}

// Fingerprint excludes mutable states and receipt times.
func (in Intent) Fingerprint() string {
	input := in.IntentInput
	input.ExpiresAt = billing.CanonicalTime(input.ExpiresAt)
	return digest(struct {
		Input    IntentInput
		Currency string
		Tax      TaxTreatment
		Amount   int64
	}{input, in.Currency, in.TaxTreatment, in.Amount})
}

func (in CommandInput) Validate() error {
	if !billing.ValidID(string(in.Account)) || !billing.ValidID(in.IntentID) || !billing.ValidID(in.Operation) || in.ExpectedRevision < 1 || in.ExpectedRevision == math.MaxInt64 || in.OccurredAt.IsZero() {
		return billing.ErrInvalid
	}
	if _, err := json.Marshal(in); err != nil {
		return billing.ErrInvalid
	}
	switch in.State {
	case CommandDispatched, CommandAccepted, CommandRejected, CommandUnknown, CommandReconciled:
	default:
		return billing.ErrInvalid
	}
	for _, ref := range []string{in.ProviderReference, in.EvidenceReference} {
		if ref != "" && !billing.ValidID(ref) {
			return billing.ErrInvalid
		}
	}
	if (in.State == CommandAccepted || in.State == CommandReconciled) && in.ProviderReference == "" {
		return billing.ErrInvalid
	}
	if in.State == CommandReconciled && in.EvidenceReference == "" {
		return billing.ErrInvalid
	}
	return nil
}
func (in CommandInput) Fingerprint() string {
	in.OccurredAt = billing.CanonicalTime(in.OccurredAt)
	return digest(in)
}
func (r CommandRecord) Validate() error {
	if err := r.Input.Validate(); err != nil {
		return err
	}
	if err := r.Result.Validate(); err != nil {
		return err
	}
	if r.Input.Account != r.Result.Account || r.Input.IntentID != r.Result.ID || r.Input.State != r.Result.Command || r.Result.Revision != r.Input.ExpectedRevision+1 {
		return billing.ErrInvalid
	}
	return nil
}
func (r CommandRecord) Fingerprint() string {
	r.Input.OccurredAt = billing.CanonicalTime(r.Input.OccurredAt)
	r.Result = normalizeIntent(r.Result)
	return digest(r)
}

func (f PaymentFact) Validate() error {
	if !billing.ValidID(string(f.Account)) || !f.Scope.Valid() || !billing.ValidID(f.EventID) || !billing.ValidID(f.TransactionID) || !billing.ValidID(f.IntentID) || !validCurrency(f.Currency) || f.OccurredAt.IsZero() || len(f.Payload) > 64<<10 {
		return billing.ErrInvalid
	}
	if _, err := json.Marshal(f); err != nil {
		return billing.ErrInvalid
	}
	switch f.Status {
	case FactPaid, FactCompleted:
		if f.CollectedAt.IsZero() || f.CollectedAt.After(f.OccurredAt) {
			return billing.ErrInvalid
		}
		return validatePaidLines(f.Lines, f.Gross, f.Tax, f.Discount)
	case FactPending, FactActionRequired, FactFailed:
		if f.Gross != 0 || f.Tax != 0 || f.Discount != 0 || len(f.Lines) != 0 || !f.CollectedAt.IsZero() {
			return billing.ErrInvalid
		}
	default:
		return billing.ErrInvalid
	}
	return nil
}
func (f PaymentFact) Paid() bool          { return f.Status == FactPaid || f.Status == FactCompleted }
func (f PaymentFact) Fingerprint() string { return digest(normalizePaymentFact(f)) }

// validatePaidLines checks a collection against itself: the lines must add
// up to the totals, and no amount may be negative.
//
// It says nothing about whether the totals are the ones we expected. The
// provider decides what was collected, including collecting nothing when
// a discount or a credit covers the whole price, and a purchase is not
// invalid for costing the customer nothing.
func validatePaidLines(lines []PaidLine, gross, tax, discount int64) error {
	if gross < 0 || tax < 0 || tax > gross || discount < 0 || len(lines) < 1 || len(lines) > maxLines {
		return billing.ErrInvalid
	}
	seen := make(map[string]bool, len(lines))
	var g, t, d int64
	for _, line := range lines {
		if !billing.ValidID(line.LineID) || seen[line.LineID] || line.Gross < 0 || line.Tax < 0 || line.Tax > line.Gross || line.Discount < 0 {
			return billing.ErrInvalid
		}
		seen[line.LineID] = true
		var err error
		g, err = checked.Add(g, line.Gross)
		if err != nil {
			return err
		}
		t, err = checked.Add(t, line.Tax)
		if err != nil {
			return err
		}
		d, err = checked.Add(d, line.Discount)
		if err != nil {
			return err
		}
	}
	if g != gross || t != tax || d != discount {
		return billing.ErrInvalid
	}
	return nil
}
func (r PaymentRecord) Validate() error {
	if err := r.Fact.Validate(); err != nil {
		return err
	}
	if r.Result.Account != r.Fact.Account || r.Result.IntentID != r.Fact.IntentID || r.Result.EventID != r.Fact.EventID || r.Result.TransactionID != r.Fact.TransactionID {
		return billing.ErrInvalid
	}
	if r.Result.Applied {
		if r.Result.Rejection != "" {
			return billing.ErrInvalid
		}
	} else {
		switch r.Result.Rejection {
		case RejectScope, RejectCurrency, RejectAllocation, RejectCollectionTime, RejectAlreadyFunded, RejectTransactionOwner, RejectStaleObservation, RejectEqualTimeConflict:
		default:
			return billing.ErrInvalid
		}
	}
	return nil
}
func (r PaymentRecord) Fingerprint() string { r.Fact = normalizePaymentFact(r.Fact); return digest(r) }
func (f Funding) Validate() error {
	if !billing.ValidID(string(f.Account)) || !f.Scope.Valid() || !billing.ValidID(f.TransactionID) || !billing.ValidID(f.IntentID) || !validCurrency(f.Currency) || f.PaidAt.IsZero() {
		return billing.ErrInvalid
	}
	if _, err := json.Marshal(f); err != nil {
		return billing.ErrInvalid
	}
	return validatePaidLines(f.Lines, f.Gross, f.Tax, f.Discount)
}
func (f Funding) Fingerprint() string { return digest(normalizeFunding(f)) }
func clonePaymentFact(in PaymentFact) PaymentFact {
	in.Payload = bytes.Clone(in.Payload)
	in.Lines = slices.Clone(in.Lines)
	return in
}
func clonePaymentRecord(in PaymentRecord) PaymentRecord {
	in.Fact = clonePaymentFact(in.Fact)
	return in
}
func cloneFunding(in Funding) Funding { in.Lines = slices.Clone(in.Lines); return in }
func normalizeIntent(in Intent) Intent {
	in.ExpiresAt = billing.CanonicalTime(in.ExpiresAt)
	in.CreatedAt = billing.CanonicalTime(in.CreatedAt)
	in.UpdatedAt = billing.CanonicalTime(in.UpdatedAt)
	in.PaidAt = billing.CanonicalTime(in.PaidAt)
	in.LastPaymentAt = billing.CanonicalTime(in.LastPaymentAt)
	return in
}
func normalizePaymentFact(in PaymentFact) PaymentFact {
	in = clonePaymentFact(in)
	in.OccurredAt = billing.CanonicalTime(in.OccurredAt)
	in.CollectedAt = billing.CanonicalTime(in.CollectedAt)
	slices.SortFunc(in.Lines, func(a, b PaidLine) int { return cmp.Compare(a.LineID, b.LineID) })
	if len(in.Lines) == 0 {
		in.Lines = nil
	}
	if len(in.Payload) == 0 {
		in.Payload = nil
	}
	return in
}
func normalizeFunding(in Funding) Funding {
	in = cloneFunding(in)
	in.PaidAt = billing.CanonicalTime(in.PaidAt)
	slices.SortFunc(in.Lines, func(a, b PaidLine) int { return cmp.Compare(a.LineID, b.LineID) })
	return in
}
