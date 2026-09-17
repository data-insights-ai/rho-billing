package pg

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/integration"
	"github.com/data-insights-ai/rho-billing/purchase"
)

// JSONB wire formatting may be larger than the canonical 1 MiB domain record.
const purchaseJSONLimit = 2 << 20

type purchaseRepository struct {
	store   *Store
	session *session
}

func (s *Store) Purchases() purchase.Repository { return &purchaseRepository{store: s} }

func (s *session) Purchases() purchase.Repository {
	return &purchaseRepository{store: s.store, session: s}
}

func (r *purchaseRepository) WithinAccount(ctx context.Context, account billing.AccountID, fn func(purchase.Tx) error) error {
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
		return fn(&purchaseTx{session: r.session})
	}
	if r.store == nil {
		return billing.ErrInvalid
	}
	return r.store.Atomic(ctx, account, func(v integration.Session) error {
		s, ok := v.(*session)
		if !ok {
			return fmt.Errorf("purchase: unexpected session type %T", v)
		}
		return fn(&purchaseTx{session: s})
	})
}

type purchaseTx struct{ session *session }

var _ purchase.Repository = (*purchaseRepository)(nil)
var _ purchase.Tx = (*purchaseTx)(nil)

func (t *purchaseTx) Offer(ctx context.Context, ref purchase.Revision) (purchase.Offer, error) {
	if err := validRevision(ref); err != nil {
		return purchase.Offer{}, err
	}
	var raw []byte
	var out purchase.Offer
	var fingerprint string
	err := t.session.tx.QueryRowContext(ctx, `
		SELECT name,effects,published_at,fingerprint
		FROM billing_purchase_offers
		WHERE account_id=$1 AND offer_id=$2 AND offer_version=$3`,
		string(t.session.account), ref.ID, ref.Version).Scan(&out.Name, &raw, &out.PublishedAt, &fingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		return purchase.Offer{}, billing.ErrNotFound
	}
	if err != nil {
		return purchase.Offer{}, err
	}
	if len(raw) > purchaseJSONLimit {
		return purchase.Offer{}, fmt.Errorf("purchase offer effects: %w", billing.ErrInvalid)
	}
	if err := json.Unmarshal(raw, &out.Effects); err != nil {
		return purchase.Offer{}, err
	}
	out.Account = t.session.account
	out.Revision = ref
	if err := out.Validate(); err != nil {
		return purchase.Offer{}, fmt.Errorf("purchase offer: %w", err)
	}
	if out.Fingerprint() != fingerprint {
		return purchase.Offer{}, fmt.Errorf("purchase offer fingerprint: %w", billing.ErrConflict)
	}
	return out, nil
}

func (t *purchaseTx) InsertOffer(ctx context.Context, in purchase.Offer) error {
	if err := t.session.scope(in.Account); err != nil {
		return err
	}
	if err := in.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(in.Effects)
	if err != nil {
		return err
	}
	if len(raw) > purchaseJSONLimit {
		return fmt.Errorf("purchase offer effects: %w", billing.ErrInvalid)
	}
	_, err = t.session.tx.ExecContext(ctx, `
		INSERT INTO billing_purchase_offers(account_id,offer_id,offer_version,name,effects,published_at,fingerprint)
		VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT DO NOTHING`,
		string(in.Account), in.Revision.ID, in.Revision.Version, in.Name, raw, in.PublishedAt, in.Fingerprint())
	if err != nil {
		return mapPurchaseConflict(err)
	}
	stored, err := t.Offer(ctx, in.Revision)
	if err != nil {
		return err
	}
	if stored.Fingerprint() != in.Fingerprint() {
		return billing.ErrConflict
	}
	return nil
}

func (t *purchaseTx) Price(ctx context.Context, ref purchase.Revision) (purchase.Price, error) {
	if err := validRevision(ref); err != nil {
		return purchase.Price{}, err
	}
	var out purchase.Price
	var fingerprint string
	err := t.session.tx.QueryRowContext(ctx, `
		SELECT price_id,price_version,offer_id,offer_version,currency,unit_amount,tax_treatment,published_at,fingerprint
		FROM billing_purchase_prices
		WHERE account_id=$1 AND price_id=$2 AND price_version=$3`, string(t.session.account), ref.ID, ref.Version).
		Scan(&out.Revision.ID, &out.Revision.Version, &out.Offer.ID, &out.Offer.Version, &out.Currency, &out.UnitAmount, &out.TaxTreatment, &out.PublishedAt, &fingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		return purchase.Price{}, billing.ErrNotFound
	}
	if err != nil {
		return purchase.Price{}, err
	}
	out.Account = t.session.account
	if err := out.Validate(); err != nil {
		return purchase.Price{}, fmt.Errorf("purchase price: %w", err)
	}
	if out.Fingerprint() != fingerprint {
		return purchase.Price{}, fmt.Errorf("purchase price fingerprint: %w", billing.ErrConflict)
	}
	return out, nil
}

