package purchase

import (
	"context"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/catalog"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/usage"
)

// Fulfillment: hosts must atomically apply the effect and deduplicate ID before
// acknowledging; payment acceptance is not host delivery.
type Fulfillment struct {
	Account                   billing.AccountID
	ID, IntentID, LineID      string
	Effect                    Effect
	Quantity                  int64
	SettlementBatchID         string
	EffectiveAt, CreatedAt    time.Time
	State                     FulfillmentState
	HostReference             string
	AppliedAt, AcknowledgedAt time.Time
}

type Acknowledgment struct {
	Account                              billing.AccountID
	EffectID, Fingerprint, HostReference string
	AppliedAt                            time.Time
}

// SettlementFunding links paid funds to existing usage evidence; it never creates usage.
type SettlementFunding struct {
	Account                     billing.AccountID
	BatchID, IntentID, EffectID string
	Scope                       billing.Scope
	TransactionID, Currency     string
	Amount                      int64
	PaidAt                      time.Time
}

// FulfillmentTx writes must share the purchase transaction; never commit nested effects.
type FulfillmentTx interface {
	Credits() credit.Repository
	Entitlements() catalog.EntitlementRepository
	Settlement(context.Context, string) (usage.BatchSummary, error)
	SettlementFunding(ctx context.Context, batchID string) (SettlementFunding, error)
	InsertSettlementFunding(context.Context, SettlementFunding) error
	Fulfillment(context.Context, string) (Fulfillment, error)
	Fulfillments(context.Context, string) ([]Fulfillment, error)
	InsertFulfillment(context.Context, Fulfillment) error
	SaveFulfillment(context.Context, Fulfillment, string) error // expected immutable fingerprint; pending -> complete only
}
