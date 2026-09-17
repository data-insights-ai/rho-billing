package purchase

import (
	"context"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
)

// DisputeStatus is independent of money; status alone never proves a financial debit.
type DisputeStatus string

const (
	DisputeWarning     DisputeStatus = "warning"
	DisputeOpen        DisputeStatus = "open"
	DisputeUnderReview DisputeStatus = "under_review"
	DisputeLost        DisputeStatus = "lost"
	DisputeWon         DisputeStatus = "won"
	DisputeClosed      DisputeStatus = "closed"
)

// DisputeRecovery correlation must be authoritative; the adapter cannot guess a
// chargeback from amount or transaction identity alone.
type DisputeRecovery struct {
	ID, AdjustmentID  string
	Lines             []PaidLine
	EvidenceReference string
}

type DisputeFact struct {
	Account                                               billing.AccountID
	Scope                                                 billing.Scope
	EventID, DisputeID, IntentID, TransactionID, Currency string
	Amount                                                int64
	Status                                                DisputeStatus
	OccurredAt                                            time.Time
	EvidenceReference                                     string
	DebitAdjustmentID                                     string
	Recovery                                              *DisputeRecovery
}

// Dispute.Won does not restore spent credits or previously reversed benefits;
// any compensation is a separately authorized grant with its own provenance.
type Dispute struct {
	Account                               billing.AccountID
	Scope                                 billing.Scope
	ID, IntentID, TransactionID, Currency string
	Amount                                int64
	Status                                DisputeStatus
	StatusOccurredAt                      time.Time
	StatusEventID                         string
	DebitAdjustmentID                     string
	Recovered                             []PaidLine
	Revision                              int64
	CreatedAt, UpdatedAt                  time.Time
}

type DisputeResult struct {
	Account                                 billing.AccountID
	DisputeID, EventID                      string
	Applied, StatusApplied, RecoveryApplied bool
	Rejection, IgnoredStatusReason          string
}
type DisputeRecord struct {
	Fact      DisputeFact
	Result    DisputeResult
	CreatedAt time.Time
}
type DisputeRecoveryRecord struct {
	Account                            billing.AccountID
	Scope                              billing.Scope
	DisputeID, IntentID, TransactionID string
	Recovery                           DisputeRecovery
	CreatedAt                          time.Time
}

type DisputeTx interface {
	ChargebackRecoveries(context.Context, string) ([]PaidLine, error)
	Dispute(context.Context, billing.Scope, string) (Dispute, error)
	DisputeEvent(context.Context, billing.Scope, string) (DisputeRecord, error)
	InsertDisputeEvent(context.Context, DisputeRecord) error
	SaveDispute(context.Context, Dispute, int64) error
	DisputeRecovery(context.Context, billing.Scope, string) (DisputeRecoveryRecord, error)
	InsertDisputeRecovery(context.Context, DisputeRecoveryRecord) error
}
