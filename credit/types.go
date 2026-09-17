package credit

import (
	"context"
	"errors"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
)

// Rejection is a terminal business outcome already recorded by the engine.
// A transaction orchestrator should commit this outcome rather than treating it
// as a storage failure. It still matches the underlying billing error via Is.
type Rejection struct{ Cause error }

func (r *Rejection) Error() string { return r.Cause.Error() }
func (r *Rejection) Unwrap() error { return r.Cause }
func IsRejection(err error) bool   { _, ok := errors.AsType[*Rejection](err); return ok }

// Lot quantities are subunits. Initial = Available + Held + Consumed + Expired + Revoked.
type Lot struct {
	// PendingRevocation is a claim against Held, not an additional balance.
	PendingRevocation                                    int64
	ID                                                   string
	Unit                                                 billing.Unit
	Scope                                                string
	Source, SourceRef                                    string
	ValidFrom, ExpiresAt                                 time.Time // zero ExpiresAt means no expiry
	GrantedAt, RevokedAt                                 time.Time
	Initial, Available, Held, Consumed, Expired, Revoked int64
}

type Allocation struct {
	LotID  string
	Amount int64
}

type ReservationState string

const (
	ReservationHeld     ReservationState = "held"
	ReservationSettled  ReservationState = "settled"
	ReservationReleased ReservationState = "released"
	ReservationTimedOut ReservationState = "timed_out"
)

type Reservation struct {
	ID, Actor, Unit, Scope string
	CreatedAt, Deadline    time.Time
	State                  ReservationState
	Authorized, Consumed   int64
	Allocations            []Allocation
	LimitPeriod            billing.Period
	Evidence               Evidence
}

type Evidence struct {
	UsageID, RatingVersion string
	Metrics                []Metric
}
type Metric struct {
	Name     string
	Quantity int64
}

type Limit struct {
	Actor, Unit string
	Period      billing.Period
	Amount      int64
}

type Entry struct {
	// PendingRevocation is the delta in the claim against held credits.
	PendingRevocation                           int64
	Sequence                                    int64
	OperationID                                 billing.OperationID
	LotID, ReservationID, Kind, Reason          string
	RecordedAt, EffectiveAt                     time.Time
	Available, Held, Consumed, Expired, Revoked int64
}

type Balance struct{ Available, Held, Consumed, Expired, Revoked int64 }

type Result struct {
	LotID, ReservationID string
	Balance              Balance
	Consumed, Exposure   int64
}
type Outcome struct {
	Fingerprint, Error string
	Result             Result
}

type GrantInput struct {
	Account                  billing.AccountID
	Operation                billing.OperationID
	LotID                    string
	Unit                     billing.Unit
	Amount                   int64
	Scope, Source, SourceRef string
	ValidFrom, ExpiresAt     time.Time
}
type ReserveInput struct {
	Account                           billing.AccountID
	Operation                         billing.OperationID
	ReservationID, Actor, Unit, Scope string
	Amount                            int64
	Deadline                          time.Time
}
type ExtendInput struct {
	Account       billing.AccountID
	Operation     billing.OperationID
	ReservationID string
	Additional    int64
}
type SettleInput struct {
	Account       billing.AccountID
	Operation     billing.OperationID
	ReservationID string
	Actual        int64
	Evidence      Evidence
}
type ReleaseInput struct {
	Account               billing.AccountID
	Operation             billing.OperationID
	ReservationID, Reason string
}
type RevokeInput struct {
	Account       billing.AccountID
	Operation     billing.OperationID
	LotID, Reason string
}

type RevokeAmountInput struct {
	Account       billing.AccountID
	Operation     billing.OperationID
	LotID, Reason string
	Amount        int64
}

type LimitInput struct {
	Account   billing.AccountID
	Operation billing.OperationID
	Limit     Limit
}

// Repository serializes account operations and commits their writes atomically.
// The callback must never perform network calls or escape/retain Tx. Returning
// an error rolls back every write. The account must exist before use.
// Store implementations must scope every Tx method to that account.
type Repository interface {
	WithinAccount(context.Context, billing.AccountID, func(Tx) error) error
	History(context.Context, billing.AccountID, int64, int) ([]Entry, error)
}

// Tx: Append writes immutable journal rows; PutOutcome must reject replacement.
// Lots/reservations are projections. Slices and records must be independent
// copies, not mutable storage aliases.
type Tx interface {
	Lots() ([]Lot, error)
	// LiveLots excludes retired history but includes future and expired available
	// quantities and held/revocation claims. A store must reject an unready
	// balance projection before returning live state.
	LiveLots() ([]Lot, error)
	// DueLots returns at most limit lots whose available quantity has expired.
	// Results are ordered by expiry and ID; limit may not exceed 1001.
	DueLots(at time.Time, limit int) ([]Lot, error)
	// EligibleLots returns at most limit spendable lots in FEFO order (expiry,
	// grant time, ID), respecting unit, scope, validity, expiry, and revocation.
	EligibleLots(unit, scope string, at time.Time, limit int) ([]Lot, error)
	Lot(string) (Lot, bool, error)
	// LotsByID is a bounded set lookup. Callers page historical allocation IDs,
	// at most 1000 per call. Missing IDs are omitted; the engine rejects
	// incomplete reservation evidence.
	LotsByID([]string) ([]Lot, error)
	LotBySource(unit, source, reference string) (Lot, bool, error)
	// StoredBalance returns exact lifetime counters plus currently spendable
	// availability at the supplied instant. Lifetime held/consumed/expired/revoked
	// come from the account/scope projection. Spendable available is an indexed
	// aggregate over currently eligible live rows, not a lifetime or LiveLots scan.
	StoredBalance(unit, scope string, at time.Time) (Balance, error)
	EnsureUnit(billing.Unit) error
	PutLot(Lot) error
	ActiveReservations() ([]Reservation, error)
	DueReservations(at time.Time, limit int) ([]Reservation, error)
	CommittedForLimit(actor, unit string, period billing.Period, at time.Time) (int64, error)
	LimitAt(actor, unit string, at time.Time) (Limit, bool, error)
	LimitsOverlapping(actor, unit string, period billing.Period) ([]Limit, error)
	Reservations() ([]Reservation, error)
	Reservation(string) (Reservation, bool, error)
	PutReservation(Reservation) error
	HasUsage(string) (bool, error)
	SettledForLimit(actor, unit string, period billing.Period) (int64, error)
	Limits() ([]Limit, error)
	PutLimit(Limit) error
	Outcome(billing.OperationID) (Outcome, bool, error)
	PutOutcome(billing.OperationID, Outcome) error
	Append(Entry) error
	AppendEntries([]Entry) error
	History(after int64, limit int) ([]Entry, error)
}
