package pg

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/catalog"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/integration"
	"github.com/data-insights-ai/rho-billing/subscription"
)

type allowanceRepository struct{ store *Store }

func (s *Store) Allowances() credit.AllowanceRepository { return &allowanceRepository{store: s} }

func (r *allowanceRepository) WithinAccount(ctx context.Context, account billing.AccountID, fn func(credit.AllowanceTx) error) error {
	if fn == nil {
		return billing.ErrInvalid
	}
	return r.store.Atomic(ctx, account, func(v integration.Session) error {
		return fn(&allowanceTransaction{session: v.(*session)})
	})
}

func (b *boundAllowances) WithinAccount(ctx context.Context, account billing.AccountID, fn func(credit.AllowanceTx) error) error {
	if fn == nil {
		return billing.ErrInvalid
	}
	if err := b.session.scope(account); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn(&allowanceTransaction{session: b.session})
}

type allowanceTransaction struct{ session *session }

func (t *allowanceTransaction) Plan(ctx context.Context, id string) (catalog.PlanVersion, error) {
	return readJSON[catalog.PlanVersion](ctx, t.session.tx, `SELECT definition FROM billing_plans WHERE id=$1`, id)
}
func (t *allowanceTransaction) Subscription(ctx context.Context, ref billing.Reference) (subscription.Snapshot, error) {
	return readSubscription(ctx, t.session.tx, t.session.account, ref)
}
func (t *allowanceTransaction) TransitionAnchor(ctx context.Context, id string) (time.Time, error) {
	return t.session.rootScheduleAnchor(ctx, id)
}
func (t *allowanceTransaction) Schedule(ctx context.Context, id string) (credit.Schedule, error) {
	var s credit.Schedule
	var assignmentRaw []byte
	var assignmentID, planID, mode string
	var anchor sql.NullTime
	err := t.session.tx.QueryRowContext(ctx, `SELECT assignment,assignment_id,plan_version_id,anchor,COALESCE(previous_schedule_id,''),
 subscription_provider,subscription_merchant,subscription_environment,subscription_id,state,state_effective_at,revision,adjustment_mode,source_id
 FROM billing_allowance_schedules WHERE account_id=$1 AND schedule_id=$2`, string(t.session.account), id).Scan(
		&assignmentRaw, &assignmentID, &planID, &anchor, &s.PreviousScheduleID, &s.Subscription.Scope.Provider, &s.Subscription.Scope.Merchant,
		&s.Subscription.Scope.Environment, &s.Subscription.ID, &s.State, &s.StateEffectiveAt, &s.Revision, &mode, &s.SourceID)
	if errors.Is(err, sql.ErrNoRows) {
		return credit.Schedule{}, billing.ErrNotFound
	}
	if err != nil {
		return credit.Schedule{}, err
	}
	if err := json.Unmarshal(assignmentRaw, &s.Assignment); err != nil {
		return credit.Schedule{}, err
	}
	if s.Assignment.ID != assignmentID || s.Assignment.PlanVersionID != planID {
		return credit.Schedule{}, billing.ErrState
	}
	s.Account, s.ID, s.Anchor = t.session.account, id, scanTime(anchor)
	s.Adjustment, err = parseScheduleAdjustment(mode)
	return normalizeStoredSchedule(s), err
}

func (t *allowanceTransaction) SaveSchedule(ctx context.Context, in credit.Schedule, expectedRevision int64, now time.Time) error {
	s := t.session
	if err := s.scope(in.Account); err != nil {
		return err
	}
	if expectedRevision < 0 || expectedRevision == math.MaxInt64 || in.Revision != expectedRevision+1 {
		return billing.ErrConflict
	}
	in.Account = s.account
	raw, err := json.Marshal(in)
	if err != nil {
		return err
	}
	assignmentRaw, err := json.Marshal(in.Assignment)
	if err != nil {
		return err
	}
	var currentRevision int64
	err = s.tx.QueryRowContext(ctx, `SELECT revision FROM billing_allowance_schedules WHERE account_id=$1 AND schedule_id=$2 FOR UPDATE`, string(s.account), in.ID).Scan(&currentRevision)
	if errors.Is(err, sql.ErrNoRows) {
		if expectedRevision != 0 {
			return billing.ErrConflict
		}
		_, err = s.tx.ExecContext(ctx, `
			INSERT INTO billing_allowance_schedules
			(account_id,schedule_id,subscription_id,subscription_provider,subscription_merchant,subscription_environment,assignment_id,plan_version_id,assignment,anchor,state,state_effective_at,revision,previous_schedule_id,adjustment_mode,source_id,created_at,updated_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$17)`,
			string(s.account), in.ID, in.Subscription.ID, in.Subscription.Scope.Provider, in.Subscription.Scope.Merchant, in.Subscription.Scope.Environment, in.Assignment.ID, in.Assignment.PlanVersionID, assignmentRaw,
			nullTime(in.Anchor), scheduleStateString(in.State), in.StateEffectiveAt, in.Revision,
			nullString(in.PreviousScheduleID), scheduleAdjustmentString(in.Adjustment), in.SourceID, now)
		if err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else {
		if expectedRevision == 0 || currentRevision != expectedRevision {
			return billing.ErrConflict
		}
		_, err = s.tx.ExecContext(ctx, `
			UPDATE billing_allowance_schedules SET subscription_id=$3,subscription_provider=$4,subscription_merchant=$5,subscription_environment=$6,assignment_id=$7,plan_version_id=$8,assignment=$9,anchor=$10,state=$11,state_effective_at=$12,revision=$13,previous_schedule_id=$14,adjustment_mode=$15,source_id=$16,updated_at=$17
			WHERE account_id=$1 AND schedule_id=$2`,
			string(s.account), in.ID, in.Subscription.ID, in.Subscription.Scope.Provider, in.Subscription.Scope.Merchant, in.Subscription.Scope.Environment, in.Assignment.ID, in.Assignment.PlanVersionID, assignmentRaw,
			nullTime(in.Anchor), scheduleStateString(in.State), in.StateEffectiveAt, in.Revision,
			nullString(in.PreviousScheduleID), scheduleAdjustmentString(in.Adjustment), in.SourceID, now)
		if err != nil {
			return err
		}
	}
	_, err = s.tx.ExecContext(ctx, `INSERT INTO billing_allowance_schedule_history(account_id,schedule_id,revision,snapshot,source_id,recorded_at) VALUES($1,$2,$3,$4,$5,$6)`, string(s.account), in.ID, in.Revision, raw, in.SourceID, now)
	if err != nil {
		return err
	}
	if err := prepareAllowanceLineage(ctx, s.tx, s.account, in); err != nil {
		return err
	}
	return t.recordAllowanceChange(ctx, in.ID, in.StateEffectiveAt, "schedule")
}

var _ credit.AllowanceTx = (*allowanceTransaction)(nil)
