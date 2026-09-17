package credit

import (
	"context"
	"errors"
	"math"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/catalog"
	"github.com/data-insights-ai/rho-billing/subscription"
)

type AllowanceService struct {
	repo AllowanceRepository
	now  func() time.Time
}

func NewAllowances(repo AllowanceRepository, now func() time.Time) *AllowanceService {
	if repo == nil {
		panic("credit: nil allowance repository")
	}
	if now == nil {
		now = time.Now
	}
	return &AllowanceService{repo: repo, now: now}
}

func (s *AllowanceService) PutSchedule(ctx context.Context, in Schedule, expectedRevision int64) (Schedule, error) {
	if err := ctx.Err(); err != nil {
		return Schedule{}, err
	}
	if expectedRevision < 0 {
		return Schedule{}, billing.ErrInvalid
	}
	in = normalizeSchedule(in)
	if err := ValidateSchedule(in); err != nil {
		return Schedule{}, err
	}
	var out Schedule
	err := s.repo.WithinAccount(ctx, in.Account, func(tx AllowanceTx) error {
		plan, err := tx.Plan(ctx, in.Assignment.PlanVersionID)
		if err != nil {
			return err
		}
		if !catalog.ValidPlan(plan) || plan.ID != in.Assignment.PlanVersionID {
			return billing.ErrConflict
		}
		snapshot, err := tx.Subscription(ctx, in.Subscription)
		if err != nil {
			return err
		}
		if snapshot.Account != in.Account || snapshot.Ref != in.Subscription || !matchesAssignment(snapshot, in.Assignment) {
			return billing.ErrConflict
		}

		existing, err := tx.Schedule(ctx, in.ID)
		existingFound := false
		switch {
		case err == nil:
			existingFound = true
			if err := sameScheduleIdentity(existing, in); err != nil {
				return err
			}
		case errors.Is(err, billing.ErrNotFound):
			if expectedRevision != 0 {
				return billing.ErrConflict
			}
		default:
			return err
		}

		if in.PreviousScheduleID != "" {
			if in.PreviousScheduleID == in.ID {
				return billing.ErrConflict
			}
			previous, err := tx.Schedule(ctx, in.PreviousScheduleID)
			if err != nil {
				return err
			}
			if previous.ID != in.PreviousScheduleID || previous.Account != in.Account || previous.Subscription != in.Subscription {
				return billing.ErrConflict
			}
			previousPlan, err := tx.Plan(ctx, previous.Assignment.PlanVersionID)
			if err != nil {
				return err
			}
			anchor := previous.Anchor
			if anchor.IsZero() {
				anchor, err = tx.TransitionAnchor(ctx, previous.ID)
				if err != nil {
					return err
				}
			}
			if err := scheduleTransition(previousPlan, plan, previous.Assignment, in.Assignment, anchor, in.Assignment.Effective.Start, in.Adjustment); err != nil {
				return err
			}
		}

		if existingFound {
			if existing.Revision == math.MaxInt64 {
				return billing.ErrOverflow
			}
			if expectedRevision == 0 || existing.Revision != expectedRevision {
				return billing.ErrConflict
			}
			in.Revision = existing.Revision + 1
		} else {
			in.Revision = 1
		}
		stamp := billing.CanonicalTime(s.now())
		if stamp.IsZero() {
			return billing.ErrInvalid
		}
		if err := tx.SaveSchedule(ctx, in, expectedRevision, stamp); err != nil {
			return err
		}
		out = in
		return nil
	})
	if err != nil {
		return Schedule{}, err
	}
	return out, nil
}

func normalizeSchedule(in Schedule) Schedule {
	in.Assignment.Effective.Start = billing.CanonicalTime(in.Assignment.Effective.Start)
	in.Assignment.Effective.End = billing.CanonicalTime(in.Assignment.Effective.End)
	in.StateEffectiveAt = billing.CanonicalTime(in.StateEffectiveAt)
	if !in.Anchor.IsZero() {
		in.Anchor = billing.CanonicalTime(in.Anchor)
	}
	return in
}

func matchesAssignment(snapshot subscription.Snapshot, want catalog.PlanAssignment) bool {
	for _, assignment := range snapshot.Assignments {
		if assignment.ID == want.ID && assignment.PlanVersionID == want.PlanVersionID && assignment.Quantity == want.Quantity && assignment.Source == want.Source && billing.CanonicalTime(assignment.Effective.Start).Equal(want.Effective.Start) && billing.CanonicalTime(assignment.Effective.End).Equal(want.Effective.End) {
			return true
		}
	}
	return false
}

