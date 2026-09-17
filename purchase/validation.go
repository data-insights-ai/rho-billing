package purchase

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"strings"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/internal/canonicaljson"
	"github.com/data-insights-ai/rho-billing/internal/checked"
)

const (
	maxEffects      = 100
	maxLines        = 100
	maxQuoteEffects = 1000
	maxQuoteBytes   = 1 << 20
	maxPayloadBytes = 64 << 10
)

func validRevision(r Revision) bool { return billing.ValidID(r.ID) && r.Version > 0 }

func validateEffect(e Effect) error {
	if !billing.ValidID(e.Key) {
		return billing.ErrInvalid
	}
	count := 0
	if e.Credit != nil {
		count++
		if !e.Credit.Unit.Valid() || e.Credit.Amount <= 0 || e.Credit.Validity < 0 || (e.Credit.Scope != "" && !billing.ValidID(e.Credit.Scope)) {
			return billing.ErrInvalid
		}
	}
	if e.Plan != nil {
		count++
		if !billing.ValidID(e.Plan.PlanVersionID) || e.Plan.Quantity <= 0 || e.Plan.Validity < 0 {
			return billing.ErrInvalid
		}
	}
	if e.Host != nil {
		count++
		if !billing.ValidID(e.Host.Kind) || !canonicalObject(e.Host.Payload) {
			return billing.ErrInvalid
		}
	}
	if e.Settlement != nil {
		count++
	}
	if count != 1 {
		return billing.ErrInvalid
	}
	return nil
}

func validateOffer(o Offer) error {
	if !billing.ValidID(string(o.Account)) || !validRevision(o.Revision) || !billing.ValidID(o.Name) || len(o.Effects) > maxEffects {
		return billing.ErrInvalid
	}
	seen := make(map[string]struct{}, len(o.Effects))
	settlements := 0
	for _, e := range o.Effects {
		if !billing.ValidID(e.Key) {
			return billing.ErrInvalid
		}
		if _, ok := seen[e.Key]; ok {
			return billing.ErrConflict
		}
		seen[e.Key] = struct{}{}
		if err := validateEffect(e); err != nil {
			return err
		}
		if e.Settlement != nil {
			settlements++
		}
	}
	if settlements > 1 {
		return billing.ErrConflict
	}
	if o.PublishedAt.IsZero() {
		return billing.ErrInvalid
	}
	encoded, err := json.Marshal(o)
	if err != nil || len(encoded) > maxQuoteBytes {
		return billing.ErrInvalid
	}
	return nil
}

func (o Offer) Validate() error { return validateOffer(o) }

func validatePrice(p Price) error {
	if !billing.ValidID(string(p.Account)) || !validRevision(p.Revision) || !validRevision(p.Offer) || p.UnitAmount < 0 || len(p.Currency) != 3 || p.Currency != strings.ToUpper(p.Currency) || strings.IndexFunc(p.Currency, func(r rune) bool { return r < 'A' || r > 'Z' }) >= 0 {
		return billing.ErrInvalid
	}
	if p.TaxTreatment != TaxInclusive && p.TaxTreatment != TaxExclusive {
		return billing.ErrInvalid
	}
	if p.PublishedAt.IsZero() {
		return billing.ErrInvalid
	}
	if _, err := json.Marshal(p); err != nil {
		return billing.ErrInvalid
	}
	return nil
}

func (p Price) Validate() error { return validatePrice(p) }

func (in QuoteInput) Validate(now time.Time) error { return validateQuoteInput(in, now) }

func (q Quote) Validate() error {
	if !billing.ValidID(string(q.Account)) || !billing.ValidID(q.ID) || q.CreatedAt.IsZero() || !q.ValidUntil.After(q.CreatedAt) || len(q.Lines) == 0 || len(q.Lines) > maxLines {
		return billing.ErrInvalid
	}
	var total int64
	seenIDs := make(map[string]struct{}, len(q.Lines))
	seenBatches := make(map[string]struct{})
	for _, line := range q.Lines {
		if !billing.ValidID(line.ID) || !validRevision(line.Price) || line.Quantity <= 0 || line.Offer.Account != q.Account || line.PriceSnapshot.Account != q.Account || line.PriceSnapshot.Revision != line.Price || line.PriceSnapshot.Offer != line.Offer.Revision {
			return billing.ErrState
		}
		if _, exists := seenIDs[line.ID]; exists {
			return billing.ErrState
		}
		seenIDs[line.ID] = struct{}{}
		if err := line.Offer.Validate(); err != nil {
			return billing.ErrState
		}
		if err := line.PriceSnapshot.Validate(); err != nil {
			return billing.ErrState
		}
		amount, err := mul(line.PriceSnapshot.UnitAmount, line.Quantity)
		if err != nil || amount != line.Amount {
			return billing.ErrState
		}
		if q.Currency != line.PriceSnapshot.Currency || q.TaxTreatment != line.PriceSnapshot.TaxTreatment {
			return billing.ErrState
		}
		settlement := false
		for _, effect := range line.Offer.Effects {
			if effect.Credit != nil {
				if _, err := mul(effect.Credit.Amount, line.Quantity); err != nil {
					return err
				}
			}
			if effect.Plan != nil {
				if _, err := mul(effect.Plan.Quantity, line.Quantity); err != nil {
					return err
				}
			}
			if effect.Settlement != nil {
				settlement = true
			}
		}
		if settlement {
			if line.Quantity != 1 || !billing.ValidID(line.SettlementBatchID) {
				return billing.ErrState
			}
			if _, exists := seenBatches[line.SettlementBatchID]; exists {
				return billing.ErrState
			}
			seenBatches[line.SettlementBatchID] = struct{}{}
		} else if line.SettlementBatchID != "" {
			return billing.ErrState
		}
		total, err = checkedAdd(total, amount)
		if err != nil {
			return err
		}
	}
	if total != q.Amount || effectCount(q) > maxQuoteEffects {
		return billing.ErrState
	}
	encoded, err := json.Marshal(q)
	if err != nil || len(encoded) > maxQuoteBytes {
		return billing.ErrState
	}
	return nil
}

