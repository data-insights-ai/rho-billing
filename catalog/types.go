// Package catalog: a published plan is a historical fact; Registry validates and copies on publish and read.
package catalog

import (
	"time"

	"github.com/data-insights-ai/rho-billing"
)

type EntitlementKind uint8

const (
	EntitlementFeature EntitlementKind = iota + 1
	EntitlementLimit
)

type AggregationMode uint8

const (
	AggregationOR AggregationMode = iota + 1
	AggregationMAX
	AggregationSUM
)

type EntitlementDefinition struct {
	Key         string
	Kind        EntitlementKind
	Aggregation AggregationMode
	Enabled     bool
	Amount      int64
	Unit        billing.Unit
}

// AllowanceRecurrence is intentionally separate from payment periods.
type AllowanceRecurrence uint8

const (
	AllowanceOneTime AllowanceRecurrence = iota + 1
	AllowanceMonthly
	AllowanceAnnual
)

type AllowanceScope uint8

const (
	AllowanceAccount AllowanceScope = iota + 1
	AllowanceMember
)

type SpendScope string

type AllowanceDefinition struct {
	ID         string
	Unit       billing.Unit
	Amount     int64
	Recurrence AllowanceRecurrence
	// Anchor is an optional calendar anchor override. When zero, the allowance
	// issuer supplies the assignment's anchor; it is never inferred here.
	Anchor time.Time
	// Validity is an optional explicit TTL. Recurring allowances with zero
	// validity end at their calculated period end, never after a fixed 30 days.
	Validity   time.Duration
	Scope      AllowanceScope
	SpendScope SpendScope
}

// PlanVersion is immutable after publication. PlanID groups historical
// versions; ID identifies this exact version and must never be reused for new
// content.
type PlanVersion struct {
	ID           string
	PlanID       string
	Version      int64
	PublishedAt  time.Time
	Entitlements []EntitlementDefinition
	Allowances   []AllowanceDefinition
}

type AssignmentSource uint8

const (
	SourceUnspecified AssignmentSource = iota
	SourceSubscription
	SourceAddOn
	SourceMigration
	SourceManual
	SourcePurchase
	SourceFree
)

type PlanAssignment struct {
	ID            string
	PlanVersionID string
	Quantity      int64
	Effective     billing.Period
	// Perpetual explicitly permits an absent Effective.End for independently
	// granted access. Provider subscription and allowance periods remain finite.
	Perpetual bool
	Source    AssignmentSource
}

type MappingScope struct {
	Provider    string
	Account     billing.AccountID
	Environment string
	Merchant    string
}

type MappingTargetKind uint8

const (
	TargetPlanVersion MappingTargetKind = iota + 1
	TargetAddOn
)

type MappingTarget struct {
	Kind MappingTargetKind
	ID   string
}

// PriceMapping.Revision is monotonic per key.
type PriceMapping struct {
	Scope           MappingScope
	ExternalPriceID string
	Target          MappingTarget
	Revision        int64
}

func (e EntitlementKind) valid() bool { return e == EntitlementFeature || e == EntitlementLimit }
func (m AggregationMode) valid() bool {
	return m == AggregationOR || m == AggregationMAX || m == AggregationSUM
}
func (r AllowanceRecurrence) valid() bool {
	return r == AllowanceOneTime || r == AllowanceMonthly || r == AllowanceAnnual
}
func (s AllowanceScope) valid() bool    { return s == AllowanceAccount || s == AllowanceMember }
func (k MappingTargetKind) valid() bool { return k == TargetPlanVersion || k == TargetAddOn }

func (e EntitlementDefinition) validate() error {
	if !billing.ValidID(e.Key) || !e.Kind.valid() || !e.Aggregation.valid() {
		return billing.ErrInvalid
	}
	if e.Kind == EntitlementFeature {
		if e.Aggregation != AggregationOR || e.Amount != 0 || e.Unit != (billing.Unit{}) {
			return billing.ErrInvalid
		}
		return nil
	}
	if e.Aggregation == AggregationOR || e.Enabled || e.Amount < 0 || !e.Unit.Valid() {
		return billing.ErrInvalid
	}
	return nil
}

func (a AllowanceDefinition) validate() error {
	if !billing.ValidID(a.ID) || !a.Unit.Valid() || !a.Recurrence.valid() || !a.Scope.valid() || a.Amount <= 0 || !billing.ValidID(string(a.SpendScope)) {
		return billing.ErrInvalid
	}
	if a.Validity < 0 {
		return billing.ErrInvalid
	}
	return nil
}

func (p PlanVersion) validate() error {
	if !billing.ValidID(p.ID) || !billing.ValidID(p.PlanID) || p.Version <= 0 {
		return billing.ErrInvalid
	}
	seenEntitlements := make(map[string]struct{}, len(p.Entitlements))
	for _, e := range p.Entitlements {
		if err := e.validate(); err != nil {
			return err
		}
		if _, ok := seenEntitlements[e.Key]; ok {
			return billing.ErrConflict
		}
		seenEntitlements[e.Key] = struct{}{}
	}
	seenAllowances := make(map[string]struct{}, len(p.Allowances))
	for _, a := range p.Allowances {
		if err := a.validate(); err != nil {
			return err
		}
		if _, ok := seenAllowances[a.ID]; ok {
			return billing.ErrConflict
		}
		seenAllowances[a.ID] = struct{}{}
	}
	return nil
}

func (s MappingScope) validate() error {
	if !s.ProviderScope().Valid() || !billing.ValidID(string(s.Account)) {
		return billing.ErrInvalid
	}
	return nil
}

func (m PriceMapping) validate() error {
	if err := m.Scope.validate(); err != nil {
		return err
	}
	if !billing.ValidID(m.ExternalPriceID) || !m.Target.Kind.valid() || !billing.ValidID(m.Target.ID) || m.Revision <= 0 {
		return billing.ErrInvalid
	}
	return nil
}

func (r MappingScope) ProviderScope() billing.Scope {
	return billing.Scope{Provider: r.Provider, Merchant: r.Merchant, Environment: r.Environment}
}

func (a PlanAssignment) Valid() bool {
	if (a.ID != "" && !billing.ValidID(a.ID)) || !billing.ValidID(a.PlanVersionID) || a.Quantity <= 0 || a.Source > SourceFree {
		return false
	}
	if !a.Perpetual {
		return a.Effective.Valid()
	}
	if a.Source != SourceManual && a.Source != SourcePurchase && a.Source != SourceFree && a.Source != SourceMigration {
		return false
	}
	return !a.Effective.Start.IsZero() && a.Effective.End.IsZero()
}

func (a PlanAssignment) ActiveAt(at time.Time) bool {
	return a.Valid() && !at.IsZero() && !at.Before(a.Effective.Start) && (a.Perpetual || at.Before(a.Effective.End))
}

func (a PlanAssignment) Overlaps(b PlanAssignment) bool {
	return a.Valid() && b.Valid() && (a.Perpetual || b.Effective.Start.Before(a.Effective.End)) && (b.Perpetual || a.Effective.Start.Before(b.Effective.End))
}
