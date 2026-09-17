package pg

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
)

type allowanceLineageData struct {
	RootAssignment               string
	RootAnchor, TransitionAnchor time.Time
}

func readAllowanceLineage(ctx context.Context, q querier, account billing.AccountID, id string) (allowanceLineageData, error) {
	var data allowanceLineageData
	err := q.QueryRowContext(ctx, `SELECT root_assignment_id,root_anchor,transition_anchor
 FROM billing_allowance_lineage WHERE account_id=$1 AND schedule_id=$2`, string(account), id).Scan(&data.RootAssignment, &data.RootAnchor, &data.TransitionAnchor)
	if errors.Is(err, sql.ErrNoRows) {
		return data, billing.ErrState
	}
	data.RootAnchor = billing.CanonicalTime(data.RootAnchor)
	data.TransitionAnchor = billing.CanonicalTime(data.TransitionAnchor)
	return data, err
}

func readLineageSchedule(ctx context.Context, q querier, account billing.AccountID, id string) (credit.Schedule, error) {
	var s credit.Schedule
	var raw []byte
	var assignmentID, planID string
	var anchor sql.NullTime
	err := q.QueryRowContext(ctx, `SELECT assignment,assignment_id,plan_version_id,anchor,COALESCE(previous_schedule_id,''),
 subscription_provider,subscription_merchant,subscription_environment,subscription_id
 FROM billing_allowance_schedules WHERE account_id=$1 AND schedule_id=$2`, string(account), id).Scan(&raw, &assignmentID, &planID, &anchor, &s.PreviousScheduleID, &s.Subscription.Scope.Provider, &s.Subscription.Scope.Merchant, &s.Subscription.Scope.Environment, &s.Subscription.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return s, billing.ErrNotFound
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(raw, &s.Assignment); err != nil {
		return s, err
	}
	if s.Assignment.ID != assignmentID || s.Assignment.PlanVersionID != planID {
		return s, billing.ErrState
	}
	s.Account, s.ID, s.Anchor = account, id, scanTime(anchor)
	return s, nil
}

func prepareAllowanceLineage(ctx context.Context, q querier, account billing.AccountID, s credit.Schedule) error {
	anchor := s.Anchor
	if anchor.IsZero() {
		anchor = s.Assignment.Effective.Start
	}
	data := allowanceLineageData{RootAssignment: s.Assignment.ID, RootAnchor: anchor, TransitionAnchor: anchor}
	if s.PreviousScheduleID != "" {
		if s.PreviousScheduleID == s.ID {
			return billing.ErrConflict
		}
		parent, err := readLineageSchedule(ctx, q, account, s.PreviousScheduleID)
		if err != nil {
			return err
		}
		if parent.Subscription != s.Subscription {
			return billing.ErrConflict
		}
		data, err = readAllowanceLineage(ctx, q, account, s.PreviousScheduleID)
		if err != nil {
			return err
		}
		if !s.Anchor.IsZero() {
			data.TransitionAnchor = s.Anchor
		}
	}
	if data.RootAssignment == "" || data.RootAnchor.IsZero() || data.TransitionAnchor.IsZero() {
		return billing.ErrState
	}
	if _, err := q.ExecContext(ctx, `INSERT INTO billing_allowance_lineage(account_id,schedule_id,root_assignment_id,root_anchor,transition_anchor)
 VALUES($1,$2,$3,$4,$5) ON CONFLICT(account_id,schedule_id) DO NOTHING`, string(account), s.ID, data.RootAssignment, databaseTime(data.RootAnchor), databaseTime(data.TransitionAnchor)); err != nil {
		return err
	}
	stored, err := readAllowanceLineage(ctx, q, account, s.ID)
	if err != nil {
		return err
	}
	if stored.RootAssignment != data.RootAssignment || !stored.RootAnchor.Equal(data.RootAnchor) || !stored.TransitionAnchor.Equal(data.TransitionAnchor) {
		return billing.ErrState
	}
	return nil
}

func (s *session) rootScheduleAnchor(ctx context.Context, scheduleID string) (time.Time, error) {
	data, err := readAllowanceLineage(ctx, s.tx, s.account, scheduleID)
	return data.TransitionAnchor, err
}
