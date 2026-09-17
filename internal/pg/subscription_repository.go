package pg

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/integration"
	"github.com/data-insights-ai/rho-billing/subscription"
)

type subscriptionRepository struct {
	store   *Store
	session *session
}

func (s *Store) Subscriptions() subscription.Repository {
	return &subscriptionRepository{store: s}
}

func (s *session) Subscriptions() subscription.Repository {
	return &subscriptionRepository{store: s.store, session: s}
}

func (r *subscriptionRepository) WithinAccount(ctx context.Context, account billing.AccountID, fn func(subscription.Tx) error) error {
	if fn == nil {
		return billing.ErrInvalid
	}
	if r.session != nil {
		if err := r.session.scope(account); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return fn(&subscriptionTransaction{session: r.session})
	}
	return r.store.Atomic(ctx, account, func(v integration.Session) error {
		return fn(&subscriptionTransaction{session: v.(*session)})
	})
}

type subscriptionTransaction struct{ session *session }

func (t *subscriptionTransaction) Snapshot(ctx context.Context, ref billing.Reference) (subscription.Snapshot, error) {
	return readSubscription(ctx, t.session.tx, t.session.account, ref)
}

func (t *subscriptionTransaction) Event(ctx context.Context, ref billing.Reference, eventID string) (subscription.Event, bool, error) {
	var out subscription.Event
	var raw []byte
	err := t.session.tx.QueryRowContext(ctx, `SELECT occurred_at,fingerprint,result FROM billing_subscription_events WHERE account_id=$1 AND provider=$2 AND merchant=$3 AND environment=$4 AND external_id=$5 AND event_id=$6`, string(t.session.account), ref.Scope.Provider, ref.Scope.Merchant, ref.Scope.Environment, ref.ID, eventID).Scan(&out.OccurredAt, &out.Fingerprint, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return subscription.Event{}, false, nil
	}
	if err != nil {
		return subscription.Event{}, false, err
	}
	if err := json.Unmarshal(raw, &out.Result); err != nil {
		return subscription.Event{}, false, err
	}
	out.Ref, out.EventID = ref, eventID
	return out, true, nil
}

func (t *subscriptionTransaction) Owner(ctx context.Context, ref billing.Reference) (billing.AccountID, bool, error) {
	var owner string
	err := t.session.tx.QueryRowContext(ctx, `SELECT account_id FROM billing_subscriptions WHERE provider=$1 AND merchant=$2 AND environment=$3 AND external_id=$4`, ref.Scope.Provider, ref.Scope.Merchant, ref.Scope.Environment, ref.ID).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return billing.AccountID(owner), true, nil
}

