package provider

// AdjustmentAction is what an adjustment does to a payment.
type AdjustmentAction string

const (
	// Refund returns money to the customer's payment method.
	Refund AdjustmentAction = "refund"
	// Credit returns money as credit to the customer's balance at the
	// provider.
	Credit AdjustmentAction = "credit"
	// Chargeback: the customer's bank took the money back.
	Chargeback AdjustmentAction = "chargeback"
	// ChargebackReverse: a chargeback was won; the money came back.
	ChargebackReverse AdjustmentAction = "chargeback_reverse"
	// ChargebackWarning: the bank warns of a likely chargeback.
	ChargebackWarning AdjustmentAction = "chargeback_warning"
	// CreditReverse takes back a credit the customer had not used.
	CreditReverse AdjustmentAction = "credit_reverse"
)

// AdjustmentStatus is where an adjustment stands.
type AdjustmentStatus string

const (
	AdjustmentPending  AdjustmentStatus = "pending_approval"
	AdjustmentApproved AdjustmentStatus = "approved"
	AdjustmentRejected AdjustmentStatus = "rejected"
	AdjustmentReversed AdjustmentStatus = "reversed"
)

// Adjustment changes a completed payment after the fact: a refund, a
// credit or a chargeback, or the reversal of one.
type Adjustment struct {
	ID string
	// Payment is the adjusted payment (its transaction).
	Payment      string
	Subscription string
	Customer     string
	Action       AdjustmentAction
	Status       AdjustmentStatus
	Currency     string
	// Total is in the currency's minor unit.
	Total int64
	Items []AdjustedLine
}

// AdjustedLine is the part of one line of the payment an adjustment
// covers.
type AdjustedLine struct {
	// Line is the payment's line (Transaction.Lines[].ID).
	Line string
	// Full says the whole line is adjusted.
	Full bool
	// Total is in the currency's minor unit.
	Total int64
}

// ReturnsMoney says the adjustment, approved, moved money back to the
// customer: what the money bought should be taken back.
func (a Adjustment) ReturnsMoney() bool {
	if a.Status != AdjustmentApproved {
		return false
	}
	switch a.Action {
	case Refund, Credit, Chargeback:
		return true
	}
	return false
}

// Reverses says the adjustment, approved, undoes an earlier one: money
// came back to the merchant.
func (a Adjustment) Reverses() bool {
	return a.Status == AdjustmentApproved && (a.Action == ChargebackReverse || a.Action == CreditReverse)
}
