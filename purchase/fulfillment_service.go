package purchase

import (
	"context"
	"errors"
	"math"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/catalog"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/usage"
)

func (s *Service) Fulfill(ctx context.Context, account billing.AccountID, intentID string) (Intent, error) {
	if s.repo == nil || s.now == nil || !billing.ValidID(string(account)) || !billing.ValidID(intentID) {
		return Intent{}, billing.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return Intent{}, err
	}
	now := billing.CanonicalTime(s.now())
	if now.IsZero() {
		return Intent{}, billing.ErrInvalid
	}
	var out Intent
	err := s.repo.WithinAccount(ctx, account, func(tx Tx) error {
		intent, err := tx.Intent(ctx, intentID)
		if err != nil {
			return err
		}
		if err := intent.Validate(); err != nil || intent.Account != account || intent.ID != intentID || intent.Payment != PaymentPaid {
			return billing.ErrState
		}
		if now.Before(intent.UpdatedAt) {
			return billing.ErrState
		}
		quote, err := tx.Quote(ctx, intent.QuoteID)
		if err != nil {
			return err
		}
		if err := quote.Validate(); err != nil || quote.Account != account || quote.ID != intent.QuoteID || quote.Fingerprint() != intent.QuoteFingerprint {
			return billing.ErrState
		}
		funding, err := tx.Funding(ctx, intent.Scope, intent.TransactionID)
		if err != nil {
			return err
		}
		if err := funding.Validate(); err != nil || funding.Account != account || funding.IntentID != intent.ID || funding.Scope != intent.Scope || funding.TransactionID != intent.TransactionID || funding.Currency != intent.Currency || !funding.PaidAt.Equal(intent.PaidAt) {
			return billing.ErrState
		}
		if err := compareFactAllocation(PaymentFact{Gross: funding.Gross, Tax: funding.Tax, Discount: funding.Discount, Lines: funding.Lines}, quote); err != nil {
			return billing.ErrState
		}
		complete, err := applyFulfillments(ctx, tx, intent, quote, funding, now)
		if err != nil {
			return err
		}
		if complete && intent.Fulfillment != FulfillmentComplete {
			if intent.Revision == math.MaxInt64 {
				return billing.ErrOverflow
			}
			intent.Fulfillment = FulfillmentComplete
			intent.Revision++
			intent.UpdatedAt = now
			if err := tx.SaveIntent(ctx, intent, intent.Revision-1); err != nil {
				return err
			}
		}
		out = normalizeIntent(intent)
		return nil
	})
	if err != nil {
		return Intent{}, err
	}
	return out, nil
}

