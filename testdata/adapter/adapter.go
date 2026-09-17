// Package adapter: signature verification and provider transport belong to the
// actual adapter.
package adapter

import (
	"context"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/integration"
	"github.com/data-insights-ai/rho-billing/usage"
)

// ReceiveVerified accepts only an already verified provider event.
func ReceiveVerified(ctx context.Context, inbox interface {
	Receive(context.Context, integration.Message) error
}, account billing.AccountID, externalID string, occurred time.Time, payload []byte) error {
	id, err := integration.ProviderMessageID("example", "merchant", "sandbox", externalID)
	if err != nil {
		return err
	}
	return inbox.Receive(ctx, integration.Message{
		Account: account, ID: id, Scope: billing.Scope{Provider: "example", Merchant: "merchant", Environment: "sandbox"},
		Kind: "payment.completed", Direction: integration.Inbound,
		OccurredAt: occurred, Payload: payload,
	})
}

// The host owns transaction commit and handling any recorded Rejection.
func CompleteSettlement(ctx context.Context, session integration.Session, in usage.CompleteSubmissionInput, now func() time.Time) (usage.BatchSummary, error) {
	return usage.NewSettlement(session.Settlements(), now).CompleteSubmission(ctx, in)
}
