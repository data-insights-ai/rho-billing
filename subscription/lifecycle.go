package subscription

import (
	"strconv"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/internal/identity"
)

type ChangeKind string

const (
	ChangeActivate  ChangeKind = "activate"
	ChangeUpgrade   ChangeKind = "upgrade"
	ChangeDowngrade ChangeKind = "downgrade"
	ChangeAddOn     ChangeKind = "add_on"
	ChangeCancel    ChangeKind = "cancel"
	ChangePause     ChangeKind = "pause"
	ChangeResume    ChangeKind = "resume"
	ChangeQuantity  ChangeKind = "quantity"
	ChangeCollect   ChangeKind = "collect"
)

type ChangeState string

const (
	ChangePlanned    ChangeState = "planned"
	ChangeAccepted   ChangeState = "accepted"
	ChangeConfirmed  ChangeState = "confirmed"
	ChangeSuperseded ChangeState = "superseded"
	ChangeRejected   ChangeState = "rejected"
)

type AccessPolicy string

const (
	AccessImmediate AccessPolicy = "immediate"
	AccessPaid      AccessPolicy = "paid"
	AccessGrace     AccessPolicy = "grace"
)

type AccessState string

const (
	AccessNone     AccessState = "none"
	AccessTrial    AccessState = "trial"
	AccessActive   AccessState = "active"
	AccessInGrace  AccessState = "grace"
	AccessPaused   AccessState = "paused"
	AccessCanceled AccessState = "canceled"
)

type CollectionMode string

const (
	CollectionNone      CollectionMode = "none"
	CollectionAutomatic CollectionMode = "automatic"
	CollectionManual    CollectionMode = "manual"
)

type CollectionState string

const (
	CollectionIdle           CollectionState = "idle"
	CollectionPending        CollectionState = "pending"
	CollectionInvoiced       CollectionState = "invoiced"
	CollectionPaid           CollectionState = "paid"
	CollectionFailed         CollectionState = "failed"
	CollectionPastDue        CollectionState = "past_due"
	CollectionActionRequired CollectionState = "action_required"
)

type ProrationPolicy string

const (
	ProrationNone       ProrationPolicy = "none"
	ProrationImmediate  ProrationPolicy = "immediate"
	ProrationNextPeriod ProrationPolicy = "next_period"
)

type AllowancePolicy string

const (
	AllowanceKeepPeriod AllowancePolicy = "keep_period"
	AllowanceReissue    AllowancePolicy = "reissue"
)

type Policies struct {
	Access           AccessPolicy
	Collection       CollectionMode
	Proration        ProrationPolicy
	Allowance        AllowancePolicy
	UsageDuringGrace bool
}