func checkedAdd(a, b int64) (int64, error) {
	if b < 0 {
		return 0, billing.ErrInvalid
	}
	return checked.Add(a, b)
}

func validateQuoteInput(in QuoteInput, now time.Time) error {
	if !billing.ValidID(string(in.Account)) || !billing.ValidID(in.ID) || !in.ValidUntil.After(now) || len(in.Lines) == 0 || len(in.Lines) > maxLines {
		return billing.ErrInvalid
	}
	seen := make(map[string]struct{}, len(in.Lines))
	for _, line := range in.Lines {
		if !billing.ValidID(line.ID) || !validRevision(line.Price) || line.Quantity <= 0 {
			return billing.ErrInvalid
		}
		if _, ok := seen[line.ID]; ok {
			return billing.ErrConflict
		}
		seen[line.ID] = struct{}{}
	}
	return nil
}

func canonicalObject(raw []byte) bool {
	_, ok := canonicalPayload(raw)
	return ok
}

func canonicalPayload(raw []byte) ([]byte, bool) {
	out, err := canonicaljson.Object(raw, maxPayloadBytes)
	return out, err == nil
}

func normalizeOffer(in Offer) (Offer, error) {
	out := cloneOffer(in)
	out.PublishedAt = billing.CanonicalTime(out.PublishedAt)
	for i := range out.Effects {
		if out.Effects[i].Host == nil {
			continue
		}
		payload, ok := canonicalPayload(out.Effects[i].Host.Payload)
		if !ok {
			return Offer{}, billing.ErrInvalid
		}
		out.Effects[i].Host.Payload = payload
	}
	return out, nil
}

func mul(a, b int64) (int64, error) {
	if a != 0 && (b > math.MaxInt64/a || b < math.MinInt64/a) {
		return 0, billing.ErrOverflow
	}
	return a * b, nil
}

func digest(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func offerFingerprint(o Offer) string {
	if normalized, err := normalizeOffer(o); err == nil {
		o = normalized
	} else {
		return ""
	}
	return digest(struct {
		Account  billing.AccountID
		Revision Revision
		Name     string
		Effects  []Effect
	}{o.Account, o.Revision, o.Name, o.Effects})
}
func priceFingerprint(p Price) string {
	return digest(struct {
		Account         billing.AccountID
		Revision, Offer Revision
		Currency        string
		Amount          int64
		Tax             TaxTreatment
	}{p.Account, p.Revision, p.Offer, p.Currency, p.UnitAmount, p.TaxTreatment})
}
func quoteFingerprint(q Quote) string {
	type lineFingerprint struct {
		Input  QuoteLineInput
		Offer  string
		Price  string
		Amount int64
	}
	lines := make([]lineFingerprint, len(q.Lines))
	for i := range q.Lines {
		normalized, err := normalizeOffer(q.Lines[i].Offer)
		if err != nil {
			return ""
		}
		lines[i] = lineFingerprint{Input: q.Lines[i].QuoteLineInput, Offer: offerFingerprint(normalized), Price: priceFingerprint(q.Lines[i].PriceSnapshot), Amount: q.Lines[i].Amount}
	}
	return digest(struct {
		Account               billing.AccountID
		ID                    string
		CreatedAt, ValidUntil time.Time
		Currency              string
		Tax                   TaxTreatment
		Amount                int64
		Lines                 []lineFingerprint
	}{q.Account, q.ID, billing.CanonicalTime(q.CreatedAt), billing.CanonicalTime(q.ValidUntil), q.Currency, q.TaxTreatment, q.Amount, lines})
}

func (o Offer) Fingerprint() string { return offerFingerprint(o) }
func (p Price) Fingerprint() string { return priceFingerprint(p) }
func (q Quote) Fingerprint() string { return quoteFingerprint(q) }

func normalizeQuote(in Quote) (Quote, error) {
	out := cloneQuote(in)
	out.CreatedAt = billing.CanonicalTime(out.CreatedAt)
	out.ValidUntil = billing.CanonicalTime(out.ValidUntil)
	for i := range out.Lines {
		var err error
		out.Lines[i].Offer, err = normalizeOffer(out.Lines[i].Offer)
		if err != nil {
			return Quote{}, err
		}
		out.Lines[i].PriceSnapshot.PublishedAt = billing.CanonicalTime(out.Lines[i].PriceSnapshot.PublishedAt)
	}
	return out, nil
}
