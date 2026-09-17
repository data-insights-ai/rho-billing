package pg

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
)

func (t *allowanceTransaction) Schedules(ctx context.Context, after string, inclusive bool, limit int) ([]credit.Schedule, error) {
	if limit < 1 || limit > 1001 {
		return nil, billing.ErrInvalid
	}
	s := t.session
	schedulePredicate := "schedule_id>$2"
	if inclusive {
		// A period cursor resumes the schedule named by After; later schedules
		// remain eligible in the same page.
		schedulePredicate = "schedule_id>=$2"
	}
	rows, err := s.tx.QueryContext(ctx, fmt.Sprintf(`
		SELECT schedule_id,subscription_id,subscription_provider,subscription_merchant,subscription_environment,assignment_id,plan_version_id,assignment,anchor,state,state_effective_at,revision,COALESCE(previous_schedule_id,''),adjustment_mode,source_id
		FROM billing_allowance_schedules
		WHERE account_id=$1 AND %s
		ORDER BY schedule_id LIMIT $3`, schedulePredicate), string(s.account), after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	schedules := make([]credit.Schedule, 0, limit)
	for rows.Next() {
		var schedule credit.Schedule
		var assignmentRaw []byte
		var anchor sql.NullTime
		var adjustmentMode, assignmentID, planID string
		if err := rows.Scan(&schedule.ID, &schedule.Subscription.ID, &schedule.Subscription.Scope.Provider, &schedule.Subscription.Scope.Merchant, &schedule.Subscription.Scope.Environment, &assignmentID, &planID, &assignmentRaw, &anchor, &schedule.State, &schedule.StateEffectiveAt, &schedule.Revision, &schedule.PreviousScheduleID, &adjustmentMode, &schedule.SourceID); err != nil {
			return nil, err
		}
		schedule.Adjustment, err = parseScheduleAdjustment(adjustmentMode)
		if err != nil {
			return nil, err
		}
		schedule.Account = s.account
		if err := json.Unmarshal(assignmentRaw, &schedule.Assignment); err != nil {
			return nil, err
		}
		// The assignment JSON contains the stable ID and plan reference; the
		// explicit columns are checked below to detect corrupt storage.
		if schedule.Assignment.ID != assignmentID || schedule.Assignment.PlanVersionID != planID {
			return nil, billing.ErrConflict
		}
		if anchor.Valid {
			schedule.Anchor = anchor.Time.UTC()
		}
		schedule.StateEffectiveAt = schedule.StateEffectiveAt.UTC()
		schedules = append(schedules, schedule)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return schedules, nil
}
func (t *allowanceTransaction) SuccessorAt(ctx context.Context, id string) (time.Time, error) {
	return t.session.allowanceSuccessorEffectiveAt(ctx, id)
}
func (t *allowanceTransaction) Lineage(ctx context.Context, id string) (credit.Lineage, error) {
	data, err := readAllowanceLineage(ctx, t.session.tx, t.session.account, id)
	return credit.Lineage{RootAssignment: data.RootAssignment, RootAnchor: data.RootAnchor, TransitionAnchor: data.TransitionAnchor}, err
}
func (t *allowanceTransaction) PeriodFacts(ctx context.Context, id string, periods []billing.Period) ([]credit.PeriodFact, error) {
	return t.session.allowancePeriodFacts(ctx, id, periods)
}
func (t *allowanceTransaction) Credits() credit.Repository { return t.session.Credits() }
func (t *allowanceTransaction) Issuance(ctx context.Context, scheduleID, definitionID string, start time.Time) (credit.Issuance, error) {
	var in credit.Issuance
	in.Account, in.ScheduleID, in.DefinitionID = t.session.account, scheduleID, definitionID
	err := t.session.tx.QueryRowContext(ctx, `SELECT subscription_id,assignment_id,plan_version_id,period_start,period_end,grant_key,operation_id,unit_code,unit_scale,entitled_amount,amount,status,eligibility_source_id,issued_at FROM billing_allowance_issuances WHERE account_id=$1 AND schedule_id=$2 AND definition_id=$3 AND period_start=$4`, string(in.Account), scheduleID, definitionID, databaseTime(start)).Scan(&in.SubscriptionID, &in.AssignmentID, &in.PlanVersionID, &in.Period.Start, &in.Period.End, &in.GrantKey, &in.Operation, &in.Unit.Code, &in.Unit.Scale, &in.EntitledAmount, &in.Amount, &in.Status, &in.EligibilitySource, &in.IssuedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return credit.Issuance{}, billing.ErrNotFound
	}
	in.Period.Start = billing.CanonicalTime(in.Period.Start)
	in.Period.End = billing.CanonicalTime(in.Period.End)
	in.IssuedAt = billing.CanonicalTime(in.IssuedAt)
	return in, err
}
func (t *allowanceTransaction) HighWater(ctx context.Context, lineage, definitionID string, start time.Time) (int64, error) {
	var amount int64
	err := t.session.tx.QueryRowContext(ctx, `SELECT entitled_amount FROM billing_allowance_high_water WHERE account_id=$1 AND lineage_key=$2 AND definition_id=$3 AND period_start=$4 FOR UPDATE`, string(t.session.account), lineage, definitionID, databaseTime(start)).Scan(&amount)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return amount, err
}
func (t *allowanceTransaction) SaveIssuance(ctx context.Context, in credit.Issuance, lineage string) error {
	if err := t.session.scope(in.Account); err != nil {
		return err
	}
	_, err := t.session.tx.ExecContext(ctx, `INSERT INTO billing_allowance_issuances(account_id,schedule_id,subscription_id,assignment_id,plan_version_id,definition_id,period_start,period_end,grant_key,operation_id,unit_code,unit_scale,entitled_amount,amount,status,eligibility_source_id,issued_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`, string(in.Account), in.ScheduleID, in.SubscriptionID, in.AssignmentID, in.PlanVersionID, in.DefinitionID, databaseTime(in.Period.Start), databaseTime(in.Period.End), in.GrantKey, string(in.Operation), in.Unit.Code, in.Unit.Scale, in.EntitledAmount, in.Amount, string(in.Status), in.EligibilitySource, databaseTime(in.IssuedAt))
	if err != nil {
		return err
	}
	_, err = t.session.tx.ExecContext(ctx, `INSERT INTO billing_allowance_high_water(account_id,subscription_id,definition_id,period_start,entitled_amount,assignment_id,plan_version_id,updated_at,lineage_key) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT(account_id,lineage_key,definition_id,period_start) DO UPDATE SET entitled_amount=GREATEST(billing_allowance_high_water.entitled_amount,EXCLUDED.entitled_amount),assignment_id=CASE WHEN EXCLUDED.entitled_amount>billing_allowance_high_water.entitled_amount THEN EXCLUDED.assignment_id ELSE billing_allowance_high_water.assignment_id END,plan_version_id=CASE WHEN EXCLUDED.entitled_amount>billing_allowance_high_water.entitled_amount THEN EXCLUDED.plan_version_id ELSE billing_allowance_high_water.plan_version_id END,updated_at=CASE WHEN EXCLUDED.entitled_amount>billing_allowance_high_water.entitled_amount THEN EXCLUDED.updated_at ELSE billing_allowance_high_water.updated_at END`, string(in.Account), in.SubscriptionID, in.DefinitionID, databaseTime(in.Period.Start), in.EntitledAmount, in.AssignmentID, in.PlanVersionID, databaseTime(in.IssuedAt), lineage)
	return err
}

func (t *allowanceTransaction) Checkpoint(ctx context.Context, id string) (credit.Checkpoint, error) {
	var out credit.Checkpoint
	var periodStart, through, dirtyFrom, completed sql.NullTime
	err := t.session.tx.QueryRowContext(ctx, `SELECT checkpoint_id,revision,schedule_id,definition_id,period_start,through,dirty_from,completed_through,processed_change_id,change_observed_id,has_more,pass_active FROM billing_allowance_checkpoints WHERE account_id=$1 AND checkpoint_id=$2`, string(t.session.account), id).Scan(&out.ID, &out.Revision, &out.ScheduleID, &out.DefinitionID, &periodStart, &through, &dirtyFrom, &completed, &out.ProcessedChange, &out.ChangeObserved, &out.HasMore, &out.PassActive)
	if errors.Is(err, sql.ErrNoRows) {
		return credit.Checkpoint{}, billing.ErrNotFound
	}
	if err != nil {
		return credit.Checkpoint{}, err
	}
	out.Account = t.session.account
	out.PeriodStart, out.Through, out.DirtyFrom, out.CompletedThrough = scanTime(periodStart), scanTime(through), scanTime(dirtyFrom), scanTime(completed)
	return out, nil
}

func (t *allowanceTransaction) AllowanceChangeWatermark(ctx context.Context) (int64, error) {
	var watermark int64
	err := t.session.tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(change_id),0) FROM billing_allowance_change_journal WHERE account_id=$1`, string(t.session.account)).Scan(&watermark)
	return watermark, err
}

func (t *allowanceTransaction) AllowanceChanges(ctx context.Context, after int64, limit int) ([]credit.AllowanceChange, error) {
	if after < 0 || limit < 1 || limit > 256 {
		return nil, billing.ErrInvalid
	}
	rows, err := t.session.tx.QueryContext(ctx, `SELECT change_id,schedule_id,effective_at,changed_at FROM billing_allowance_change_journal WHERE account_id=$1 AND change_id>$2 ORDER BY change_id LIMIT $3`, string(t.session.account), after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	changes := make([]credit.AllowanceChange, 0, limit)
	for rows.Next() {
		var change credit.AllowanceChange
		if err := rows.Scan(&change.Sequence, &change.ScheduleID, &change.EffectiveAt, &change.ChangedAt); err != nil {
			return nil, err
		}
		change.EffectiveAt = billing.CanonicalTime(change.EffectiveAt)
		change.ChangedAt = billing.CanonicalTime(change.ChangedAt)
		changes = append(changes, change)
	}
	return changes, rows.Err()
}

func (t *allowanceTransaction) SaveCheckpoint(ctx context.Context, in credit.Checkpoint, expectedRevision int64) error {
	if in.Account != t.session.account || !billing.ValidID(in.ID) || expectedRevision < 0 {
		return billing.ErrInvalid
	}
	if in.Revision != expectedRevision+1 {
		return billing.ErrConflict
	}
	result, err := t.session.tx.ExecContext(ctx, `UPDATE billing_allowance_checkpoints SET revision=$3,schedule_id=$4,definition_id=$5,period_start=$6,through=$7,dirty_from=$8,completed_through=$9,processed_change_id=$10,change_observed_id=$11,has_more=$12,pass_active=$13,updated_at=$14 WHERE account_id=$1 AND checkpoint_id=$2 AND revision=$15`, string(in.Account), in.ID, in.Revision, in.ScheduleID, in.DefinitionID, nullTime(in.PeriodStart), nullTime(in.Through), nullTime(in.DirtyFrom), nullTime(in.CompletedThrough), in.ProcessedChange, in.ChangeObserved, in.HasMore, in.PassActive, databaseTime(t.session.store.now()), expectedRevision)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 1 {
		return nil
	}
	if expectedRevision != 0 {
		return billing.ErrConflict
	}
	result, err = t.session.tx.ExecContext(ctx, `INSERT INTO billing_allowance_checkpoints(account_id,checkpoint_id,revision,schedule_id,definition_id,period_start,through,dirty_from,completed_through,processed_change_id,change_observed_id,has_more,pass_active,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14) ON CONFLICT DO NOTHING`, string(in.Account), in.ID, in.Revision, in.ScheduleID, in.DefinitionID, nullTime(in.PeriodStart), nullTime(in.Through), nullTime(in.DirtyFrom), nullTime(in.CompletedThrough), in.ProcessedChange, in.ChangeObserved, in.HasMore, in.PassActive, databaseTime(t.session.store.now()))
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return billing.ErrConflict
	}
	return nil
}
