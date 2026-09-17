package purchase

import (
	"context"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
)

// AdjustmentKind: an opened or won dispute is not a chargeback; adapters must
// retain that lifecycle separately.
type AdjustmentKind string

const (
	AdjustmentRefund     AdjustmentKind = "refund"
	AdjustmentChargeback AdjustmentKind = "chargeback"
)

type CreditRefundPolicy string

const (
	CreditRefundProportional CreditRefundPolicy = "proportional"
	CreditRefundFullOnly     CreditRefundPolicy = "full_only"
)

// AdjustmentInput: chargeback admission caps outstanding exposure after verified
// recoveries. Product revocation is capped at the original benefit so overlapping
// financial losses never revoke twice. Plan and host effects use full-line
// reversal; partial refunds retain them.
type AdjustmentInput struct {
	Account                                           billing.AccountID
	ID, IntentID, ProviderAdjustmentID, TransactionID string
	Scope                                             billing.Scope
	Kind                                              AdjustmentKind
	Currency                                          string
	Lines                                             []PaidLine
	PolicyVersion                                     string
	CreditPolicy                                      CreditRefundPolicy
	Actor, Reason                                     string
	OccurredAt                                        time.Time
}

type LineAdjustmentTotal struct {
	LineID                                                     string
	RefundedGross, RefundedTax, ChargebackGross, ChargebackTax int64
}
type EffectAdjustmentTotal struct {
	EffectID string
	Target   int64
}

type AdjustmentState struct {
	Account  billing.AccountID
	IntentID string
	Revision int64
	Lines    []LineAdjustmentTotal
	Effects  []EffectAdjustmentTotal
}

// EffectAdjustment.ConsumedExposure is a snapshot, never a second usage debit or additive total.
type EffectAdjustment struct {
	EffectID                                                      string
	TargetDelta, RevokedCredits, PendingCredits, ConsumedExposure int64
	AccessRevoked                                                 bool
	HostReversalID                                                string
}
type AdjustmentResult struct {
	Account      billing.AccountID
	ID, IntentID string
	Applied      bool
	Rejection    string
	Effects      []EffectAdjustment
}
type AdjustmentRecord struct {
	Input     AdjustmentInput
	Result    AdjustmentResult
	CreatedAt time.Time
}

// Reversal: the host must atomically deduplicate this ID, undo its resource, and
// tombstone Original.ID so an older grant delivery cannot provision it afterward.
type Reversal struct {
	Account                    billing.AccountID
	ID, AdjustmentID, IntentID string
	Original                   Fulfillment
	EffectiveAt, CreatedAt     time.Time
	State                      FulfillmentState
	HostReference              string
	AppliedAt, AcknowledgedAt  time.Time
}

type AdjustmentTx interface {
	Adjustment(context.Context, string) (AdjustmentRecord, error)
	ProviderAdjustment(context.Context, billing.Scope, string) (AdjustmentRecord, error)
	InsertAdjustment(context.Context, AdjustmentRecord) error
	AdjustmentState(context.Context, string) (AdjustmentState, error)
	SaveAdjustmentState(context.Context, AdjustmentState, int64) error
	Reversal(context.Context, string) (Reversal, error)
	Reversals(context.Context, string) ([]Reversal, error)
	InsertReversal(context.Context, Reversal) error
	SaveReversal(context.Context, Reversal, string) error
}

const FulfillmentCanceled FulfillmentState = "canceled"
