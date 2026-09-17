package pg

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/catalog"
	"github.com/data-insights-ai/rho-billing/integration"
	"github.com/data-insights-ai/rho-billing/internal/identity"
	"github.com/data-insights-ai/rho-billing/usage"
)

func readJSON[T any](ctx context.Context, q querier, query string, args ...any) (T, error) {
	var out T
	var raw []byte
	err := q.QueryRowContext(ctx, query, args...).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return out, billing.ErrNotFound
	}
	if err != nil {
		return out, err
	}
	err = json.Unmarshal(raw, &out)
	return out, err
}
func (s *Store) Account(ctx context.Context, id billing.AccountID) (catalog.Account, error) {
	var a catalog.Account
	err := s.db.QueryRowContext(ctx, `SELECT account_id,subject FROM billing_accounts WHERE account_id=$1`, string(id)).Scan(&a.ID, &a.Subject)
	if errors.Is(err, sql.ErrNoRows) {
		err = billing.ErrNotFound
	}
	return a, err
}
func (s *Store) Link(ctx context.Context, r catalog.AccountReference) error {
	if !r.Valid() {
		return billing.ErrInvalid
	}
	return s.Atomic(ctx, r.Account, func(v integration.Session) error {
		t := v.(*session)
		res, err := t.tx.ExecContext(ctx, `INSERT INTO billing_provider_refs(account_id,provider,merchant,environment,kind,external_id) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, string(r.Account), r.Ref.Scope.Provider, r.Ref.Scope.Merchant, r.Ref.Scope.Environment, r.Kind, r.Ref.ID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 1 {
			return nil
		}
		var a string
		err = t.tx.QueryRowContext(ctx, `SELECT account_id FROM billing_provider_refs WHERE provider=$1 AND merchant=$2 AND environment=$3 AND kind=$4 AND external_id=$5`, r.Ref.Scope.Provider, r.Ref.Scope.Merchant, r.Ref.Scope.Environment, r.Kind, r.Ref.ID).Scan(&a)
		if err != nil {
			return err
		}
		if a != string(r.Account) {
			return billing.ErrConflict
		}
		return nil
	})
}
func (s *Store) Resolve(ctx context.Context, r catalog.AccountReference) (catalog.Account, error) {
	if !r.Valid() {
		return catalog.Account{}, billing.ErrInvalid
	}
	var a catalog.Account
	err := s.db.QueryRowContext(ctx, `SELECT a.account_id,a.subject FROM billing_accounts a JOIN billing_provider_refs r USING(account_id) WHERE r.account_id=$1 AND r.provider=$2 AND r.merchant=$3 AND r.environment=$4 AND r.kind=$5 AND r.external_id=$6`, string(r.Account), r.Ref.Scope.Provider, r.Ref.Scope.Merchant, r.Ref.Scope.Environment, r.Kind, r.Ref.ID).Scan(&a.ID, &a.Subject)
	if errors.Is(err, sql.ErrNoRows) {
		err = billing.ErrNotFound
	}
	return a, err
}

func (s *Store) PublishPlan(ctx context.Context, plan catalog.PlanVersion) error {
	registry := catalog.NewRegistry()
	if err := registry.PublishPlan(ctx, plan); err != nil {
		return err
	}
	p, err := registry.Plan(ctx, plan.ID)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, a := range p.Allowances {
		if err = publishUnit(ctx, tx, a.Unit); err != nil {
			return err
		}
	}
	for _, e := range p.Entitlements {
		if e.Kind == catalog.EntitlementLimit {
			if err = publishUnit(ctx, tx, e.Unit); err != nil {
				return err
			}
		}
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO billing_plans(id,plan_id,version,fingerprint,definition) VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, p.ID, p.PlanID, p.Version, p.Fingerprint(), raw)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		var fp string
		if err = tx.QueryRowContext(ctx, `SELECT fingerprint FROM billing_plans WHERE id=$1`, p.ID).Scan(&fp); errors.Is(err, sql.ErrNoRows) {
			return billing.ErrConflict
		} else if err != nil {
			return err
		}
		if fp != p.Fingerprint() {
			return billing.ErrConflict
		}
	}
	return tx.Commit()
}
func publishUnit(ctx context.Context, q querier, u billing.Unit) error {
	return ensureUnit(ctx, q, u)
}
func (s *Store) Plan(ctx context.Context, id string) (catalog.PlanVersion, error) {
	return readJSON[catalog.PlanVersion](ctx, s.db, `SELECT definition FROM billing_plans WHERE id=$1`, id)
}
func (s *Store) PutPriceMapping(ctx context.Context, m catalog.PriceMapping) error {
	// Reuse catalog validation and target semantics before database publication.
	p, err := s.Plan(ctx, m.Target.ID)
	if err != nil {
		return err
	}
	r := catalog.NewRegistry()
	if err = r.PublishPlan(ctx, p); err != nil {
		return err
	}
	probe := m
	probe.Revision = 1
	if err = r.PutPriceMapping(ctx, probe); err != nil {
		return err
	}
	if m.Revision < 1 {
		return billing.ErrInvalid
	}
	return s.Atomic(ctx, m.Scope.Account, func(v integration.Session) error {
		t := v.(*session)
		args := []any{string(m.Scope.Account), m.Scope.Provider, m.Scope.Merchant, m.Scope.Environment, m.ExternalPriceID}
		var latest int64
		if err := t.tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(revision),0) FROM billing_price_mappings WHERE account_id=$1 AND provider=$2 AND merchant=$3 AND environment=$4 AND price_id=$5`, args...).Scan(&latest); err != nil {
			return err
		}
		if m.Revision <= latest {
			old, err := mapping(ctx, t.tx, m.Scope, m.ExternalPriceID, m.Revision)
			if err != nil {
				return err
			}
			if old != m {
				return billing.ErrConflict
			}
			return nil
		}
		if latest == int64(^uint64(0)>>1) || m.Revision != latest+1 {
			return billing.ErrConflict
		}
		raw, err := json.Marshal(m)
		if err != nil {
			return err
		}
		_, err = t.tx.ExecContext(ctx, `INSERT INTO billing_price_mappings(account_id,provider,merchant,environment,price_id,revision,target_id,definition) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, append(args, m.Revision, m.Target.ID, raw)...)
		return err
	})
}
func mapping(ctx context.Context, q querier, scope catalog.MappingScope, id string, revision int64) (catalog.PriceMapping, error) {
	return readJSON[catalog.PriceMapping](ctx, q, `SELECT definition FROM billing_price_mappings WHERE account_id=$1 AND provider=$2 AND merchant=$3 AND environment=$4 AND price_id=$5 AND ($6::bigint=0 OR revision=$6) ORDER BY revision DESC LIMIT 1`, string(scope.Account), scope.Provider, scope.Merchant, scope.Environment, id, revision)
}
func (s *Store) PriceMapping(ctx context.Context, scope catalog.MappingScope, id string) (catalog.PriceMapping, error) {
	return mapping(ctx, s.db, scope, id, 0)
}
func (s *Store) PriceMappingAtRevision(ctx context.Context, scope catalog.MappingScope, id string, rev int64) (catalog.PriceMapping, error) {
	if rev <= 0 {
		return catalog.PriceMapping{}, billing.ErrInvalid
	}
	return mapping(ctx, s.db, scope, id, rev)
}

// Equivalent inputs are normalized by JSON map ordering; the exact definition is retained.
func (s *Store) PublishRating(ctx context.Context, c usage.RuleConfig) error {
	if _, err := usage.NewRule(c); err != nil {
		return err
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	fp := identity.Fingerprint(string(raw))
	res, err := s.db.ExecContext(ctx, `INSERT INTO billing_rating_rules(version,fingerprint,definition) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, c.Version, fp, raw)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 1 {
		return nil
	}
	var old string
	if err = s.db.QueryRowContext(ctx, `SELECT fingerprint FROM billing_rating_rules WHERE version=$1`, c.Version).Scan(&old); err != nil {
		return err
	}
	if old != fp {
		return billing.ErrConflict
	}
	return nil
}
func (s *Store) Rating(ctx context.Context, version string) (*usage.Rule, error) {
	c, err := readJSON[usage.RuleConfig](ctx, s.db, `SELECT definition FROM billing_rating_rules WHERE version=$1`, version)
	if err != nil {
		return nil, err
	}
	return usage.NewRule(c)
}

var _ catalog.Repository = (*Store)(nil)
var _ catalog.AccountRepository = (*Store)(nil)
