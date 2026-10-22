// Package provider is what a payment provider plugin (rho-paddle, ...)
// offers a host, in provider-neutral terms. The plugin does the network
// calls; this package only names them, so a host can switch providers
// without touching its billing logic.
package provider

import (
	"context"
	"errors"
	"time"

	"github.com/data-insights-ai/rho-billing/subscription"
)

var (
	// ErrSignature: a webhook was not signed by the provider.
	ErrSignature = errors.New("provider: webhook signature does not verify")
	// ErrIgnored: a webhook kind the host does not need.
	ErrIgnored = errors.New("provider: event kind not handled")
)

// Provider is a payment provider's API.
type Provider interface {
	Checkout(ctx context.Context, r CheckoutRequest) (Checkout, error)
	// CustomerPortal links a customer to the provider's self-service pages
	// (payment methods, invoices, cancellation).
	CustomerPortal(ctx context.Context, customer string) (string, error)
	Catalog(ctx context.Context) ([]Product, error)
	// Charge bills a one-time amount on a subscription's next invoice,
	// for example usage beyond prepaid credit; the key makes it idempotent.
	Charge(ctx context.Context, r ChargeRequest) error
	// CreateCustomer creates the provider's customer for a host account,
	// so checkouts and payments are bound to it from the start.
	CreateCustomer(ctx context.Context, r CustomerRequest) (string, error)
	// GetTransaction reads a transaction the host did not start itself,
	// such as the provider's link to retry a failed renewal.
	GetTransaction(ctx context.Context, id string) (Transaction, error)
}

type CustomerRequest struct {
	Email string
	Name  string
	// Reference is the host's key for the account, kept as custom data.
	Reference string
}

// Transaction is what the provider knows of one transaction.
type Transaction struct {
	ID       string
	Status   string
	Customer string
	// Subscription is set for a subscription's transactions.
	Subscription string
	Reference    string
}

type CheckoutRequest struct {
	// Customer is the provider's customer id when the host knows one.
	Customer string
	Email    string
	Items    []subscription.Item
	// Reference comes back on the resulting subscription and payment.
	Reference  string
	SuccessURL string
}

type Checkout struct {
	ID  string
	URL string
}

type ChargeRequest struct {
	Subscription string
	PriceID      string
	Quantity     int
	Key          string
}

// Product is a sellable thing with its prices; Reference is the host's key
// for it (for example a package), set as custom data at the provider.
type Product struct {
	ID        string
	Name      string
	Reference string
	Archived  bool
	Prices    []Price
}

type Price struct {
	ID          string
	Description string
	// Amount is in the currency's minor unit.
	Amount   int64
	Currency string
	// Interval is empty for one-time prices, else day, week, month or year.
	Interval  string
	Frequency int
	Reference string
	Archived  bool
}

// Kind is what an event says happened.
type Kind string

const (
	// SubscriptionChanged carries the subscription's state after the change.
	SubscriptionChanged Kind = "subscription.changed"
	// PaymentCompleted: money was collected, for a subscription or a
	// one-time purchase.
	PaymentCompleted Kind = "payment.completed"
)

// Event is a verified, decoded provider webhook.
type Event struct {
	ID         string
	Kind       Kind
	OccurredAt time.Time
	// Subscription is set for SubscriptionChanged.
	Subscription *subscription.Subscription
	// Payment is set for PaymentCompleted.
	Payment *Payment
}

type Payment struct {
	ID           string
	Customer     string
	Subscription string
	Items        []subscription.Item
	Reference    string
	Currency     string
	// Total is in the currency's minor unit.
	Total int64
	// BilledAt is when the provider collected it.
	BilledAt time.Time
}
