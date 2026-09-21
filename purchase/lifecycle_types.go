package purchase

import (
	"context"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
)

type CommandState string

const (
	CommandPlanned    CommandState = "planned"
	CommandDispatched CommandState = "dispatched"
	CommandAccepted   CommandState = "accepted"
	CommandRejected   CommandState = "rejected"
	CommandUnknown    CommandState = "unknown"
	CommandReconciled CommandState = "reconciled"
)

// PaymentState never equates checkout completion or invoice issuance with payment.
type PaymentState string

const (
	PaymentPending        PaymentState = "pending"
	PaymentActionRequired PaymentState = "action_required"
	PaymentFailed         PaymentState = "failed"
	PaymentPaid           PaymentState = "paid"
)

type FulfillmentState string

const (
	FulfillmentPending  FulfillmentState = "pending"
	FulfillmentComplete FulfillmentState = "complete"
)

// IntentInput.ExpiresAt limits collection time, not when a delayed verified event may be processed.
type IntentInput struct {
	Account                                  billing.AccountID
	ID, Operation, QuoteID, QuoteFingerprint string
	Scope                                    billing.Scope
	Actor, Reason                            string
	ExpiresAt                                time.Time
}

type Intent struct {
	IntentInput
	Currency             string
	TaxTreatment         TaxTreatment
	Amount               int64
	Command              CommandState
	Payment              PaymentState
	Fulfillment          FulfillmentState
	Revision             int64
	CreatedAt, UpdatedAt time.Time
	TransactionID        string
	PaidAt               time.Time
	LastPaymentAt        time.Time
	LastPaymentEventID   string
}

// CommandInput: persist Dispatched before network I/O. Resolve Unknown only via
// authoritative provider lookup with a nonempty EvidenceReference.
type CommandInput struct {
	Account                              billing.AccountID
	IntentID, Operation                  string
	ExpectedRevision                     int64
	State                                CommandState
	ProviderReference, EvidenceReference string
	OccurredAt                           time.Time
}

type CommandRecord struct {
	Input  CommandInput
	Result Intent
}

// PaidLine: for exclusive quotes Gross-Tax must equal the frozen commercial
// amount; for inclusive quotes Gross must equal it. Fees never reduce these amounts.
type PaidLine struct {
	LineID     string
	Gross, Tax int64
	// Discount is what the provider took off this line: the difference
	// between the quoted amount and what was actually collected. It exists
	// because a provider-side discount code is a legitimate reason for the
	// two to differ, and the only one. Recording it keeps the rule that
	// every gap between quote and collection must be explained, rather
	// than abandoning the rule to allow discounts.
	//
	// omitempty on purpose: a line without a discount serialises exactly as
	// it did before this field existed, so stored fingerprints still match.
	Discount int64 `json:",omitempty"`
}

// PaymentFact: Paid/completed are collection evidence; other statuses carry zero
// totals and no lines. CollectedAt is the transfer time, separate from OccurredAt.
type PaymentFact struct {
	Account                          billing.AccountID
	Scope                            billing.Scope
	EventID, TransactionID, IntentID string
	Status                           PaymentFactStatus
	Currency                         string
	Gross, Tax                       int64
	// Discount is the sum of the lines' discounts: what the provider took
	// off the quoted price. Gross is what was actually collected, so a
	// fully discounted purchase has Gross 0 and Discount equal to the
	// quote.
	Discount                int64 `json:",omitempty"`
	Lines                   []PaidLine
	OccurredAt, CollectedAt time.Time
	Payload                 []byte
}
type PaymentFactStatus string

const (
	FactPending        PaymentFactStatus = "pending"
	FactActionRequired PaymentFactStatus = "action_required"
	FactFailed         PaymentFactStatus = "failed"
	FactPaid           PaymentFactStatus = "paid"
	FactCompleted      PaymentFactStatus = "completed"
)

// PaymentResult.Applied means this observation was accepted; it never means a
// new transfer or product effect was executed.
type PaymentResult struct {
	Account                          billing.AccountID
	IntentID, EventID, TransactionID string
	Applied                          bool
	Rejection                        string
	// Discrepancy says the provider's figures differ from the quote they
	// were made against. The payment is still applied: the provider is the
	// authority on money, and what the customer bought is known from the
	// intent rather than from the amount. It means our catalog has drifted
	// from theirs, which somebody should look at and no customer should
	// suffer for.
	Discrepancy string `json:",omitempty"`
}

const (
	RejectScope             = "scope_mismatch"
	RejectCurrency          = "currency_mismatch"
	RejectAllocation        = "allocation_mismatch"
	RejectCollectionTime    = "collection_time_outside_intent"
	RejectAlreadyFunded     = "intent_already_funded"
	RejectTransactionOwner  = "transaction_owned_by_another_intent"
	RejectStaleObservation  = "stale_observation"
	RejectEqualTimeConflict = "equal_time_conflict"
)

type PaymentRecord struct {
	Fact   PaymentFact
	Result PaymentResult
}

type Funding struct {
	Account                           billing.AccountID
	Scope                             billing.Scope
	TransactionID, IntentID, Currency string
	Gross, Tax                        int64
	// Discount is what the provider took off the quoted price. Gross is
	// what was collected, so the two together are what was quoted.
	Discount int64 `json:",omitempty"`
	PaidAt   time.Time
	Lines    []PaidLine
}

type LifecycleTx interface {
	Intent(context.Context, string) (Intent, error)
	IntentByOperation(context.Context, string) (Intent, error)
	InsertIntent(context.Context, Intent) error
	SaveIntent(context.Context, Intent, int64) error
	Command(context.Context, string) (CommandRecord, error)
	InsertCommand(context.Context, CommandRecord) error
	PaymentEvent(context.Context, billing.Scope, string) (PaymentRecord, error)
	InsertPaymentEvent(context.Context, PaymentRecord) error
	Funding(context.Context, billing.Scope, string) (Funding, error)
	InsertFunding(context.Context, Funding) error
}
