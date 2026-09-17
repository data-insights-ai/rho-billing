package catalog

// Entitlement resolution does not inspect provider status or subscription state;
// the caller supplies the assignments and historical plan versions.

import (
	"maps"
	"math"
	"slices"
	"time"

	"github.com/data-insights-ai/rho-billing"

	"github.com/data-insights-ai/rho-billing/internal/checked"
)

type ResolvedEntitlement struct {
	Key         string
	Kind        EntitlementKind
	Aggregation AggregationMode
	Enabled     bool
	Amount      int64
	Unit        billing.Unit
}

type EntitlementSnapshot struct {
	At           time.Time
	Entitlements map[string]ResolvedEntitlement
}

type EntitlementInput struct {
	At          time.Time
	Assignments []PlanAssignment
	Versions    []PlanVersion
	Revocations []Revocation
}

func ResolveEntitlements(input EntitlementInput) (EntitlementSnapshot, error) {
	if input.At.IsZero() {
		return EntitlementSnapshot{}, billing.ErrInvalid
	}
	plans := make(map[string]PlanVersion, len(input.Versions))
	for _, plan := range input.Versions {
		if _, ok := plans[plan.ID]; ok {
			return EntitlementSnapshot{}, billing.ErrConflict
		}
		if !ValidPlan(plan) {
			return EntitlementSnapshot{}, billing.ErrInvalid
		}
		plans[plan.ID] = plan
	}
	for i, assignment := range input.Assignments {
		if err := validateAssignment(assignment); err != nil {
			return EntitlementSnapshot{}, err
		}
		for j := i + 1; j < len(input.Assignments); j++ {
			other := input.Assignments[j]
			if assignment.ID != "" && assignment.ID == other.ID && assignment.Overlaps(other) {
				return EntitlementSnapshot{}, billing.ErrConflict
			}
		}
	}
	revocations := make(map[string]Revocation, len(input.Revocations))
	assignments := make(map[string]PlanAssignment, len(input.Assignments))
	assignmentCounts := make(map[string]int, len(input.Assignments))
	for _, assignment := range input.Assignments {
		if assignment.ID != "" {
			assignments[assignment.ID] = assignment
			assignmentCounts[assignment.ID]++
		}
	}
	for _, revocation := range input.Revocations {
		if err := revocation.Validate(); err != nil {
			return EntitlementSnapshot{}, err
		}
		if _, exists := revocations[revocation.AssignmentID]; exists {
			return EntitlementSnapshot{}, billing.ErrConflict
		}
		assignment, exists := assignments[revocation.AssignmentID]
		if !revocation.AssignmentStart.IsZero() {
			// A reused ID is disambiguated by naming the interval.
			exists = false
			for _, candidate := range input.Assignments {
				if candidate.ID == revocation.AssignmentID && candidate.Effective.Start.Equal(revocation.AssignmentStart) {
					assignment, exists = candidate, true
					break
				}
			}
			if !exists || revocation.EffectiveAt.Before(assignment.Effective.Start) {
				return EntitlementSnapshot{}, billing.ErrConflict
			}
		} else if !exists || assignmentCounts[revocation.AssignmentID] != 1 || revocation.EffectiveAt.Before(assignment.Effective.Start) {
			return EntitlementSnapshot{}, billing.ErrConflict
		}
		revocations[revocation.AssignmentID] = revocation
	}

	result := EntitlementSnapshot{At: input.At, Entitlements: make(map[string]ResolvedEntitlement)}
	for _, assignment := range input.Assignments {
		if revocation, revoked := revocations[assignment.ID]; revoked && !input.At.Before(revocation.EffectiveAt) {
			if revocation.AssignmentStart.IsZero() || revocation.AssignmentStart.Equal(assignment.Effective.Start) {
				continue
			}
		}
		if !assignment.ActiveAt(input.At) {
			continue
		}
		plan, ok := plans[assignment.PlanVersionID]
		if !ok {
			return EntitlementSnapshot{}, billing.ErrNotFound
		}
		for _, definition := range plan.Entitlements {
			current, exists := result.Entitlements[definition.Key]
			if !exists {
				current = ResolvedEntitlement{
					Key: definition.Key, Kind: definition.Kind, Aggregation: definition.Aggregation,
					Enabled: definition.Enabled, Amount: 0, Unit: definition.Unit,
				}
				if definition.Kind == EntitlementFeature {
					result.Entitlements[definition.Key] = current
					continue
				}
				amount, err := quantityAwareAmount(definition.Amount, assignment.Quantity)
				if err != nil {
					return EntitlementSnapshot{}, err
				}
				current.Amount = amount
				result.Entitlements[definition.Key] = current
				continue
			}
			if current.Kind != definition.Kind || current.Aggregation != definition.Aggregation || current.Unit != definition.Unit {
				return EntitlementSnapshot{}, billing.ErrConflict
			}
			if definition.Kind == EntitlementFeature {
				current.Enabled = current.Enabled || definition.Enabled
				result.Entitlements[definition.Key] = current
				continue
			}
			amount, err := quantityAwareAmount(definition.Amount, assignment.Quantity)
			if err != nil {
				return EntitlementSnapshot{}, err
			}
			switch definition.Aggregation {
			case AggregationMAX:
				current.Amount = max(current.Amount, amount)
			case AggregationSUM:
				current.Amount, err = checked.Add(current.Amount, amount)
				if err != nil {
					return EntitlementSnapshot{}, err
				}
			default:
				return EntitlementSnapshot{}, billing.ErrConflict
			}
			result.Entitlements[definition.Key] = current
		}
	}
	return result, nil
}

func (s EntitlementSnapshot) Keys() []string {
	return slices.Sorted(maps.Keys(s.Entitlements))
}

func (s EntitlementSnapshot) Get(key string) (ResolvedEntitlement, bool) {
	value, ok := s.Entitlements[key]
	return value, ok
}

func validateAssignment(a PlanAssignment) error {
	if !a.Valid() {
		return billing.ErrInvalid
	}
	return nil
}

func quantityAwareAmount(amount, quantity int64) (int64, error) {
	if amount < 0 || quantity <= 0 {
		return 0, billing.ErrInvalid
	}
	if amount != 0 && quantity > math.MaxInt64/amount {
		return 0, billing.ErrOverflow
	}
	return amount * quantity, nil
}
