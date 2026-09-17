// Package integration: adapters verify signatures before receiving inbox messages.
package integration

import (
	"context"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/catalog"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/internal/identity"

	"github.com/data-insights-ai/rho-billing/purchase"
	"github.com/data-insights-ai/rho-billing/subscription"
	"github.com/data-insights-ai/rho-billing/usage"
)

type Direction string

const (
	Inbound  Direction = "inbound"
	Outbound Direction = "outbound"
)

type Message struct {
	Account    billing.AccountID
	ID, Kind   string
	Scope      billing.Scope
	Direction  Direction
	OccurredAt time.Time
	Payload    []byte
}

func ProviderMessageID(provider, merchant, environment, externalID string) (string, error) {
	if !billing.ValidID(provider) || !billing.ValidID(merchant) || !billing.ValidID(environment) || !billing.ValidID(externalID) {
		return "", billing.ErrInvalid
	}
	return identity.Fingerprint("provider-message-v1", provider, merchant, environment, externalID), nil
}

func (m Message) Validate() error {
	if !billing.ValidID(string(m.Account)) || !billing.ValidID(m.ID) || !m.ProviderScope().Valid() || !billing.ValidID(m.Kind) || m.OccurredAt.IsZero() || len(m.Payload) > 1<<20 || (m.Direction != Inbound && m.Direction != Outbound) {
		return billing.ErrInvalid
	}
	return nil
}
func (m Message) Fingerprint() string {
	return identity.Fingerprint(string(m.Account), m.ID, m.Scope.Provider, m.Scope.Merchant, m.Scope.Environment, m.Kind, string(m.Direction), identity.Instant(m.OccurredAt), string(m.Payload))
}

type Claim struct {
	Message  Message
	Fence    int64
	Worker   string
	Deadline time.Time
	Attempt  int
}
type DeliveryState string

const (
	DeliveryPending    DeliveryState = "pending"
	DeliveryProcessing DeliveryState = "processing"
	DeliveryProcessed  DeliveryState = "processed"
	DeliveryCompleted  DeliveryState = "completed"
	DeliveryRejected   DeliveryState = "rejected"
	DeliveryUnknown    DeliveryState = "unknown"
	DeliveryDead       DeliveryState = "dead"
)

type Delivery struct {
	Message               Message
	State                 DeliveryState
	ProviderReference     string
	BegunAt               time.Time
	PayloadPruned         bool
	LastResult            *OutboxResult
	Fence                 int64
	Attempt               int
	LastError             string
	AvailableAt, Deadline time.Time
}

type Session interface {
	Credits() credit.Repository
	Purchases() purchase.Repository
	Entitlements() catalog.EntitlementRepository
	Settlements() usage.SettlementRepository
	Allowances() credit.AllowanceRepository
	Usage() usage.Repository
	Subscriptions() subscription.Repository
	Limits() credit.LimitRepository
	Enqueue(context.Context, Message) error
}
type UnitOfWork interface {
	Atomic(context.Context, billing.AccountID, func(Session) error) error
}

// Repository: expired outbound claims become unknown, never automatically
// eligible for resubmission.
type Repository interface {
	UnitOfWork
	OutboxRepository
	Receive(context.Context, Message) error
	Claim(context.Context, Direction, string, time.Time, time.Duration) (Claim, bool, error)
	ProcessInbox(context.Context, Claim, func(Session) error) error
	Fail(context.Context, Claim, string, time.Time, int) error
	Delivery(context.Context, Message) (Delivery, error)
	PruneDeliveryPayloads(context.Context, billing.AccountID, Direction, time.Time, int) (int64, error)
}

func (r Message) ProviderScope() billing.Scope { return r.Scope }