// Acknowledge requires the host to have atomically applied the effect and deduplicated its ID.
func (s *Service) Acknowledge(ctx context.Context, ack Acknowledgment) (Fulfillment, error) {
	if s.repo == nil || s.now == nil {
		return Fulfillment{}, billing.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return Fulfillment{}, err
	}
	ack.AppliedAt = billing.CanonicalTime(ack.AppliedAt)
	if err := ack.Validate(); err != nil {
		return Fulfillment{}, err
	}
	now := billing.CanonicalTime(s.now())
	if now.IsZero() {
		return Fulfillment{}, billing.ErrInvalid
	}
	var out Fulfillment
	err := s.repo.WithinAccount(ctx, ack.Account, func(tx Tx) error {
		old, err := tx.Fulfillment(ctx, ack.EffectID)
		if err != nil {
			return err
		}
		if err := old.Validate(); err != nil || old.Account != ack.Account || old.ID != ack.EffectID || old.Fingerprint() != ack.Fingerprint {
			return billing.ErrConflict
		}
		intent, err := tx.Intent(ctx, old.IntentID)
		if err != nil {
			return err
		}
		if err := intent.Validate(); err != nil || intent.Account != ack.Account || intent.ID != old.IntentID || intent.Payment != PaymentPaid {
			return billing.ErrState
		}
		if now.Before(intent.UpdatedAt) {
			return billing.ErrState
		}
		if old.State == FulfillmentComplete {
			if old.HostReference != ack.HostReference || !old.AppliedAt.Equal(ack.AppliedAt) {
				return billing.ErrConflict
			}
			out, err = normalizeFulfillment(old)
			if err != nil {
				return err
			}
			return nil
		}
		if old.Effect.Host == nil || old.State != FulfillmentPending || old.HostReference != "" {
			return billing.ErrState
		}
		old.HostReference, old.AppliedAt, old.AcknowledgedAt, old.State = ack.HostReference, billing.CanonicalTime(ack.AppliedAt), now, FulfillmentComplete
		if err := tx.SaveFulfillment(ctx, old, ack.Fingerprint); err != nil {
			return err
		}
		if err := updateIntentFulfillment(ctx, tx, old.IntentID, ack.Account, now); err != nil {
			return err
		}
		out, err = normalizeFulfillment(old)
		if err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return Fulfillment{}, err
	}
	return cloneFulfillment(out), nil
}

func (s *Service) Fulfillments(ctx context.Context, account billing.AccountID, intentID string) ([]Fulfillment, error) {
	if s.repo == nil || !billing.ValidID(string(account)) || !billing.ValidID(intentID) {
		return nil, billing.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var out []Fulfillment
	err := s.repo.WithinAccount(ctx, account, func(tx Tx) error {
		rows, err := tx.Fulfillments(ctx, intentID)
		if err != nil {
			return err
		}
		if len(rows) > maxQuoteEffects {
			return billing.ErrState
		}
		for _, row := range rows {
			if err := row.Validate(); err != nil || row.Account != account || row.IntentID != intentID {
				return billing.ErrState
			}
			normalized, err := normalizeFulfillment(row)
			if err != nil {
				return err
			}
			out = append(out, normalized)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return cloneFulfillments(out), nil
}

func cloneFulfillments(in []Fulfillment) []Fulfillment {
	out := make([]Fulfillment, len(in))
	for i := range in {
		out[i] = cloneFulfillment(in[i])
	}
	return out
}

func applyFulfillments(ctx context.Context, tx Tx, intent Intent, quote Quote, funding Funding, now time.Time) (bool, error) {
	rows, err := tx.Fulfillments(ctx, intent.ID)
	if err != nil {
		return false, err
	}
	if len(rows) > maxQuoteEffects {
		return false, billing.ErrState
	}
	expected := make(map[string]Fulfillment)
	for _, line := range quote.Lines {
		for _, effect := range line.Offer.Effects {
			row, err := expectedFulfillment(intent, line, effect, now)
			if err != nil {
				return false, err
			}
			expected[row.ID] = row
		}
	}
	existing := make(map[string]Fulfillment, len(rows))
	for _, row := range rows {
		if _, duplicate := existing[row.ID]; duplicate {
			return false, billing.ErrConflict
		}
		want, ok := expected[row.ID]
		if !ok || row.Account != intent.Account || row.IntentID != intent.ID || row.Validate() != nil || row.Fingerprint() != want.Fingerprint() {
			return false, billing.ErrState
		}
		existing[row.ID] = row
	}
	if intent.Fulfillment == FulfillmentComplete && len(existing) != len(expected) {
		return false, billing.ErrState
	}
	allComplete := true
	for _, line := range quote.Lines {
		for _, effect := range line.Offer.Effects {
			id := fulfillmentID(intent.Account, intent.ID, line.ID, effect.Key)
			if old, ok := existing[id]; ok {
				if old.State != FulfillmentComplete {
					if effect.Host == nil || intent.Fulfillment == FulfillmentComplete {
						return false, billing.ErrState
					}
					allComplete = false
				}
				continue
			}
			row := expected[id]
			if err := deliverEffect(ctx, tx, intent, line, effect, funding, now); err != nil {
				return false, err
			}
			if effect.Host == nil {
				row.State = FulfillmentComplete
				row.AppliedAt = now
			} else {
				allComplete = false
			}
			if err := tx.InsertFulfillment(ctx, row); err != nil {
				return false, err
			}
		}
	}

	return allComplete, nil
}

func deliverEffect(ctx context.Context, tx Tx, intent Intent, line QuoteLine, effect Effect, funding Funding, now time.Time) error {
	id := fulfillmentID(intent.Account, intent.ID, line.ID, effect.Key)
	if effect.Credit != nil {
		amount, err := mul(effect.Credit.Amount, line.Quantity)
		if err != nil {
			return err
		}
		expires, err := effectExpiry(funding.PaidAt, effect.Credit.Validity)
		if err != nil {
			return err
		}
		_, err = credit.New(tx.Credits(), func() time.Time { return now }).Grant(ctx, credit.GrantInput{Account: intent.Account, Operation: billing.OperationID(id + "-grant"), LotID: id, Unit: effect.Credit.Unit, Amount: amount, Scope: effect.Credit.Scope, Source: "purchase", SourceRef: id, ValidFrom: funding.PaidAt, ExpiresAt: expires})
		return err
	}
	if effect.Plan != nil {
		quantity, err := mul(effect.Plan.Quantity, line.Quantity)
		if err != nil {
			return err
		}
		end, err := effectExpiry(funding.PaidAt, effect.Plan.Validity)
		if err != nil {
			return err
		}
		perpetual := effect.Plan.Validity == 0
		_, err = catalog.NewEntitlement(tx.Entitlements(), func() time.Time { return now }).Assign(ctx, catalog.Assignment{Account: intent.Account, Plan: catalog.PlanAssignment{ID: id, PlanVersionID: effect.Plan.PlanVersionID, Quantity: quantity, Effective: billing.Period{Start: funding.PaidAt, End: end}, Perpetual: perpetual, Source: catalog.SourcePurchase}, SourceRef: id, Actor: intent.Actor, Reason: intent.Reason, CreatedAt: now})
		return err
	}
	if effect.Settlement != nil {
		if intent.TaxTreatment != TaxExclusive || line.Quantity != 1 || line.SettlementBatchID == "" {
			return billing.ErrState
		}
		batch, err := tx.Settlement(ctx, line.SettlementBatchID)
		if err != nil {
			return err
		}
		if batch.ID != line.SettlementBatchID || batch.Account != intent.Account || batch.Currency != intent.Currency || batch.Total != line.Amount {
			return billing.ErrState
		}
		switch batch.State {
		case usage.BatchReady, usage.BatchSubmitting, usage.BatchConfirmed, usage.BatchRejected, usage.BatchUnknown:
		default:
			return billing.ErrState
		}
		old, err := tx.SettlementFunding(ctx, line.SettlementBatchID)
		expectedFunding := SettlementFunding{Account: intent.Account, BatchID: line.SettlementBatchID, IntentID: intent.ID, EffectID: id, Scope: intent.Scope, TransactionID: funding.TransactionID, Currency: intent.Currency, Amount: line.Amount, PaidAt: funding.PaidAt}
		if err == nil {
			if old.Validate() != nil || old.Fingerprint() != expectedFunding.Fingerprint() {
				return billing.ErrConflict
			}
			return nil
		} else if !errors.Is(err, billing.ErrNotFound) {
			return err
		}
		return tx.InsertSettlementFunding(ctx, expectedFunding)
	}
	return nil
}

func updateIntentFulfillment(ctx context.Context, tx Tx, intentID string, account billing.AccountID, now time.Time) error {
	intent, err := tx.Intent(ctx, intentID)
	if err != nil {
		return err
	}
	if err := intent.Validate(); err != nil || intent.Account != account || intent.ID != intentID || intent.Payment != PaymentPaid || now.Before(intent.UpdatedAt) {
		return billing.ErrState
	}
	quote, err := tx.Quote(ctx, intent.QuoteID)
	if err != nil {
		return err
	}
	if err := quote.Validate(); err != nil || quote.Account != account || quote.ID != intent.QuoteID || quote.Fingerprint() != intent.QuoteFingerprint {
		return billing.ErrState
	}
	rows, err := tx.Fulfillments(ctx, intentID)
	if err != nil {
		return err
	}
	expected := make(map[string]Fulfillment)
	for _, line := range quote.Lines {
		for _, effect := range line.Offer.Effects {
			row, err := expectedFulfillment(intent, line, effect, now)
			if err != nil {
				return err
			}
			expected[row.ID] = row
		}
	}
	if len(rows) != len(expected) || len(rows) > maxQuoteEffects {
		return billing.ErrState
	}
	complete := true
	seen := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		if _, duplicate := seen[row.ID]; duplicate {
			return billing.ErrConflict
		}
		seen[row.ID] = struct{}{}
		want, ok := expected[row.ID]
		if !ok || row.Account != account || row.IntentID != intentID || row.Validate() != nil || row.Fingerprint() != want.Fingerprint() {
			return billing.ErrState
		}
		if row.State != FulfillmentComplete {
			complete = false
		}
	}
	if !complete && intent.Fulfillment == FulfillmentComplete {
		return billing.ErrState
	}
	if !complete || intent.Fulfillment == FulfillmentComplete {
		return nil
	}
	if intent.Revision == math.MaxInt64 {
		return billing.ErrOverflow
	}
	intent.Fulfillment = FulfillmentComplete
	intent.Revision++
	intent.UpdatedAt = now
	return tx.SaveIntent(ctx, intent, intent.Revision-1)
}
