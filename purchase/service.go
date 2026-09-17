package purchase

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/internal/checked"
)

type Service struct {
	repo Repository
	now  func() time.Time
}

func New(repo Repository, now func() time.Time) *Service {
	if repo == nil {
		panic("purchase: nil repository")
	}
	if now == nil {
		now = time.Now
	}
	return &Service{repo: repo, now: now}
}

func (s *Service) PublishOffer(ctx context.Context, offer Offer) (Offer, error) {
	if s.repo == nil || s.now == nil {
		return Offer{}, billing.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return Offer{}, err
	}
	offer.PublishedAt = billing.CanonicalTime(s.now())
	if offer.PublishedAt.IsZero() {
		return Offer{}, billing.ErrInvalid
	}
	if err := offer.Validate(); err != nil {
		return Offer{}, err
	}
	var normalizeErr error
	offer, normalizeErr = normalizeOffer(offer)
	if normalizeErr != nil {
		return Offer{}, normalizeErr
	}
	if err := offer.Validate(); err != nil {
		return Offer{}, err
	}
	offer = cloneOffer(offer)
	var result Offer
	err := s.repo.WithinAccount(ctx, offer.Account, func(tx Tx) error {
		existing, err := tx.Offer(ctx, offer.Revision)
		if err == nil {
			if existing.Validate() != nil || existing.Account != offer.Account || existing.Revision != offer.Revision {
				return billing.ErrState
			}
			existing, err = normalizeOffer(existing)
			if err != nil {
				return err
			}
			if existing.Fingerprint() != offer.Fingerprint() {
				return billing.ErrConflict
			}
			result = cloneOffer(existing)
			return nil
		}
		if !errors.Is(err, billing.ErrNotFound) {
			return err
		}
		if err := tx.InsertOffer(ctx, cloneOffer(offer)); err != nil {
			return err
		}
		result = cloneOffer(offer)
		return nil
	})
	if err != nil {
		return Offer{}, err
	}
	return result, nil
}

func (s *Service) PublishPrice(ctx context.Context, price Price) (Price, error) {
	if s.repo == nil || s.now == nil {
		return Price{}, billing.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return Price{}, err
	}
	price.PublishedAt = billing.CanonicalTime(s.now())
	if price.PublishedAt.IsZero() {
		return Price{}, billing.ErrInvalid
	}
	if err := price.Validate(); err != nil {
		return Price{}, err
	}
	var result Price
	err := s.repo.WithinAccount(ctx, price.Account, func(tx Tx) error {
		existing, err := tx.Price(ctx, price.Revision)
		if err == nil {
			if existing.Validate() != nil || existing.Account != price.Account || existing.Revision != price.Revision {
				return billing.ErrState
			}
			existing.PublishedAt = billing.CanonicalTime(existing.PublishedAt)
			if existing.Fingerprint() != price.Fingerprint() {
				return billing.ErrConflict
			}
			result = existing
			return nil
		}
		if !errors.Is(err, billing.ErrNotFound) {
			return err
		}
		bound, err := tx.Offer(ctx, price.Offer)
		if err != nil {
			return err
		}
		if bound.Account != price.Account || bound.Revision != price.Offer || bound.Validate() != nil {
			return billing.ErrConflict
		}
		// A settlement effect is only deliverable against a tax-exclusive
		// price. Enforcing that here keeps the combination from being
		// publishable at all; discovering it during fulfilment would abort the
		// payment transaction after the money was collected, discarding the
		// funding record and poisoning every redelivery of that webhook.
		for _, effect := range bound.Effects {
			if effect.Settlement != nil && price.TaxTreatment != TaxExclusive {
				return billing.ErrInvalid
			}
		}
		if err := tx.InsertPrice(ctx, price); err != nil {
			return err
		}
		result = price
		return nil
	})
	if err != nil {
		return Price{}, err
	}
	return result, nil
}

