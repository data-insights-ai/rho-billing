package pg

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/integration"
	"github.com/data-insights-ai/rho-billing/subscription"
	"github.com/data-insights-ai/rho-billing/usage"
)

type usageRepository struct {
	store   *Store
	session *session
}

func (s *Store) UsageRepository() usage.Repository {
	return &usageRepository{store: s}
}

func (s *session) Usage() usage.Repository {
	return &usageRepository{store: s.store, session: s}
}

func (r *usageRepository) WithinAccount(ctx context.Context, account billing.AccountID, fn func(usage.Tx) error) error {
	if fn == nil || !billing.ValidID(string(account)) {
		return billing.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.session != nil {
		if err := r.session.scope(account); err != nil {
			return err
		}
		return fn(&usageTransaction{session: r.session})
	}
	return r.store.Atomic(ctx, account, func(v integration.Session) error {
		return fn(&usageTransaction{session: v.(*session)})
	})
}

type usageTransaction struct{ session *session }

func (t *usageTransaction) Record(ctx context.Context, id string) (usage.Record, error) {
	return readJSON[usage.Record](ctx, t.session.tx, `SELECT record FROM billing_usage WHERE account_id=$1 AND usage_id=$2`, string(t.session.account), id)
}

func (t *usageTransaction) Rule(ctx context.Context, version string) (*usage.Rule, error) {
	config, err := readJSON[usage.RuleConfig](ctx, t.session.tx, `SELECT definition FROM billing_rating_rules WHERE version=$1`, version)
	if err != nil {
		return nil, err
	}
	return usage.NewRule(config)
}

func (t *usageTransaction) Subscription(ctx context.Context, ref billing.Reference) (subscription.Snapshot, error) {
	return readSubscription(ctx, t.session.tx, t.session.account, ref)
}

func (t *usageTransaction) Reservation(ctx context.Context, id string) (credit.Reservation, error) {
	var reservation credit.Reservation
	var evidence []byte
	err := t.session.tx.QueryRowContext(ctx, `SELECT reservation_id,actor,unit,scope,state,consumed,evidence FROM billing_reservations WHERE account_id=$1 AND reservation_id=$2`, string(t.session.account), id).Scan(&reservation.ID, &reservation.Actor, &reservation.Unit, &reservation.Scope, &reservation.State, &reservation.Consumed, &evidence)
	if errors.Is(err, sql.ErrNoRows) {
		return credit.Reservation{}, billing.ErrNotFound
	}
	if err != nil {
		return credit.Reservation{}, err
	}
	if err := json.Unmarshal(evidence, &reservation.Evidence); err != nil {
		return credit.Reservation{}, err
	}
	return reservation, nil
}

func (t *usageTransaction) Candidates(ctx context.Context, observation usage.Observation, after string, limit int) ([]usage.Record, error) {
	if limit < 1 || limit > 1000 || !observation.Scope.Valid() {
		return nil, billing.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := t.session.scope(observation.Account); err != nil {
		return nil, err
	}
	var (
		query string
		args  []any
	)
	common := []any{
		string(t.session.account), observation.Source,
		observation.Scope.Subscription.Scope.Provider, observation.Scope.Subscription.Scope.Merchant, observation.Scope.Subscription.Scope.Environment,
		observation.Scope.Subscription.ID, observation.Scope.ItemID, after,
	}
	if observation.Interval.Valid() {
		query = `
			SELECT record FROM billing_usage
			WHERE account_id=$1 AND source=$2
			  AND scope_provider=$3 AND scope_merchant=$4 AND scope_environment=$5
			  AND scope_subscription_id=$6 AND scope_item_id=$7
			  AND usage_id COLLATE "C" > $8
			  AND (
				(interval_start IS NOT NULL AND interval_start < $10 AND interval_end > $9)
				OR (interval_start IS NULL AND occurred_at >= $9 AND occurred_at < $10)
			  )
			ORDER BY usage_id COLLATE "C"
			LIMIT $11`
		args = append(common, observation.Interval.Start, observation.Interval.End, limit)
	} else {
		query = `
			SELECT record FROM billing_usage
			WHERE account_id=$1 AND source=$2
			  AND scope_provider=$3 AND scope_merchant=$4 AND scope_environment=$5
			  AND scope_subscription_id=$6 AND scope_item_id=$7
			  AND usage_id COLLATE "C" > $8
			  AND interval_start IS NOT NULL AND interval_start <= $9 AND interval_end > $9
			ORDER BY usage_id COLLATE "C"
			LIMIT $10`
		args = append(common, observation.OccurredAt, limit)
	}
	rows, err := t.session.tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]usage.Record, 0, limit)
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var record usage.Record
		if err := json.Unmarshal(raw, &record); err != nil {
			return nil, err
		}
		result = append(result, usage.Copy(record))
	}
	return result, rows.Err()
}

