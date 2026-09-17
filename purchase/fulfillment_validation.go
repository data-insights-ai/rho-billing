package purchase

import (
	"encoding/json"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/internal/identity"
)

func fulfillmentID(account billing.AccountID, intent, line, key string) string {
	return identity.Fingerprint("purchase-effect", string(account), intent, line, key)
}

func (f Fulfillment) Validate() error {
	if !billing.ValidID(string(f.Account)) || !billing.ValidID(f.IntentID) || !billing.ValidID(f.LineID) || f.ID != fulfillmentID(f.Account, f.IntentID, f.LineID, f.Effect.Key) || f.Quantity < 1 || f.CreatedAt.IsZero() || f.EffectiveAt.IsZero() {
		return billing.ErrInvalid
	}
	if err := validateEffect(f.Effect); err != nil {
		return err
	}
	if f.Effect.Settlement != nil {
		if !billing.ValidID(f.SettlementBatchID) || f.Quantity != 1 {
			return billing.ErrInvalid
		}
	} else if f.SettlementBatchID != "" {
		return billing.ErrInvalid
	}
	switch f.State {
	case FulfillmentCanceled:
		if f.Effect.Host == nil || !f.AppliedAt.IsZero() || !f.AcknowledgedAt.IsZero() || f.HostReference != "" {
			return billing.ErrInvalid
		}
	case FulfillmentPending:
		if !f.AppliedAt.IsZero() || !f.AcknowledgedAt.IsZero() || f.HostReference != "" {
			return billing.ErrInvalid
		}
	case FulfillmentComplete:
		if f.AppliedAt.IsZero() {
			return billing.ErrInvalid
		}
		if f.Effect.Host != nil {
			if !billing.ValidID(f.HostReference) || f.AcknowledgedAt.IsZero() {
				return billing.ErrInvalid
			}
		} else if f.HostReference != "" || !f.AcknowledgedAt.IsZero() {
			return billing.ErrInvalid
		}
	default:
		return billing.ErrInvalid
	}
	if f.Effect.Credit != nil {
		if _, err := mul(f.Effect.Credit.Amount, f.Quantity); err != nil {
			return err
		}
		if _, err := effectExpiry(f.EffectiveAt, f.Effect.Credit.Validity); err != nil {
			return err
		}
	}
	if f.Effect.Plan != nil {
		if _, err := mul(f.Effect.Plan.Quantity, f.Quantity); err != nil {
			return err
		}
		if _, err := effectExpiry(f.EffectiveAt, f.Effect.Plan.Validity); err != nil {
			return err
		}
	}
	encoded, err := json.Marshal(f)
	if err != nil || len(encoded) > maxQuoteBytes {
		return billing.ErrInvalid
	}
	return nil
}

// Fingerprint excludes receipt and acknowledgment times.
func (f Fulfillment) Fingerprint() string {
	n, err := normalizeFulfillment(f)
	if err != nil {
		return ""
	}
	n.CreatedAt = time.Time{}
	n.State = ""
	n.HostReference = ""
	n.AppliedAt = time.Time{}
	n.AcknowledgedAt = time.Time{}
	return digest(n)
}

func (f Fulfillment) RecordFingerprint() string {
	n, err := normalizeFulfillment(f)
	if err != nil {
		return ""
	}
	return digest(n)
}
func (a Acknowledgment) Validate() error {
	if !billing.ValidID(string(a.Account)) || !billing.ValidID(a.EffectID) || !validDigest(a.Fingerprint) || !billing.ValidID(a.HostReference) || a.AppliedAt.IsZero() {
		return billing.ErrInvalid
	}
	if _, err := json.Marshal(a); err != nil {
		return billing.ErrInvalid
	}
	return nil
}
func (f SettlementFunding) Validate() error {
	if !billing.ValidID(string(f.Account)) || !billing.ValidID(f.BatchID) || !billing.ValidID(f.IntentID) || !billing.ValidID(f.EffectID) || !f.Scope.Valid() || !billing.ValidID(f.TransactionID) || !validCurrency(f.Currency) || f.Amount < 0 || f.PaidAt.IsZero() {
		return billing.ErrInvalid
	}
	if _, err := json.Marshal(f); err != nil {
		return billing.ErrInvalid
	}
	return nil
}
func (f SettlementFunding) Fingerprint() string {
	f.PaidAt = billing.CanonicalTime(f.PaidAt)
	return digest(f)
}
func cloneFulfillment(f Fulfillment) Fulfillment {
	f.Effect = cloneOffer(Offer{Effects: []Effect{f.Effect}}).Effects[0]
	return f
}
func normalizeFulfillment(f Fulfillment) (Fulfillment, error) {
	f = cloneFulfillment(f)
	f.EffectiveAt = billing.CanonicalTime(f.EffectiveAt)
	f.CreatedAt = billing.CanonicalTime(f.CreatedAt)
	f.AppliedAt = billing.CanonicalTime(f.AppliedAt)
	f.AcknowledgedAt = billing.CanonicalTime(f.AcknowledgedAt)
	if f.Effect.Host != nil {
		p, ok := canonicalPayload(f.Effect.Host.Payload)
		if !ok {
			return Fulfillment{}, billing.ErrInvalid
		}
		f.Effect.Host.Payload = p
	}
	return f, nil
}
func effectExpiry(start time.Time, validity time.Duration) (time.Time, error) {
	if validity == 0 {
		return time.Time{}, nil
	}
	if validity < 0 {
		return time.Time{}, billing.ErrInvalid
	}
	end := billing.CanonicalTime(start.Add(validity))
	if !end.After(start) {
		return time.Time{}, billing.ErrInvalid
	}
	if _, err := end.MarshalText(); err != nil {
		return time.Time{}, billing.ErrInvalid
	}
	return end, nil
}
func expectedFulfillment(intent Intent, line QuoteLine, effect Effect, now time.Time) (Fulfillment, error) {
	f := Fulfillment{Account: intent.Account, ID: fulfillmentID(intent.Account, intent.ID, line.ID, effect.Key), IntentID: intent.ID, LineID: line.ID, Effect: effect, Quantity: line.Quantity, EffectiveAt: intent.PaidAt, CreatedAt: now, State: FulfillmentPending}
	if effect.Settlement != nil {
		f.SettlementBatchID = line.SettlementBatchID
	}
	normalized, err := normalizeFulfillment(f)
	if err != nil {
		return Fulfillment{}, err
	}
	if err = normalized.Validate(); err != nil {
		return Fulfillment{}, err
	}
	return normalized, nil
}