func (s *Service) Offer(ctx context.Context, account billing.AccountID, revision Revision) (Offer, error) {
	if err := ctx.Err(); err != nil {
		return Offer{}, err
	}
	if s.repo == nil || !billing.ValidID(string(account)) || !validRevision(revision) {
		return Offer{}, billing.ErrInvalid
	}
	var out Offer
	err := s.repo.WithinAccount(ctx, account, func(tx Tx) error {
		v, err := tx.Offer(ctx, revision)
		if err != nil {
			return err
		}
		if v.Account != account || v.Revision != revision || v.Validate() != nil {
			return billing.ErrState
		}
		out, err = normalizeOffer(v)
		return err
	})
	if err != nil {
		return Offer{}, err
	}
	return out, nil
}

func (s *Service) Price(ctx context.Context, account billing.AccountID, revision Revision) (Price, error) {
	if err := ctx.Err(); err != nil {
		return Price{}, err
	}
	if s.repo == nil || !billing.ValidID(string(account)) || !validRevision(revision) {
		return Price{}, billing.ErrInvalid
	}
	var out Price
	err := s.repo.WithinAccount(ctx, account, func(tx Tx) error {
		v, err := tx.Price(ctx, revision)
		if err != nil {
			return err
		}
		if v.Account != account || v.Revision != revision || v.Validate() != nil {
			return billing.ErrState
		}
		v.PublishedAt = billing.CanonicalTime(v.PublishedAt)
		out = v
		return nil
	})
	if err != nil {
		return Price{}, err
	}
	return out, nil
}

// Quote remains readable after expiry.
func (s *Service) Quote(ctx context.Context, account billing.AccountID, id string) (Quote, error) {
	if err := ctx.Err(); err != nil {
		return Quote{}, err
	}
	if s.repo == nil || !billing.ValidID(string(account)) || !billing.ValidID(id) {
		return Quote{}, billing.ErrInvalid
	}
	var out Quote
	err := s.repo.WithinAccount(ctx, account, func(tx Tx) error {
		v, err := tx.Quote(ctx, id)
		if err != nil {
			return err
		}
		if v.Account != account || v.ID != id || v.Validate() != nil {
			return billing.ErrState
		}
		out, err = normalizeQuote(v)
		return err
	})
	if err != nil {
		return Quote{}, err
	}
	return out, nil
}

func (s *Service) CreateQuote(ctx context.Context, in QuoteInput) (Quote, error) {
	if s.repo == nil || s.now == nil {
		return Quote{}, billing.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return Quote{}, err
	}
	if len(in.Lines) > maxLines {
		return Quote{}, billing.ErrInvalid
	}
	in = cloneQuoteInput(in)
	in.ValidUntil = billing.CanonicalTime(in.ValidUntil)
	now := billing.CanonicalTime(s.now())
	if now.IsZero() {
		return Quote{}, billing.ErrInvalid
	}
	if err := in.Validate(now); err != nil {
		if !billing.ValidID(string(in.Account)) || !billing.ValidID(in.ID) {
			return Quote{}, err
		}
	}
	var result Quote
	err := s.repo.WithinAccount(ctx, in.Account, func(tx Tx) error {
		existing, err := tx.Quote(ctx, in.ID)
		if err == nil {
			if existing.Validate() != nil || existing.Account != in.Account || existing.ID != in.ID {
				return billing.ErrState
			}
			existing, err = normalizeQuote(existing)
			if err != nil {
				return err
			}
			if quoteMatchesInput(existing, in) {
				result = cloneQuote(existing)
				return nil
			}
			return billing.ErrConflict
		}
		if !errors.Is(err, billing.ErrNotFound) {
			return err
		}
		if err := in.Validate(now); err != nil {
			return err
		}
		q, err := s.buildQuote(ctx, tx, in, now)
		if err != nil {
			return err
		}
		if err := tx.InsertQuote(ctx, cloneQuote(q)); err != nil {
			return err
		}
		result = cloneQuote(q)
		return nil
	})
	if err != nil {
		return Quote{}, err
	}
	return result, nil
}

