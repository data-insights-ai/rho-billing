package credit

import (
	"context"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
)

type RepairPhase string

const (
	RepairPhaseReplay        RepairPhase = "replay"
	RepairPhaseAllocations   RepairPhase = "allocations"
	RepairPhaseVerify        RepairPhase = "verify"
	RepairPhaseReady         RepairPhase = "ready"
	RepairPhaseApplyLots     RepairPhase = "apply_lots"
	RepairPhaseClearAccounts RepairPhase = "clear_accounts"
	RepairPhaseClearScopes   RepairPhase = "clear_scopes"
	RepairPhaseWriteAccounts RepairPhase = "write_accounts"
	RepairPhaseWriteScopes   RepairPhase = "write_scopes"
	RepairPhaseCompleted     RepairPhase = "completed"
)

type RepairRequest struct {
	Account                 billing.AccountID
	ID, Actor, Reason       string
	ExpectedJournalSequence int64
}

type RepairStatus struct {
	Account                                            billing.AccountID
	ID, Actor, Reason                                  string
	Phase                                              RepairPhase
	Revision, TargetSequence, JournalCursor            int64
	LotCursor, ReservationCursor                       string
	AllocationPosition                                 int64
	ReplayLots, VerifiedLots, ChangedLots, AppliedLots int64
	CreatedAt, UpdatedAt                               time.Time
}

type RepairQuantities struct {
	Initial, Available, Held, Consumed, Expired, Revoked, PendingRevocation int64
}

type RepairEvidence struct {
	LotID         string
	Before, After RepairQuantities
}

type RepairStore interface {
	Status(context.Context, billing.AccountID, string) (RepairStatus, error)
	Start(context.Context, RepairRequest) (RepairStatus, error)
	Apply(context.Context, billing.AccountID, string, int64) (RepairStatus, error)
	Advance(context.Context, billing.AccountID, string, int64, int) (RepairStatus, error)
	EvidencePage(context.Context, billing.AccountID, string, string, int) ([]RepairEvidence, string, bool, error)
}

// ProjectionStore Available includes future and expired lots; it is not an
// authorization balance (see LiveLots / StoredBalance / EligibleLots).
type ProjectionStore interface {
	Rebuild(context.Context, billing.AccountID, int) (bool, error)
	Stored(context.Context, billing.AccountID, string, string) (Balance, error)
}