type Lifecycle struct {
	Account           billing.AccountID
	ID                string
	Ref               billing.Reference
	Revision          int64
	DesiredQuantity   int64
	QuantityRevision  int64
	ConfirmedQuantity int64
	Items             []Item
	Change            ScheduledChange
	Access            AccessState
	Collection        CollectionState
	Policies          Policies
	Coverage          billing.Period
	AllowanceAnchor   time.Time
	AllowanceDay      int
	QuoteFingerprint  string
	PendingOperation  string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

type ActivateInput struct {
	Account          billing.AccountID
	ID, Operation    string
	Ref              billing.Reference
	Items            []Item
	Quantity         int64
	Trial            bool
	Policies         Policies
	Coverage         billing.Period
	QuoteFingerprint string
	Actor, Reason    string
	At               time.Time
}

type QuantityInput struct {
	Account   billing.AccountID
	ID        string
	Operation string
	Quantity  int64
	Revision  int64
	Actor     string
	Reason    string
	At        time.Time
}

type ChangeInput struct {
	Account          billing.AccountID
	ID, Operation    string
	Kind             ChangeKind
	Items            []Item
	Quantity         int64
	QuantityRevision int64
	EffectiveAt      time.Time
	Policies         Policies
	QuoteFingerprint string
	Actor, Reason    string
	At               time.Time
}

type CollectionInput struct {
	Account   billing.AccountID
	ID        string
	Operation string
	State     CollectionState
	Actor     string
	Reason    string
	At        time.Time
}

type Preview struct {
	Kind        ChangeKind
	EffectiveAt time.Time
	Policies    Policies
	Quantity    int64
}

type ChangeRecord struct {
	Account     billing.AccountID
	LifecycleID string
	Operation   string
	Kind        ChangeKind
	State       ChangeState
	Quantity    int64
	Revision    int64
	Items       []Item
	EffectiveAt time.Time
	Policies    Policies
	Collection  CollectionState
	Fingerprint string
	CreatedAt   time.Time
}

func (p Policies) valid() bool {
	switch p.Access {
	case AccessImmediate, AccessPaid, AccessGrace:
	default:
		return false
	}
	switch p.Collection {
	case CollectionNone, CollectionAutomatic, CollectionManual:
	default:
		return false
	}
	switch p.Proration {
	case ProrationNone, ProrationImmediate, ProrationNextPeriod:
	default:
		return false
	}
	switch p.Allowance {
	case AllowanceKeepPeriod, AllowanceReissue:
	default:
		return false
	}
	return true
}

func (k ChangeKind) valid() bool {
	switch k {
	case ChangeActivate, ChangeUpgrade, ChangeDowngrade, ChangeAddOn, ChangeCancel, ChangePause, ChangeResume, ChangeQuantity, ChangeCollect:
		return true
	default:
		return false
	}
}

func (s CollectionState) valid() bool {
	switch s {
	case CollectionIdle, CollectionPending, CollectionInvoiced, CollectionPaid, CollectionFailed, CollectionPastDue, CollectionActionRequired:
		return true
	default:
		return false
	}
}

func (l Lifecycle) Validate() error {
	if !billing.ValidID(string(l.Account)) || !billing.ValidID(l.ID) || l.Revision < 0 || l.DesiredQuantity < 0 || l.QuantityRevision < 0 || l.ConfirmedQuantity < 0 {
		return billing.ErrInvalid
	}
	if l.Ref.ID != "" && !l.Ref.Valid() {
		return billing.ErrInvalid
	}
	if !l.Policies.valid() {
		return billing.ErrInvalid
	}
	// Coverage is either wholly unset or a valid period. A half-set one is
	// neither, and the comparison used to short-circuit past it: with only one
	// end set, IsZero() == IsZero() is false and no check ran at all, so the
	// partial period flowed on into the allowance anchor.
	if l.Coverage.Start.IsZero() != l.Coverage.End.IsZero() {
		return billing.ErrInvalid
	}
	if !l.Coverage.Start.IsZero() && !l.Coverage.Valid() {
		return billing.ErrInvalid
	}
	if l.AllowanceDay < 0 || l.AllowanceDay > 31 {
		return billing.ErrInvalid
	}
	probe := Snapshot{Account: l.Account, Ref: l.Ref, Status: "lifecycle", Items: l.Items, Change: l.Change}
	if l.Ref.ID == "" {
		probe.Ref = billing.Reference{Scope: billing.Scope{Provider: "none", Merchant: "none", Environment: "none"}, ID: "none"}
	}
	if err := Validate(probe); err != nil {
		return err
	}
	return nil
}

func (c ChangeRecord) Validate() error {
	if !billing.ValidID(string(c.Account)) || !billing.ValidID(c.LifecycleID) || !billing.ValidID(c.Operation) || !c.Kind.valid() || c.Quantity < 0 || c.Revision < 0 || c.CreatedAt.IsZero() {
		return billing.ErrInvalid
	}
	switch c.State {
	case ChangePlanned, ChangeAccepted, ChangeConfirmed, ChangeSuperseded, ChangeRejected:
	default:
		return billing.ErrInvalid
	}
	if !c.Policies.valid() {
		return billing.ErrInvalid
	}
	return nil
}

func (c ChangeRecord) fingerprint() string {
	parts := []string{"subscription-change", string(c.Account), c.LifecycleID, c.Operation, string(c.Kind), strconv.FormatInt(c.Quantity, 10), strconv.FormatInt(c.Revision, 10), identity.Instant(c.EffectiveAt), string(c.Policies.Access), string(c.Policies.Collection), string(c.Policies.Proration), string(c.Policies.Allowance), string(c.Collection)}
	for _, item := range c.Items {
		parts = append(parts, item.ID, item.PlanVersion, strconv.FormatInt(item.Quantity, 10), identity.Instant(item.Period.Start), identity.Instant(item.Period.End))
	}
	return identity.Fingerprint(parts...)
}
