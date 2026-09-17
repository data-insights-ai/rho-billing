package purchase

import (
	"cmp"
	"encoding/json"
	"slices"
	"strings"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/internal/identity"
)

func (a AdjustmentInput) Validate() error {
	if !billing.ValidID(string(a.Account)) || !billing.ValidID(a.ID) || !billing.ValidID(a.IntentID) || !billing.ValidID(a.ProviderAdjustmentID) || !billing.ValidID(a.TransactionID) || !a.Scope.Valid() || !validCurrency(a.Currency) || !billing.ValidID(a.PolicyVersion) || !billing.ValidID(a.Actor) || strings.TrimSpace(a.Reason) == "" || len(a.Reason) > 2000 || a.OccurredAt.IsZero() || len(a.Lines) == 0 || len(a.Lines) > maxLines {
		return billing.ErrInvalid
	}
	if a.Kind != AdjustmentRefund && a.Kind != AdjustmentChargeback {
		return billing.ErrInvalid
	}
	if a.CreditPolicy != CreditRefundProportional && a.CreditPolicy != CreditRefundFullOnly {
		return billing.ErrInvalid
	}
	seen := make(map[string]bool, len(a.Lines))
	for _, line := range a.Lines {
		if !billing.ValidID(line.LineID) || seen[line.LineID] || line.Gross <= 0 || line.Tax < 0 || line.Tax > line.Gross {
			return billing.ErrInvalid
		}
		seen[line.LineID] = true
	}
	return boundedAdjustmentJSON(a)
}
func normalizeAdjustmentInput(a AdjustmentInput) AdjustmentInput {
	a.Lines = slices.Clone(a.Lines)
	slices.SortFunc(a.Lines, func(a, b PaidLine) int { return cmp.Compare(a.LineID, b.LineID) })
	a.OccurredAt = billing.CanonicalTime(a.OccurredAt)
	return a
}

func (a AdjustmentInput) Fingerprint() string { return digest(normalizeAdjustmentInput(a)) }
func (a AdjustmentRecord) Validate() error {
	if err := a.Input.Validate(); err != nil {
		return err
	}
	if a.CreatedAt.IsZero() || a.Result.Account != a.Input.Account || a.Result.ID != a.Input.ID || a.Result.IntentID != a.Input.IntentID || a.Result.Applied == (a.Result.Rejection != "") || len(a.Result.Effects) > maxQuoteEffects {
		return billing.ErrInvalid
	}
	if !a.Result.Applied && len(a.Result.Effects) != 0 {
		return billing.ErrInvalid
	}
	seen := make(map[string]bool, len(a.Result.Effects))
	for _, e := range a.Result.Effects {
		if !billing.ValidID(e.EffectID) || seen[e.EffectID] || e.TargetDelta < 0 || e.RevokedCredits < 0 || e.PendingCredits < 0 || e.ConsumedExposure < 0 || (e.HostReversalID != "" && !billing.ValidID(e.HostReversalID)) {
			return billing.ErrInvalid
		}
		seen[e.EffectID] = true
	}
	return boundedAdjustmentJSON(a)
}
func (a AdjustmentRecord) Fingerprint() string {
	a.Input = normalizeAdjustmentInput(a.Input)
	a.CreatedAt = billing.CanonicalTime(a.CreatedAt)
	a.Result.Effects = slices.Clone(a.Result.Effects)
	slices.SortFunc(a.Result.Effects, func(a, b EffectAdjustment) int { return cmp.Compare(a.EffectID, b.EffectID) })
	return digest(a)
}
func (s AdjustmentState) Validate() error {
	if !billing.ValidID(string(s.Account)) || !billing.ValidID(s.IntentID) || s.Revision < 0 || len(s.Lines) > maxLines || len(s.Effects) > maxQuoteEffects || (s.Revision == 0 && (len(s.Lines) > 0 || len(s.Effects) > 0)) {
		return billing.ErrInvalid
	}
	seen := make(map[string]bool, len(s.Lines))
	for _, l := range s.Lines {
		if !billing.ValidID(l.LineID) || seen[l.LineID] || l.RefundedGross < 0 || l.RefundedTax < 0 || l.RefundedTax > l.RefundedGross || l.ChargebackGross < 0 || l.ChargebackTax < 0 || l.ChargebackTax > l.ChargebackGross {
			return billing.ErrInvalid
		}
		seen[l.LineID] = true
	}
	seen = make(map[string]bool, len(s.Effects))
	for _, e := range s.Effects {
		if !billing.ValidID(e.EffectID) || seen[e.EffectID] || e.Target < 0 {
			return billing.ErrInvalid
		}
		seen[e.EffectID] = true
	}
	return boundedAdjustmentJSON(s)
}
func (s AdjustmentState) Fingerprint() string {
	s.Lines = slices.Clone(s.Lines)
	s.Effects = slices.Clone(s.Effects)
	slices.SortFunc(s.Lines, func(a, b LineAdjustmentTotal) int { return cmp.Compare(a.LineID, b.LineID) })
	slices.SortFunc(s.Effects, func(a, b EffectAdjustmentTotal) int { return cmp.Compare(a.EffectID, b.EffectID) })
	return digest(s)
}
func reversalID(account billing.AccountID, effectID string) string {
	return identity.Fingerprint("purchase-reversal", string(account), effectID)
}
func (r Reversal) Validate() error {
	if !billing.ValidID(string(r.Account)) || !billing.ValidID(r.AdjustmentID) || !billing.ValidID(r.IntentID) || r.ID != reversalID(r.Account, r.Original.ID) || r.Original.Account != r.Account || r.Original.IntentID != r.IntentID || r.Original.Effect.Host == nil || r.Original.State == FulfillmentCanceled || r.Original.Validate() != nil || r.EffectiveAt.IsZero() || r.CreatedAt.IsZero() || r.EffectiveAt.Before(r.Original.EffectiveAt) {
		return billing.ErrInvalid
	}
	switch r.State {
	case FulfillmentPending:
		if r.HostReference != "" || !r.AppliedAt.IsZero() || !r.AcknowledgedAt.IsZero() {
			return billing.ErrInvalid
		}
	case FulfillmentComplete:
		if !billing.ValidID(r.HostReference) || r.AppliedAt.IsZero() || r.AcknowledgedAt.IsZero() {
			return billing.ErrInvalid
		}
	default:
		return billing.ErrInvalid
	}
	return boundedAdjustmentJSON(r)
}
func normalizeReversal(r Reversal) (Reversal, error) {
	var err error
	r.Original, err = normalizeFulfillment(r.Original)
	if err != nil {
		return Reversal{}, err
	}
	r.EffectiveAt = billing.CanonicalTime(r.EffectiveAt)
	r.CreatedAt = billing.CanonicalTime(r.CreatedAt)
	r.AppliedAt = billing.CanonicalTime(r.AppliedAt)
	r.AcknowledgedAt = billing.CanonicalTime(r.AcknowledgedAt)
	return r, nil
}
func (r Reversal) Fingerprint() string {
	n, err := normalizeReversal(r)
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
func (r Reversal) RecordFingerprint() string {
	n, err := normalizeReversal(r)
	if err != nil {
		return ""
	}
	return digest(n)
}
func boundedAdjustmentJSON(v any) error {
	b, err := json.Marshal(v)
	if err != nil || len(b) > maxQuoteBytes {
		return billing.ErrInvalid
	}
	return nil
}
