package credit

import (
	"context"
	"errors"
	"fmt"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/catalog"

	"github.com/data-insights-ai/rho-billing/internal/identity"
)

const (
	defaultMaxIssuances = 1000
	defaultMaxPeriods   = 1000
)

func (s *AllowanceService) AdvanceCheckpoint(ctx context.Context, request CheckpointRequest) (CheckpointResult, error) {
	if err := ctx.Err(); err != nil {
		return CheckpointResult{}, err
	}
	request.Now = billing.CanonicalTime(request.Now)
	if !billing.ValidID(string(request.Account)) || !billing.ValidID(request.ID) || request.Now.IsZero() || request.Revision < 0 || request.Limit < 1 || request.Limit > 1000 || request.MaxIssuances < 0 || request.MaxIssuances > 10000 || request.MaxPeriods < 0 || request.MaxPeriods > 10000 {
		return CheckpointResult{}, billing.ErrInvalid
	}
	for _, evidence := range request.Evidence {
		if evidence.Account != request.Account {
			return CheckpointResult{}, billing.ErrNotFound
		}
		if err := evidence.Validate(); err != nil {
			return CheckpointResult{}, err
		}
	}
	var result CheckpointResult
	err := s.repo.WithinAccount(ctx, request.Account, func(tx AllowanceTx) error {
		checkpoint, err := tx.Checkpoint(ctx, request.ID)
		if errors.Is(err, billing.ErrNotFound) {
			checkpoint = Checkpoint{Account: request.Account, ID: request.ID}
		} else if err != nil {
			return err
		}
		if checkpoint.Account != request.Account || checkpoint.ID != request.ID {
			return billing.ErrConflict
		}
		if request.Revision != 0 && checkpoint.Revision != request.Revision {
			return billing.ErrConflict
		}
		for _, evidence := range request.Evidence {
			if err := recordEligibility(ctx, tx, evidence); err != nil {
				return err
			}
		}
		watermark, err := tx.AllowanceChangeWatermark(ctx)
		if err != nil {
			return err
		}
		if watermark < checkpoint.ChangeObserved || checkpoint.ProcessedChange > checkpoint.ChangeObserved {
			return billing.ErrConflict
		}
		expectedRevision := checkpoint.Revision
		if !checkpoint.PassActive {
			if !checkpoint.CompletedThrough.IsZero() && !request.Now.After(checkpoint.CompletedThrough) && watermark == checkpoint.ProcessedChange {
				result = CheckpointResult{Checkpoint: checkpoint}
				return nil
			}
			checkpoint.PassActive = true
			checkpoint.ChangeObserved = watermark
			if request.Now.After(checkpoint.Through) {
				checkpoint.Through = request.Now
			}
			checkpoint.ScheduleID, checkpoint.DefinitionID, checkpoint.PeriodStart = "", "", time.Time{}
			checkpoint.DirtyFrom = time.Time{}
		}
		if checkpoint.ProcessedChange < checkpoint.ChangeObserved {
			changes, err := tx.AllowanceChanges(ctx, checkpoint.ProcessedChange, 256)
			if err != nil {
				return err
			}
			if len(changes) == 0 || len(changes) > 256 {
				return billing.ErrConflict
			}
			for _, change := range changes {
				if change.Sequence > checkpoint.ChangeObserved {
					break
				}
				if change.Sequence <= checkpoint.ProcessedChange || change.EffectiveAt.IsZero() {
					return billing.ErrConflict
				}
				checkpoint.ProcessedChange = change.Sequence
				if checkpoint.DirtyFrom.IsZero() || change.EffectiveAt.Before(checkpoint.DirtyFrom) {
					checkpoint.DirtyFrom = change.EffectiveAt
				}
			}
		}
		issued := issueResult{HasMore: true}
		if checkpoint.ProcessedChange == checkpoint.ChangeObserved {
			issued, err = s.issueDue(ctx, tx, issueRequest{Account: request.Account, Now: checkpoint.Through, After: checkpoint.ScheduleID, AfterPeriod: checkpoint.PeriodStart, AfterDefinition: checkpoint.DefinitionID, Limit: request.Limit, MaxIssuances: request.MaxIssuances, MaxPeriods: request.MaxPeriods})
			if err != nil {
				return err
			}
			checkpoint.ScheduleID, checkpoint.PeriodStart, checkpoint.DefinitionID = issued.Next, issued.NextPeriod, issued.NextDefinition
			if !issued.HasMore {
				checkpoint.CompletedThrough = checkpoint.Through
				checkpoint.PassActive = false
				checkpoint.DirtyFrom = time.Time{}
				checkpoint.ScheduleID, checkpoint.PeriodStart, checkpoint.DefinitionID = "", time.Time{}, ""
			}
		}
		checkpoint.Revision++
		checkpoint.HasMore = checkpoint.PassActive || watermark > checkpoint.ChangeObserved

		if err := tx.SaveCheckpoint(ctx, checkpoint, expectedRevision); err != nil {
			return err
		}
		result = CheckpointResult{Checkpoint: checkpoint, Issuances: issued.Issuances, HasMore: checkpoint.HasMore, NextSchedule: issued.Next, NextPeriod: issued.NextPeriod, NextDefinition: issued.NextDefinition}
		return nil
	})
	if err != nil {
		return CheckpointResult{}, err
	}
	return result, nil
}