func (t *usageTransaction) Save(ctx context.Context, record usage.Record) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := t.session.scope(record.Observation.Account); err != nil {
		return err
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	o := record.Observation
	_, err = t.session.tx.ExecContext(ctx, `INSERT INTO billing_usage(account_id,usage_id,source,occurred_at,interval_start,interval_end,funding,scope_provider,scope_merchant,scope_environment,scope_subscription_id,scope_item_id,fingerprint,record) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`, string(t.session.account), o.ID, o.Source, o.OccurredAt, nullTime(o.Interval.Start), nullTime(o.Interval.End), string(o.Funding), o.Scope.Subscription.Scope.Provider, o.Scope.Subscription.Scope.Merchant, o.Scope.Subscription.Scope.Environment, o.Scope.Subscription.ID, o.Scope.ItemID, record.Fingerprint, raw)
	return err
}

func (t *usageTransaction) Page(ctx context.Context, period billing.Period, after string, limit int) ([]usage.Record, error) {
	if !period.Valid() || limit < 1 || limit > 1000 {
		return nil, billing.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := t.session.tx.QueryContext(ctx, `SELECT record FROM billing_usage WHERE account_id=$1 AND occurred_at >= $2 AND occurred_at < $3 AND usage_id COLLATE "C" > $4 ORDER BY usage_id COLLATE "C" LIMIT $5`, string(t.session.account), databaseTime(period.Start), databaseTime(period.End), after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]usage.Record, 0, limit)
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var record usage.Record
		if err := json.Unmarshal(raw, &record); err != nil {
			return nil, err
		}
		result = append(result, usage.Copy(record))
	}
	return result, rows.Err()
}

func (t *usageTransaction) Cost(ctx context.Context, id string) (usage.CostRecord, error) {
	return readJSON[usage.CostRecord](ctx, t.session.tx, `SELECT record FROM billing_internal_costs WHERE account_id=$1 AND cost_id=$2`, string(t.session.account), id)
}

func (t *usageTransaction) SaveCost(ctx context.Context, record usage.CostRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := t.session.scope(record.Account); err != nil {
		return err
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	_, err = t.session.tx.ExecContext(ctx, `INSERT INTO billing_internal_costs(account_id,cost_id,usage_id,resource,model,quantity,rule_version,currency,amount,exact_amount,state,corrects_id,missing_reason,source_currency,occurred_at,recorded_at,fingerprint,record) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`,
		string(t.session.account), record.ID, record.UsageID, record.Resource, record.Model, record.Quantity, record.RuleVersion, record.Currency, record.Amount, record.ExactAmount, string(record.State), record.CorrectsID, record.MissingReason, record.SourceCurrency, record.OccurredAt, record.RecordedAt, record.Fingerprint, raw)
	if err == nil {
		return nil
	}
	if pgerr, ok := errors.AsType[*pgconn.PgError](err); ok && pgerr.Code == "23505" {
		return billing.ErrConflict
	}
	return err
}

var _ usage.Repository = (*usageRepository)(nil)
var _ usage.Tx = (*usageTransaction)(nil)