func sameScheduleIdentity(existing, want Schedule) error {
	if existing.ID != want.ID || existing.Account != want.Account || existing.Subscription != want.Subscription ||
		existing.Assignment.ID != want.Assignment.ID || existing.Assignment.PlanVersionID != want.Assignment.PlanVersionID ||
		!billing.CanonicalTime(existing.Assignment.Effective.Start).Equal(want.Assignment.Effective.Start) ||
		existing.PreviousScheduleID != want.PreviousScheduleID || !billing.CanonicalTime(existing.Anchor).Equal(want.Anchor) {
		return ErrAdjustment
	}
	return nil
}

func scheduleTransition(previousPlan, nextPlan catalog.PlanVersion, previousAssignment, nextAssignment catalog.PlanAssignment, anchor, candidate time.Time, mode AdjustmentMode) error {
	if anchor.IsZero() {
		anchor = previousAssignment.Effective.Start
	}
	if mode == AdjustmentReject || mode == AdjustmentProrated || mode == AdjustmentInitial {
		return ErrAdjustment
	}
	for _, definition := range previousPlan.Allowances {
		nextDefinition, found := findAllowance(nextPlan, definition.ID)
		if found && nextDefinition.Recurrence != definition.Recurrence {
			return ErrAdjustment
		}
		if definition.Recurrence == catalog.AllowanceOneTime {
			continue
		}
		boundary := isDefinitionBoundary(anchor, candidate, definition)
		if !found {
			// Removing an allowance is a downgrade, even if another allowance grows.
			if !boundary {
				return ErrAdjustment
			}
			continue
		}
		// A boundary for one metric cannot authorize a change halfway through
		// another metric's annual period or differently anchored monthly period.
		if boundary && isDefinitionBoundary(anchor, candidate, nextDefinition) {
			continue
		}
		if mode != AdjustmentDelta || !previousAssignment.Effective.Contains(candidate) {
			return ErrAdjustment
		}
		// A delta retains the original bucket. Changing its calendar at the same
		// time requires a boundary transition, not a new full mid-cycle grant.
		if !billing.CanonicalTime(definition.Anchor).Equal(billing.CanonicalTime(nextDefinition.Anchor)) {
			return ErrAdjustment
		}
		previousAmount, err := transitionAmount(definition.Amount, previousAssignment.Quantity)
		if err != nil {
			return err
		}
		newAmount, err := transitionAmount(nextDefinition.Amount, nextAssignment.Quantity)
		if err != nil {
			return err
		}
		if newAmount <= previousAmount {
			return ErrAdjustment
		}
	}
	for _, definition := range nextPlan.Allowances {
		if _, found := findAllowance(previousPlan, definition.ID); found || definition.Recurrence == catalog.AllowanceOneTime {
			continue
		}
		// A newly introduced recurring allowance has no prior grant against which
		// a positive delta can be rated. Start it at its own recurrence boundary.
		if !isDefinitionBoundary(anchor, candidate, definition) {
			return ErrAdjustment
		}
	}
	return nil
}

func isDefinitionBoundary(anchor, candidate time.Time, definition catalog.AllowanceDefinition) bool {
	if !definition.Anchor.IsZero() {
		anchor = definition.Anchor
	}
	anchor = billing.CanonicalTime(anchor)
	candidate = billing.CanonicalTime(candidate)
	if anchor.IsZero() || candidate.Before(anchor) || (definition.Recurrence != catalog.AllowanceMonthly && definition.Recurrence != catalog.AllowanceAnnual) {
		return false
	}
	months := (candidate.Year()-anchor.Year())*12 + int(candidate.Month()-anchor.Month())
	if months%recurrenceMonths(definition.Recurrence) != 0 {
		return false
	}
	return calendarDate(anchor, anchor.Day(), months).Equal(candidate)
}

func transitionAmount(amount, quantity int64) (int64, error) {
	if amount <= 0 || quantity <= 0 {
		return 0, billing.ErrInvalid
	}
	if amount > math.MaxInt64/quantity {
		return 0, billing.ErrOverflow
	}
	return amount * quantity, nil
}

func findAllowance(plan catalog.PlanVersion, id string) (catalog.AllowanceDefinition, bool) {
	for _, definition := range plan.Allowances {
		if definition.ID == id {
			return definition, true
		}
	}
	return catalog.AllowanceDefinition{}, false
}