func (t *subscriptionTransaction) PublishedPlans(ctx context.Context, ids []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := t.session.tx.QueryContext(ctx, `SELECT id FROM billing_plans WHERE id = ANY($1::text[])`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	found := 0
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		found++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if found != len(ids) {
		return billing.ErrNotFound
	}
	return nil
}

func (t *subscriptionTransaction) SaveSnapshot(ctx context.Context, in subscription.Snapshot) error {
	raw, err := json.Marshal(in)
	if err != nil {
		return err
	}
	ref := in.Ref
	if _, err = t.session.tx.ExecContext(ctx, `INSERT INTO billing_subscriptions(account_id,provider,merchant,environment,external_id,revision,snapshot) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(account_id,provider,merchant,environment,external_id) DO UPDATE SET revision=EXCLUDED.revision,snapshot=EXCLUDED.snapshot`, string(t.session.account), ref.Scope.Provider, ref.Scope.Merchant, ref.Scope.Environment, ref.ID, in.Revision, raw); err != nil {
		return err
	}
	_, err = t.session.tx.ExecContext(ctx, `INSERT INTO billing_subscription_history(account_id,provider,merchant,environment,external_id,revision,snapshot) VALUES($1,$2,$3,$4,$5,$6,$7)`, string(t.session.account), ref.Scope.Provider, ref.Scope.Merchant, ref.Scope.Environment, ref.ID, in.Revision, raw)
	return err
}

func (t *subscriptionTransaction) SaveEvent(ctx context.Context, in subscription.Event) error {
	raw, err := json.Marshal(in.Result)
	if err != nil {
		return err
	}
	ref := in.Ref
	_, err = t.session.tx.ExecContext(ctx, `INSERT INTO billing_subscription_events(account_id,provider,merchant,environment,external_id,event_id,occurred_at,fingerprint,result) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, string(t.session.account), ref.Scope.Provider, ref.Scope.Merchant, ref.Scope.Environment, ref.ID, in.EventID, billing.CanonicalTime(in.OccurredAt), in.Fingerprint, raw)
	return err
}

func readSubscription(ctx context.Context, q querier, account billing.AccountID, ref billing.Reference) (subscription.Snapshot, error) {
	return readJSON[subscription.Snapshot](ctx, q, `SELECT snapshot FROM billing_subscriptions WHERE account_id=$1 AND provider=$2 AND merchant=$3 AND environment=$4 AND external_id=$5`, string(account), ref.Scope.Provider, ref.Scope.Merchant, ref.Scope.Environment, ref.ID)
}

func (t *subscriptionTransaction) Lifecycle(ctx context.Context, id string) (subscription.Lifecycle, error) {
	return readJSON[subscription.Lifecycle](ctx, t.session.tx, `SELECT record FROM billing_subscription_lifecycles WHERE account_id=$1 AND lifecycle_id=$2`, string(t.session.account), id)
}

func (t *subscriptionTransaction) SaveLifecycle(ctx context.Context, in subscription.Lifecycle, expected int64) error {
	if err := t.session.scope(in.Account); err != nil {
		return err
	}
	if err := in.Validate(); err != nil {
		return err
	}
	if expected < 0 || in.Revision != expected+1 {
		return billing.ErrConflict
	}
	raw, err := json.Marshal(in)
	if err != nil || len(raw) > 1<<20 {
		return billing.ErrInvalid
	}
	fp := in.ID + ":" + strconv.FormatInt(in.Revision, 10)
	if expected == 0 {
		_, err = t.session.tx.ExecContext(ctx, `INSERT INTO billing_subscription_lifecycles(account_id,lifecycle_id,revision,record,fingerprint) VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, string(t.session.account), in.ID, in.Revision, raw, fp)
		if err != nil {
			return err
		}
		got, e := t.Lifecycle(ctx, in.ID)
		if errors.Is(e, billing.ErrNotFound) {
			return billing.ErrConflict
		}
		if e != nil {
			return e
		}
		if got.Revision != in.Revision || got.ID != in.ID {
			return billing.ErrConflict
		}
		return nil
	}
	res, err := t.session.tx.ExecContext(ctx, `UPDATE billing_subscription_lifecycles SET revision=$3,record=$4,fingerprint=$5 WHERE account_id=$1 AND lifecycle_id=$2 AND revision=$6`, string(t.session.account), in.ID, in.Revision, raw, fp, expected)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return billing.ErrConflict
	}
	return nil
}

func (t *subscriptionTransaction) Change(ctx context.Context, operation string) (subscription.ChangeRecord, bool, error) {
	got, err := readJSON[subscription.ChangeRecord](ctx, t.session.tx, `SELECT record FROM billing_subscription_changes WHERE account_id=$1 AND operation=$2`, string(t.session.account), operation)
	if errors.Is(err, billing.ErrNotFound) {
		return subscription.ChangeRecord{}, false, nil
	}
	if err != nil {
		return subscription.ChangeRecord{}, false, err
	}
	return got, true, nil
}

func (t *subscriptionTransaction) SaveChange(ctx context.Context, in subscription.ChangeRecord) error {
	if err := t.session.scope(in.Account); err != nil {
		return err
	}
	if err := in.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(in)
	if err != nil || len(raw) > 1<<20 {
		return billing.ErrInvalid
	}
	old, found, err := t.Change(ctx, in.Operation)
	if err != nil {
		return err
	}
	if !found {
		_, err = t.session.tx.ExecContext(ctx, `INSERT INTO billing_subscription_changes(account_id,operation,lifecycle_id,fingerprint,state,record) VALUES($1,$2,$3,$4,$5,$6)`, string(t.session.account), in.Operation, in.LifecycleID, in.Fingerprint, string(in.State), raw)
		return err
	}
	if old.Fingerprint != in.Fingerprint {
		return billing.ErrConflict
	}
	if old.State == in.State {
		return nil
	}
	if old.State == subscription.ChangePlanned && in.State == subscription.ChangeSuperseded {
		_, err = t.session.tx.ExecContext(ctx, `UPDATE billing_subscription_changes SET state=$3,record=$4 WHERE account_id=$1 AND operation=$2 AND state='planned'`, string(t.session.account), in.Operation, string(in.State), raw)
		return err
	}
	return billing.ErrConflict
}

var _ subscription.Repository = (*subscriptionRepository)(nil)
var _ subscription.Tx = (*subscriptionTransaction)(nil)
