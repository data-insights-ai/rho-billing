package credit

import (
	"context"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/catalog"

	"github.com/data-insights-ai/rho-billing/subscription"
)

type AdjustmentMode uint8

const (
	AdjustmentInitial AdjustmentMode = iota + 1
	AdjustmentReject
	AdjustmentDelta
	AdjustmentProrated
)

type ScheduleState string

const (
	ScheduleActive     ScheduleState = "active"
	SchedulePaused     ScheduleState = "paused"
	ScheduleCanceled   ScheduleState = "canceled"
	ScheduleDelinquent ScheduleState = "delinquent"
)

type Schedule struct {
	Account            billing.AccountID
	ID                 string
	Subscription       billing.Reference
	Assignment         catalog.PlanAssignment
	Anchor             time.Time
	State              ScheduleState
	StateEffectiveAt   time.Time
	Revision           int64
	PreviousScheduleID string
	// AdjustmentDelta permits a positive mid-period upgrade and never silently
	// prorates a downgrade.
	Adjustment AdjustmentMode
	SourceID   string
}

// EligibilityObservation: EffectiveAt controls which allowance periods the
// observation can authorize; ObservedAt records the provider's authoritative
// observation version/time. Delayed payment evidence may catch up historical
// periods without authorizing future periods. Equal EffectiveAt and ObservedAt
// values are an ordering tie and conflicting evidence is rejected.
type EligibilityObservation struct {
	Account     billing.AccountID
	ScheduleID  string
	SourceID    string
	EffectiveAt time.Time
	ObservedAt  time.Time
	Eligibility Eligibility
}

type issueRequest struct {
	Account         billing.AccountID
	Now             time.Time
	After           string
	AfterPeriod     time.Time
	AfterDefinition string
	Limit           int
	MaxIssuances    int
	MaxPeriods      int
	Evidence        []EligibilityObservation
}

type IssuanceStatus string

const (
	IssuanceGranted IssuanceStatus = "granted"
	IssuanceCapped  IssuanceStatus = "capped"
)

type Issuance struct {
	Account           billing.AccountID
	ScheduleID        string
	SubscriptionID    string
	AssignmentID      string
	PlanVersionID     string
	DefinitionID      string
	Period            billing.Period
	GrantKey          string
	Operation         billing.OperationID
	Unit              billing.Unit
	EntitledAmount    int64
	Amount            int64
	Status            IssuanceStatus
	EligibilitySource string
	IssuedAt          time.Time
}

type issueResult struct {
	Issuances      []Issuance
	Next           string
	NextPeriod     time.Time
	NextDefinition string
	HasMore        bool
}

type Checkpoint struct {
	Account          billing.AccountID
	ID               string
	Revision         int64
	ScheduleID       string
	DefinitionID     string
	PeriodStart      time.Time
	Through          time.Time
	DirtyFrom        time.Time
	CompletedThrough time.Time
	ProcessedChange  int64
	ChangeObserved   int64
	HasMore          bool
	PassActive       bool
}

type CheckpointRequest struct {
	Account      billing.AccountID
	ID           string
	Revision     int64
	Now          time.Time
	Limit        int
	MaxIssuances int
	MaxPeriods   int
	Evidence     []EligibilityObservation
}

type CheckpointResult struct {
	Checkpoint     Checkpoint
	Issuances      []Issuance
	HasMore        bool
	NextSchedule   string
	NextPeriod     time.Time
	NextDefinition string
}

type AllowanceChange struct {
	Sequence    int64
	ScheduleID  string
	EffectiveAt time.Time
	ChangedAt   time.Time
}

// A callback error must roll back all writes, including bound credit writes.
// A transaction-bound implementation reuses the caller's transaction; it never
// commits independently.
type AllowanceRepository interface {
	WithinAccount(context.Context, billing.AccountID, func(AllowanceTx) error) error
}

