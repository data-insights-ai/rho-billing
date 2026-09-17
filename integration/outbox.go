package integration

import (
	"context"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/internal/identity"
)

type OutboxState string

const (
	OutboxCompleted OutboxState = "completed"
	OutboxRejected  OutboxState = "rejected"
	OutboxUnknown   OutboxState = "unknown"
)

// OutboxResult.Evidence contains bounded, secret-free correlation or diagnostics,
// never credentials or URLs. Unknown is an unresolved outcome and does not
// authorize another send.
type OutboxResult struct {
	ObservationID string
	// ExpectedPrevious fences a reconciliation against the last observation.
	// It is empty for the first observation and for dispatch completion.
	ExpectedPrevious  string
	State             OutboxState
	ProviderReference string
	Evidence          string
}

func (r OutboxResult) Validate() error {
	if !billing.ValidID(r.ObservationID) || (r.ExpectedPrevious != "" && !billing.ValidID(r.ExpectedPrevious)) || r.Evidence == "" || len(r.Evidence) > 4096 || (r.ProviderReference != "" && !billing.ValidID(r.ProviderReference)) {
		return billing.ErrInvalid
	}
	switch r.State {
	case OutboxCompleted:
		if r.ProviderReference == "" {
			return billing.ErrInvalid
		}
	case OutboxRejected, OutboxUnknown:
	default:
		return billing.ErrInvalid
	}
	return nil
}

func (r OutboxResult) Fingerprint() string {
	return identity.Fingerprint(r.ObservationID, r.ExpectedPrevious, string(r.State), r.ProviderReference, r.Evidence)
}

type OutboxRepository interface {
	Outbox(context.Context, billing.AccountID, billing.Scope, string) (Delivery, error)
	// BeginOutbox grants a single send permission only after its transaction
	// commits. Repeated begin returns false or a conflict, never another permit.
	BeginOutbox(context.Context, Claim, func(Session) error) (bool, error)
	// FinishOutbox: expired claims cannot complete; recovery must resolve the unknown.
	FinishOutbox(context.Context, Claim, OutboxResult, func(Session) error) error
	// ReleaseOutbox returns an unsent message to the queue and makes it
	// available again at the supplied time. It is valid only when the provider
	// refused the request before it could take effect, so the granted send
	// permission was not used: a rate limit or a rejected credential, never a
	// timeout or a response that could not be read. A mutation that may have
	// taken effect must go to unknown instead. Attempts are not counted, so
	// provider backpressure cannot dead-letter a pending financial operation.
	ReleaseOutbox(context.Context, Claim, string, time.Time) error
	// ResolveOutbox retains accepted, rejected, or still-unknown lookup evidence.
	// Unknown does not authorize another send.
	ResolveOutbox(context.Context, Claim, OutboxResult, func(Session) error) error
}