func (s *AllowanceService) issueDue(ctx context.Context, tx AllowanceTx, request issueRequest) (issueResult, error) {
	limit := request.Limit
	schedules, err := tx.Schedules(ctx, request.After, !request.AfterPeriod.IsZero(), limit+1)
	if err != nil {
		return issueResult{}, err
	}
	if len(schedules) > limit+1 {
		return issueResult{}, billing.ErrConflict
	}
	hasMore := len(schedules) > limit
	if hasMore {
		schedules = schedules[:limit]
	}
	maxIssuances := request.MaxIssuances
	if maxIssuances == 0 {
		maxIssuances = defaultMaxIssuances
	}
	maxPeriods := request.MaxPeriods
	if maxPeriods == 0 {
		maxPeriods = defaultMaxPeriods
	}
	result := issueResult{Issuances: make([]Issuance, 0)}
	var lastCompleted string
	for _, schedule := range schedules {
		if !billing.ValidID(schedule.ID) || schedule.Account != request.Account {
			return issueResult{}, billing.ErrConflict
		}
		if lastCompleted != "" && schedule.ID <= lastCompleted {
			return issueResult{}, billing.ErrConflict
		}
		if request.After != "" {
			if request.AfterPeriod.IsZero() && schedule.ID <= request.After {
				return issueResult{}, billing.ErrConflict
			}
			if !request.AfterPeriod.IsZero() && schedule.ID < request.After {
				return issueResult{}, billing.ErrConflict
			}
		}
		remaining := maxIssuances - len(result.Issuances)
		if remaining <= 0 || maxPeriods <= 0 {
			result.HasMore = true
			result.Next = lastCompleted
			return result, nil
		}
		afterPeriod, afterDefinition := time.Time{}, ""
		if schedule.ID == request.After {
			afterPeriod, afterDefinition = request.AfterPeriod, request.AfterDefinition
		}
		successorAt, err := tx.SuccessorAt(ctx, schedule.ID)
		if err != nil {
			return issueResult{}, fmt.Errorf("schedule %s successor: %w", schedule.ID, err)
		}
		issued, more, nextPeriod, nextDefinition, err := s.issueSchedule(ctx, tx, request.Now, schedule, remaining, &maxPeriods, afterPeriod, afterDefinition, successorAt)
		if err != nil {
			return issueResult{}, fmt.Errorf("schedule %s: %w", schedule.ID, err)
		}
		result.Issuances = append(result.Issuances, issued...)
		if more {
			result.HasMore = true
			result.Next = schedule.ID
			result.NextPeriod = nextPeriod
			result.NextDefinition = nextDefinition
			return result, nil
		}
		lastCompleted = schedule.ID
	}
	if len(schedules) > 0 && hasMore {
		result.Next = schedules[len(schedules)-1].ID
		result.HasMore = true
	}
	return result, nil
}

