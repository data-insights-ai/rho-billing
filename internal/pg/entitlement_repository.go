package pg

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/catalog"
	"github.com/data-insights-ai/rho-billing/integration"
)

const entitlementJSONLimit = 1 << 20

type entitlementRepository struct {
	store   *Store
	session *session
}

func (s *Store) Entitlements() catalog.EntitlementRepository {
	return &entitlementRepository{store: s}
}

func (s *session) Entitlements() catalog.EntitlementRepository {
	return &entitlementRepository{store: s.store, session: s}
}

func (r *entitlementRepository) WithinAccount(ctx context.Context, account billing.AccountID, fn func(catalog.EntitlementTx) error) error {
	if fn == nil {
		return billing.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.session != nil {
		if err := r.session.scope(account); err != nil {
			return err
		}
		return fn(&entitlementTx{session: r.session})
	}
	if r.store == nil {
		return billing.ErrInvalid
	}
	return r.store.Atomic(ctx, account, func(v integration.Session) error {
		s, ok := v.(*session)
		if !ok {
			return fmt.Errorf("entitlement: unexpected session type %T", v)
		}
		return fn(&entitlementTx{session: s})
	})
}

type entitlementTx struct{ session *session }

var _ catalog.EntitlementRepository = (*entitlementRepository)(nil)
var _ catalog.EntitlementTx = (*entitlementTx)(nil)

func (t *entitlementTx) Assignment(ctx context.Context, id string) (catalog.Assignment, error) {
	if !billing.ValidID(id) {
		return catalog.Assignment{}, billing.ErrInvalid
	}
	var out catalog.Assignment
	var raw []byte
	var fingerprint, planVersionID string
	err := t.session.tx.QueryRowContext(ctx, `
		SELECT assignment_id,plan_version_id,plan,source_ref,actor,reason,created_at,fingerprint
		FROM billing_entitlement_assignments
		WHERE account_id=$1 AND assignment_id=$2`, string(t.session.account), id).
		Scan(&out.Plan.ID, &planVersionID, &raw, &out.SourceRef, &out.Actor, &out.Reason, &out.CreatedAt, &fingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		return catalog.Assignment{}, billing.ErrNotFound
	}
	if err != nil {
		return catalog.Assignment{}, err
	}
	if len(raw) > entitlementJSONLimit {
		return catalog.Assignment{}, fmt.Errorf("entitlement assignment plan: %w", billing.ErrInvalid)
	}
	if err := json.Unmarshal(raw, &out.Plan); err != nil {
		return catalog.Assignment{}, err
	}
	out.Account = t.session.account
	out.CreatedAt = billing.CanonicalTime(out.CreatedAt)
	if out.Plan.ID != id || out.Plan.PlanVersionID != planVersionID {
		return catalog.Assignment{}, fmt.Errorf("entitlement assignment identity: %w", billing.ErrConflict)
	}
	if err := out.Validate(); err != nil {
		return catalog.Assignment{}, fmt.Errorf("entitlement assignment: %w", err)
	}
	if out.Fingerprint() != fingerprint {
		return catalog.Assignment{}, fmt.Errorf("entitlement assignment fingerprint: %w", billing.ErrConflict)
	}
	return out, nil
}

func (t *entitlementTx) InsertAssignment(ctx context.Context, in catalog.Assignment) error {
	if err := t.session.scope(in.Account); err != nil {
		return err
	}
	if err := in.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(in.Plan)
	if err != nil {
		return err
	}
	if len(raw) > entitlementJSONLimit {
		return fmt.Errorf("entitlement assignment plan: %w", billing.ErrInvalid)
	}
	_, err = t.session.tx.ExecContext(ctx, `
		INSERT INTO billing_entitlement_assignments
		(account_id,assignment_id,plan_version_id,plan,source_ref,actor,reason,created_at,fingerprint)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT DO NOTHING`,
		string(in.Account), in.Plan.ID, in.Plan.PlanVersionID, raw, in.SourceRef, in.Actor, in.Reason, in.CreatedAt, in.Fingerprint())
	if err != nil {
		return mapEntitlementConflict(err)
	}
	stored, err := t.Assignment(ctx, in.Plan.ID)
	if err != nil {
		return err
	}
	if stored.Fingerprint() != in.Fingerprint() {
		return billing.ErrConflict
	}
	return nil
}

func (t *entitlementTx) Plan(ctx context.Context, id string) (catalog.PlanVersion, error) {
	if !billing.ValidID(id) {
		return catalog.PlanVersion{}, billing.ErrInvalid
	}
	var raw []byte
	var fingerprint string
	var out catalog.PlanVersion
	err := t.session.tx.QueryRowContext(ctx, `
		SELECT id,definition,fingerprint FROM billing_plans WHERE id=$1`, id).
		Scan(&out.ID, &raw, &fingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		return catalog.PlanVersion{}, billing.ErrNotFound
	}
	if err != nil {
		return catalog.PlanVersion{}, err
	}
	if len(raw) > entitlementJSONLimit {
		return catalog.PlanVersion{}, fmt.Errorf("catalog plan definition: %w", billing.ErrInvalid)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return catalog.PlanVersion{}, err
	}
	if out.ID != id || !catalog.ValidPlan(out) || out.Fingerprint() != fingerprint {
		return catalog.PlanVersion{}, fmt.Errorf("catalog plan integrity: %w", billing.ErrConflict)
	}
	return out, nil
}

func mapEntitlementConflict(err error) error {
	if err == nil {
		return nil
	}
	if pgerr, ok := errors.AsType[*pgconn.PgError](err); ok && pgerr.Code == "23505" {
		return billing.ErrConflict
	}
	return err
}

// AssignmentsPage walks an account's assignments in assignment_id order.
//
// Keyset paging, not OFFSET: the walk runs inside the account transaction and
// must not miss or repeat a row if concurrent work inserts an assignment, and
// an offset shifts under exactly that.
func (t *entitlementTx) AssignmentsPage(ctx context.Context, after string, limit int) ([]catalog.Assignment, error) {
	if limit < 1 || limit > 1000 || (after != "" && !billing.ValidID(after)) {
		return nil, billing.ErrInvalid
	}
	rows, err := t.session.tx.QueryContext(ctx, `
		SELECT assignment_id,plan_version_id,plan,source_ref,actor,reason,created_at,fingerprint
		FROM billing_entitlement_assignments
		WHERE account_id=$1 AND assignment_id>$2
		ORDER BY assignment_id ASC LIMIT $3`, string(t.session.account), after, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]catalog.Assignment, 0, limit)
	for rows.Next() {
		var value catalog.Assignment
		var raw []byte
		var fingerprint, planVersionID string
		if err := rows.Scan(&value.Plan.ID, &planVersionID, &raw, &value.SourceRef, &value.Actor, &value.Reason, &value.CreatedAt, &fingerprint); err != nil {
			return nil, err
		}
		if len(raw) > entitlementJSONLimit {
			return nil, fmt.Errorf("entitlement assignment plan: %w", billing.ErrInvalid)
		}
		id := value.Plan.ID
		if err := json.Unmarshal(raw, &value.Plan); err != nil {
			return nil, err
		}
		value.Account = t.session.account
		value.CreatedAt = billing.CanonicalTime(value.CreatedAt)
		if value.Plan.ID != id || value.Plan.PlanVersionID != planVersionID {
			return nil, fmt.Errorf("entitlement assignment identity: %w", billing.ErrConflict)
		}
		if err := value.Validate(); err != nil {
			return nil, fmt.Errorf("entitlement assignment: %w", err)
		}
		if value.Fingerprint() != fingerprint {
			return nil, fmt.Errorf("entitlement assignment fingerprint: %w", billing.ErrConflict)
		}
		out = append(out, value)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