func (t *purchaseTx) InsertPrice(ctx context.Context, in purchase.Price) error {
	if err := t.session.scope(in.Account); err != nil {
		return err
	}
	if err := in.Validate(); err != nil {
		return err
	}
	_, err := t.session.tx.ExecContext(ctx, `
		INSERT INTO billing_purchase_prices(account_id,price_id,price_version,offer_id,offer_version,currency,unit_amount,tax_treatment,published_at,fingerprint)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT DO NOTHING`,
		string(in.Account), in.Revision.ID, in.Revision.Version, in.Offer.ID, in.Offer.Version, in.Currency, in.UnitAmount, in.TaxTreatment, in.PublishedAt, in.Fingerprint())
	if err != nil {
		return mapPurchaseConflict(err)
	}
	stored, err := t.Price(ctx, in.Revision)
	if err != nil {
		return err
	}
	if stored.Fingerprint() != in.Fingerprint() {
		return billing.ErrConflict
	}
	return nil
}

func (t *purchaseTx) Quote(ctx context.Context, id string) (purchase.Quote, error) {
	if !billing.ValidID(id) {
		return purchase.Quote{}, billing.ErrInvalid
	}
	var out purchase.Quote
	var raw []byte
	var fingerprint string
	err := t.session.tx.QueryRowContext(ctx, `
		SELECT quote_id,currency,tax_treatment,amount,created_at,valid_until,lines,fingerprint
		FROM billing_purchase_quotes WHERE account_id=$1 AND quote_id=$2`, string(t.session.account), id).
		Scan(&out.ID, &out.Currency, &out.TaxTreatment, &out.Amount, &out.CreatedAt, &out.ValidUntil, &raw, &fingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		return purchase.Quote{}, billing.ErrNotFound
	}
	if err != nil {
		return purchase.Quote{}, err
	}
	if len(raw) > purchaseJSONLimit {
		return purchase.Quote{}, fmt.Errorf("purchase quote lines: %w", billing.ErrInvalid)
	}
	if err := json.Unmarshal(raw, &out.Lines); err != nil {
		return purchase.Quote{}, err
	}
	out.Account = t.session.account
	if err := out.Validate(); err != nil {
		return purchase.Quote{}, err
	}

	if out.Fingerprint() != fingerprint {
		return purchase.Quote{}, fmt.Errorf("purchase quote fingerprint: %w", billing.ErrConflict)
	}
	return out, nil
}

func (t *purchaseTx) InsertQuote(ctx context.Context, in purchase.Quote) error {
	if err := t.session.scope(in.Account); err != nil {
		return err
	}
	if err := in.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(in.Lines)
	if err != nil {
		return err
	}
	if len(raw) > purchaseJSONLimit {
		return fmt.Errorf("purchase quote lines: %w", billing.ErrInvalid)
	}
	_, err = t.session.tx.ExecContext(ctx, `
		INSERT INTO billing_purchase_quotes(account_id,quote_id,currency,tax_treatment,amount,created_at,valid_until,lines,fingerprint)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT DO NOTHING`,
		string(in.Account), in.ID, in.Currency, in.TaxTreatment, in.Amount, in.CreatedAt, in.ValidUntil, raw, in.Fingerprint())
	if err != nil {
		return mapPurchaseConflict(err)
	}
	stored, err := t.Quote(ctx, in.ID)
	if err != nil {
		return err
	}
	if stored.Fingerprint() != in.Fingerprint() {
		return billing.ErrConflict
	}
	return nil
}

func validRevision(ref purchase.Revision) error {
	if !billing.ValidID(ref.ID) || ref.Version <= 0 {
		return billing.ErrInvalid
	}
	return nil
}

func mapPurchaseConflict(err error) error {
	if err == nil {
		return nil
	}
	if pgerr, ok := errors.AsType[*pgconn.PgError](err); ok && pgerr.Code == "23505" {
		return billing.ErrConflict
	}
	return err
}