func (s *AllowanceService) issueSchedule(ctx context.Context, tx AllowanceTx, now time.Time, schedule Schedule, maxIssuances int, remainingPeriods *int, afterPeriod time.Time, afterDefinition string, successorAt time.Time) ([]Issuance, bool, time.Time, string, error) {
	if schedule.Assignment.Effective.Start.After(now) {
		return nil, false, time.Time{}, "", nil
	}
	plan, err := tx.Plan(ctx, schedule.Assignment.PlanVersionID)
	if err != nil {
		return nil, false, time.Time{}, "", err
	}
	if !catalog.ValidPlan(plan) || plan.ID != schedule.Assignment.PlanVersionID {
		return nil, false, time.Time{}, "", billing.ErrConflict
	}
	through := now.Add(time.Microsecond)
	if !successorAt.IsZero() && successorAt.Before(through) {
		through = successorAt
	}
	lineageData, err := tx.Lineage(ctx, schedule.ID)
	if err != nil {
		return nil, false, time.Time{}, "", err
	}
	if lineageData.RootAssignment == "" || lineageData.RootAnchor.IsZero() {
		return nil, false, time.Time{}, "", billing.ErrState
	}
	lineage := identity.Fingerprint(schedule.Subscription.Scope.Provider, schedule.Subscription.Scope.Merchant, schedule.Subscription.Scope.Environment, schedule.Subscription.ID, lineageData.RootAssignment)
	originalDay := lineageData.RootAnchor.Day()
	result := make([]Issuance, 0)
	resumingDefinition := afterDefinition != ""
	matchedDefinition := afterDefinition == ""
	for _, definition := range plan.Allowances {
		if resumingDefinition && definition.ID != afterDefinition {
			continue
		}
		if resumingDefinition {
			matchedDefinition = true
		}
		anchor := lineageData.RootAnchor
		if anchor.IsZero() {
			anchor = schedule.Anchor
		}
		if !definition.Anchor.IsZero() {
			anchor = definition.Anchor
		}
		definitionAfter := time.Time{}
		if definition.ID == afterDefinition {
			definitionAfter = afterPeriod
		}
		periods, pageMore, err := PeriodsPage(PeriodInput{Definition: definition, Assignment: schedule.Assignment, Anchor: anchor, OriginalDay: originalDay, Through: through}, definitionAfter, *remainingPeriods)
		if err != nil {
			return nil, false, time.Time{}, "", fmt.Errorf("periods %s: %w", definition.ID, err)
		}
		facts, err := tx.PeriodFacts(ctx, schedule.ID, periods)
		if err != nil {
			return nil, false, time.Time{}, "", err
		}
		if len(facts) != len(periods) || len(facts) > *remainingPeriods {
			return nil, false, time.Time{}, "", billing.ErrConflict
		}
		*remainingPeriods -= len(periods)
		lastPeriod := time.Time{}
		for i, period := range periods {
			lastPeriod = period.Start
			if !successorAt.IsZero() && !period.Start.Before(successorAt) || period.Start.After(now) {
				continue
			}
			fact := facts[i]
			if fact.State.State != ScheduleActive {
				continue
			}
			if fact.State.Account != schedule.Account || fact.State.ID != schedule.ID {
				return nil, false, time.Time{}, "", billing.ErrConflict
			}
			if fact.SourceID == "" || fact.Eligibility.Covers(period) != nil {
				continue
			}
			issuance, created, err := s.issuePeriod(ctx, tx, now, schedule, plan, definition, lineage, period, fact.Eligibility, fact.SourceID)
			if err != nil {
				return nil, false, time.Time{}, "", fmt.Errorf("period %s: %w", definition.ID, err)
			}
			if created {
				result = append(result, issuance)
				if len(result) == maxIssuances {
					return result, true, lastPeriod, definition.ID, nil
				}
			}
		}
		if pageMore || *remainingPeriods == 0 {
			return result, true, lastPeriod, definition.ID, nil
		}
		if resumingDefinition {
			resumingDefinition = false
			afterPeriod = time.Time{}
		}
	}
	if !matchedDefinition {
		return nil, false, time.Time{}, "", billing.ErrInvalid
	}
	return result, false, time.Time{}, "", nil
}

func (s *AllowanceService) issuePeriod(ctx context.Context, tx AllowanceTx, now time.Time, schedule Schedule, plan catalog.PlanVersion, definition catalog.AllowanceDefinition, lineage string, period billing.Period, eligibility Eligibility, sourceID string) (Issuance, bool, error) {
	existing, err := tx.Issuance(ctx, schedule.ID, definition.ID, period.Start)
	if err == nil {
		if existing.Account != schedule.Account || existing.ScheduleID != schedule.ID || existing.DefinitionID != definition.ID || !existing.Period.Start.Equal(period.Start) {
			return Issuance{}, false, billing.ErrConflict
		}
		return existing, false, nil
	}
	if !errors.Is(err, billing.ErrNotFound) {
		return Issuance{}, false, err
	}
	grant, err := prepareGrant(schedule, plan, definition, period, eligibility)
	if err != nil {
		return Issuance{}, false, err
	}
	key := identity.Fingerprint(lineage, grant.SourceRef)
	grant.Operation = billing.OperationID("allowance-op-" + key)
	grant.LotID = "allowance-lot-" + key
	grant.SourceRef = key
	entitled := grant.Amount
	highWater, err := tx.HighWater(ctx, lineage, definition.ID, period.Start)
	if err != nil {
		return Issuance{}, false, err
	}
	if highWater < 0 {
		return Issuance{}, false, billing.ErrState
	}
	if highWater > entitled {
		entitled = highWater
	}
	grantAmount := grant.Amount
	if highWater >= grantAmount {
		grantAmount = 0
	} else if highWater > 0 {
		grantAmount -= highWater
	}
	if grantAmount > 0 {
		grant.Amount = grantAmount
		if _, err := New(tx.Credits(), func() time.Time { return now }).Grant(ctx, grant); err != nil {
			return Issuance{}, false, err
		}
	}
	status := IssuanceCapped
	if grantAmount > 0 {
		status = IssuanceGranted
	}
	issuedAt := billing.CanonicalTime(now)
	issuance := Issuance{Account: schedule.Account, ScheduleID: schedule.ID, SubscriptionID: schedule.Subscription.ID, AssignmentID: schedule.Assignment.ID, PlanVersionID: schedule.Assignment.PlanVersionID, DefinitionID: definition.ID, Period: period, GrantKey: key, Operation: grant.Operation, Unit: definition.Unit, EntitledAmount: entitled, Amount: grantAmount, Status: status, EligibilitySource: sourceID, IssuedAt: issuedAt}
	if err := tx.SaveIssuance(ctx, issuance, lineage); err != nil {
		return Issuance{}, false, err
	}
	return issuance, true, nil
}