// Tx values must not escape the WithinAccount callback. Missing records return
// billing.ErrNotFound; TransitionAnchor returns billing.ErrState when its
// required lineage projection has not been prepared.
type AllowanceTx interface {
	Plan(context.Context, string) (catalog.PlanVersion, error)
	Subscription(context.Context, billing.Reference) (subscription.Snapshot, error)
	Schedule(context.Context, string) (Schedule, error)
	TransitionAnchor(context.Context, string) (time.Time, error)
	SaveSchedule(context.Context, Schedule, int64, time.Time) error
	// EligibilityAtVersion reads an exact effective/observed timestamp pair,
	// not the newest observation.
	EligibilityBySource(context.Context, string, string) (EligibilityObservation, error)
	EligibilityAtVersion(context.Context, string, time.Time, time.Time) (EligibilityObservation, error)
	InsertEligibility(context.Context, EligibilityObservation) error
	Schedules(context.Context, string, bool, int) ([]Schedule, error)
	SuccessorAt(context.Context, string) (time.Time, error)
	Lineage(context.Context, string) (Lineage, error)
	PeriodFacts(context.Context, string, []billing.Period) ([]PeriodFact, error)
	Issuance(context.Context, string, string, time.Time) (Issuance, error)
	HighWater(context.Context, string, string, time.Time) (int64, error)
	// SaveIssuance inserts immutable audit identity and raises its lineage's
	// high-water projection monotonically in the same transaction as Credits.
	SaveIssuance(context.Context, Issuance, string) error
	Checkpoint(context.Context, string) (Checkpoint, error)
	SaveCheckpoint(context.Context, Checkpoint, int64) error
	AllowanceChanges(context.Context, int64, int) ([]AllowanceChange, error)
	AllowanceChangeWatermark(context.Context) (int64, error)
	// Credits must bind to this transaction and account, with no nested commit.
	Credits() Repository
}

func ValidateSchedule(s Schedule) error {
	if !billing.ValidID(string(s.Account)) || !billing.ValidID(s.ID) ||
		!s.Subscription.Valid() || !billing.ValidID(s.Assignment.PlanVersionID) ||
		s.Assignment.ID == "" || !billing.ValidID(s.Assignment.ID) ||
		s.Assignment.Quantity <= 0 ||
		!s.Assignment.Effective.Valid() || s.Assignment.Perpetual || !billing.ValidID(s.SourceID) {
		return billing.ErrInvalid
	}
	if s.State != ScheduleActive && s.State != SchedulePaused &&
		s.State != ScheduleCanceled && s.State != ScheduleDelinquent {
		return billing.ErrInvalid
	}
	if s.StateEffectiveAt.IsZero() {
		return billing.ErrInvalid
	}
	if !s.Anchor.IsZero() && s.Anchor.After(s.Assignment.Effective.End) {
		return billing.ErrInvalid
	}
	if s.PreviousScheduleID != "" && !billing.ValidID(s.PreviousScheduleID) {
		return billing.ErrInvalid
	}
	if s.PreviousScheduleID == "" && s.Adjustment != 0 && s.Adjustment != AdjustmentInitial {
		return billing.ErrInvalid
	}
	if s.Adjustment != 0 && s.Adjustment != AdjustmentInitial && s.Adjustment != AdjustmentReject && s.Adjustment != AdjustmentDelta && s.Adjustment != AdjustmentProrated {
		return billing.ErrInvalid
	}
	return nil
}

func (o EligibilityObservation) Validate() error {
	if !billing.ValidID(string(o.Account)) || !billing.ValidID(o.ScheduleID) ||
		!billing.ValidID(o.SourceID) || o.EffectiveAt.IsZero() || o.ObservedAt.IsZero() {
		return billing.ErrInvalid
	}
	if err := o.Eligibility.Covers(billing.Period{Start: o.EffectiveAt, End: o.EffectiveAt.Add(time.Microsecond)}); err != nil && err != ErrIneligible {
		// Covers validates status and evidence shape. A short probe can be
		// ineligible while the observation is still valid evidence.
		return err
	}
	for _, evidence := range o.Eligibility.Evidence {
		if err := evidence.validate(); err != nil {
			return err
		}
	}
	return nil
}

func (r issueRequest) Validate() error {
	if !billing.ValidID(string(r.Account)) || r.Now.IsZero() || r.Limit < 1 || r.Limit > 1000 ||
		(r.After != "" && !billing.ValidID(r.After)) ||
		(!r.AfterPeriod.IsZero() && (r.After == "" || r.AfterDefinition == "")) ||
		(r.AfterDefinition != "" && (!billing.ValidID(r.AfterDefinition) || r.After == "" || r.AfterPeriod.IsZero())) ||
		r.MaxIssuances < 0 || r.MaxIssuances > 10000 || r.MaxPeriods < 0 || r.MaxPeriods > 10000 {
		return billing.ErrInvalid
	}
	for _, evidence := range r.Evidence {
		if evidence.Account != r.Account {
			return billing.ErrNotFound
		}
		if err := evidence.Validate(); err != nil {
			return err
		}
	}
	return nil
}

type Lineage struct {
	RootAssignment               string
	RootAnchor, TransitionAnchor time.Time
}

type PeriodFact struct {
	State       Schedule
	Eligibility Eligibility
	SourceID    string
}
