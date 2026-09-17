// Package purchase keeps provider transport and application authorization outside.
package purchase

import (
	"context"
	"encoding/json"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
)

type Revision struct {
	ID      string
	Version int64
}

// TaxTreatment: an exclusive quote is not an assertion of the final
// tax-inclusive amount collected.
type TaxTreatment string

const (
	TaxInclusive TaxTreatment = "inclusive"
	TaxExclusive TaxTreatment = "exclusive"
)

// CreditBenefit: zero Validity means no expiry; a positive duration starts at
// the effective payment/assignment time, never at delayed webhook processing time.
type CreditBenefit struct {
	Unit     billing.Unit
	Amount   int64
	Scope    string
	Validity time.Duration
}

// PlanBenefit: zero Validity denotes perpetual access; it does not fabricate a
// provider subscription.
type PlanBenefit struct {
	PlanVersionID string
	Quantity      int64
	Validity      time.Duration
}

type HostBenefit struct {
	Kind    string
	Payload json.RawMessage
}

// SettlementBenefit requires a finalized batch reference on the selected quote
// line. Fulfillment links paid funds to existing usage evidence, never new usage.
type SettlementBenefit struct{}

type Effect struct {
	Key        string
	Credit     *CreditBenefit
	Plan       *PlanBenefit
	Host       *HostBenefit
	Settlement *SettlementBenefit
}

type Offer struct {
	Account     billing.AccountID
	Revision    Revision
	Name        string
	Effects     []Effect
	PublishedAt time.Time
}

type Price struct {
	Account      billing.AccountID
	Revision     Revision
	Offer        Revision
	Currency     string
	UnitAmount   int64
	TaxTreatment TaxTreatment
	PublishedAt  time.Time
}

type QuoteLineInput struct {
	ID                string
	Price             Revision
	Quantity          int64
	SettlementBatchID string
}

// QuoteInput: an expired quote may be read or exact-replayed but cannot fund a new purchase.
type QuoteInput struct {
	Account    billing.AccountID
	ID         string
	ValidUntil time.Time
	Lines      []QuoteLineInput
}

type QuoteLine struct {
	QuoteLineInput
	Offer         Offer
	PriceSnapshot Price
	Amount        int64
}

type Quote struct {
	Account               billing.AccountID
	ID                    string
	CreatedAt, ValidUntil time.Time
	Currency              string
	TaxTreatment          TaxTreatment
	Amount                int64
	Lines                 []QuoteLine
}

// Repository: root callbacks commit only on nil error. Bound implementations reuse
// the host transaction; no handles may escape the callback.
type Repository interface {
	WithinAccount(context.Context, billing.AccountID, func(Tx) error) error
}

type Tx interface {
	LifecycleTx
	FulfillmentTx
	AdjustmentTx
	DisputeTx
	CollectionTx
	TopUpTx
	Offer(context.Context, Revision) (Offer, error)
	InsertOffer(context.Context, Offer) error
	Price(context.Context, Revision) (Price, error)
	InsertPrice(context.Context, Price) error
	Quote(context.Context, string) (Quote, error)
	InsertQuote(context.Context, Quote) error
}