func quoteMatchesInput(q Quote, in QuoteInput) bool {
	if q.Account != in.Account || q.ID != in.ID || !q.ValidUntil.Equal(billing.CanonicalTime(in.ValidUntil)) || len(q.Lines) != len(in.Lines) {
		return false
	}
	for i, line := range in.Lines {
		got := q.Lines[i]
		if got.ID != line.ID || got.Price != line.Price || got.Quantity != line.Quantity || got.SettlementBatchID != line.SettlementBatchID {
			return false
		}
	}
	return true
}

func (s *Service) buildQuote(ctx context.Context, tx Tx, in QuoteInput, now time.Time) (Quote, error) {
	q := Quote{Account: in.Account, ID: in.ID, CreatedAt: now, ValidUntil: billing.CanonicalTime(in.ValidUntil)}
	q.Lines = make([]QuoteLine, len(in.Lines))
	settlementBatches := make(map[string]struct{})
	serializedSize := 0
	for i, input := range in.Lines {
		price, err := tx.Price(ctx, input.Price)
		if err != nil {
			return Quote{}, err
		}
		offer, err := tx.Offer(ctx, price.Offer)
		if err != nil {
			return Quote{}, err
		}
		if err := price.Validate(); err != nil || price.Revision != input.Price {
			return Quote{}, billing.ErrState
		}
		if err := offer.Validate(); err != nil || offer.Revision != price.Offer || price.Account != in.Account || offer.Account != in.Account {
			return Quote{}, billing.ErrConflict
		}
		amount, err := mul(price.UnitAmount, input.Quantity)
		if err != nil {
			return Quote{}, err
		}
		if i == 0 {
			q.Currency, q.TaxTreatment = price.Currency, price.TaxTreatment
		} else if q.Currency != price.Currency || q.TaxTreatment != price.TaxTreatment {
			return Quote{}, billing.ErrConflict
		}
		needsBatch := false
		for _, e := range offer.Effects {
			if e.Credit != nil {
				if _, err := mul(e.Credit.Amount, input.Quantity); err != nil {
					return Quote{}, err
				}
			}
			if e.Plan != nil {
				if _, err := mul(e.Plan.Quantity, input.Quantity); err != nil {
					return Quote{}, err
				}
			}
			if e.Settlement != nil {
				needsBatch = true
			}
		}
		if needsBatch != (input.SettlementBatchID != "") {
			return Quote{}, billing.ErrInvalid
		}
		if needsBatch {
			if input.Quantity != 1 || !billing.ValidID(input.SettlementBatchID) {
				return Quote{}, billing.ErrInvalid
			}
			if _, exists := settlementBatches[input.SettlementBatchID]; exists {
				return Quote{}, billing.ErrConflict
			}
			settlementBatches[input.SettlementBatchID] = struct{}{}
		}
		offer, err = normalizeOffer(offer)
		if err != nil {
			return Quote{}, err
		}
		price.PublishedAt = billing.CanonicalTime(price.PublishedAt)
		line := QuoteLine{QuoteLineInput: input, Offer: offer, PriceSnapshot: price, Amount: amount}
		raw, err := json.Marshal(line)
		if err != nil {
			return Quote{}, err
		}
		serializedSize += len(raw)
		if serializedSize > maxQuoteBytes {
			return Quote{}, billing.ErrInvalid
		}
		q.Lines[i] = line
		q.Amount, err = checked.Add(q.Amount, amount)
		if err != nil {
			return Quote{}, err
		}
	}
	if len(q.Lines) == 0 || effectCount(q) > maxQuoteEffects {
		return Quote{}, billing.ErrInvalid
	}
	serialized, err := json.Marshal(q)
	if err != nil {
		return Quote{}, err
	}
	if len(serialized) > maxQuoteBytes {
		return Quote{}, billing.ErrInvalid
	}
	if err := q.Validate(); err != nil {
		return Quote{}, err
	}
	return q, nil
}

func effectCount(q Quote) int {
	n := 0
	for _, l := range q.Lines {
		n += len(l.Offer.Effects)
	}
	return n
}

func cloneQuoteInput(in QuoteInput) QuoteInput {
	out := in
	out.Lines = append([]QuoteLineInput(nil), in.Lines...)
	return out
}
