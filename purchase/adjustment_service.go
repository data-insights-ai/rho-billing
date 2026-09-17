package purchase

import (
	"context"
	"errors"
	"math"
	"math/big"
	"slices"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/catalog"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/internal/checked"
	"github.com/data-insights-ai/rho-billing/internal/identity"
)

// ApplyAdjustment: partial plan/host refunds retain the benefit until its entire line is reversed.
func (s *Service) ApplyAdjustment(ctx context.Context, in AdjustmentInput) (AdjustmentResult, error) {
	if s == nil || s.repo == nil || s.now == nil {
		return AdjustmentResult{}, billing.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return AdjustmentResult{}, err
	}
	in = normalizeAdjustmentInput(in)
	if err := in.Validate(); err != nil {
		return AdjustmentResult{}, err
	}
	now := billing.CanonicalTime(s.now())
	if now.IsZero() {
		return AdjustmentResult{}, billing.ErrInvalid
	}
	var out AdjustmentResult
	err := s.repo.WithinAccount(ctx, in.Account, func(tx Tx) error {
		old, err := tx.Adjustment(ctx, in.ID)
		if err == nil {
			if old.Validate() != nil || old.Input.Fingerprint() != in.Fingerprint() {
				return billing.ErrConflict
			}
			out = old.Result
			return nil
		}
		if !errors.Is(err, billing.ErrNotFound) {
			return err
		}
		old, err = tx.ProviderAdjustment(ctx, in.Scope, in.ProviderAdjustmentID)
		if err == nil {
			if old.Validate() != nil || old.Input.Fingerprint() != in.Fingerprint() {
				return billing.ErrConflict
			}
			out = old.Result
			return nil
		}
		if !errors.Is(err, billing.ErrNotFound) {
			return err
		}
		intent, err := tx.Intent(ctx, in.IntentID)
		if err != nil {
			return err
		}
		if intent.Validate() != nil || intent.Account != in.Account || intent.ID != in.IntentID {
			return billing.ErrState
		}
		out = AdjustmentResult{Account: in.Account, ID: in.ID, IntentID: in.IntentID}
		reject := func(reason string) error {
			out.Rejection = reason
			return tx.InsertAdjustment(ctx, AdjustmentRecord{Input: in, Result: out, CreatedAt: now})
		}
		if intent.Scope != in.Scope {
			return reject(RejectScope)
		}
		if intent.Currency != in.Currency {
			return reject(RejectCurrency)
		}
		if intent.Payment != PaymentPaid {
			// Delivery can precede the original payment webhook. Do not retain a
			// permanent rejection that would prevent replay after funding arrives.
			return billing.ErrNotFound
		}
		if intent.TransactionID != in.TransactionID {
			return reject("funding_mismatch")
		}
		if in.OccurredAt.Before(intent.PaidAt) || in.OccurredAt.After(now) {
			return reject("adjustment_time_outside_funding")
		}
		funding, err := tx.Funding(ctx, in.Scope, in.TransactionID)
		if err != nil {
			return err
		}
		if funding.Validate() != nil || funding.Account != in.Account || funding.IntentID != intent.ID || funding.Scope != in.Scope || funding.TransactionID != in.TransactionID || funding.Currency != in.Currency || !funding.PaidAt.Equal(intent.PaidAt) {
			return billing.ErrState
		}
		quote, err := tx.Quote(ctx, intent.QuoteID)
		if err != nil {
			return err
		}
		if quote.Validate() != nil || quote.Account != in.Account || quote.ID != intent.QuoteID || quote.Fingerprint() != intent.QuoteFingerprint || compareFactAllocation(PaymentFact{Gross: funding.Gross, Tax: funding.Tax, Lines: funding.Lines}, quote) != nil {
			return billing.ErrState
		}
		state, err := tx.AdjustmentState(ctx, intent.ID)
		if errors.Is(err, billing.ErrNotFound) {
			state = AdjustmentState{Account: in.Account, IntentID: intent.ID}
		} else if err != nil {
			return err
		}
		if state.Validate() != nil || state.Account != in.Account || state.IntentID != intent.ID {
			return billing.ErrState
		}
		if state.Revision == math.MaxInt64 {
			return billing.ErrOverflow
		}
		recoveries, err := tx.ChargebackRecoveries(ctx, intent.ID)
		if err != nil {
			return err
		}
		next, reason, err := nextAdjustmentState(state, funding, in, recoveries)
		if err != nil {
			return err
		}
		if reason != "" {
			return reject(reason)
		}
		rows, err := tx.Fulfillments(ctx, intent.ID)
		if err != nil {
			return err
		}
		effects, err := adjustmentEffects(intent, quote, rows, now)
		if err != nil {
			return err
		}
		prior := make(map[string]int64, len(state.Effects))
		for _, e := range state.Effects {
			if _, ok := effects[e.EffectID]; !ok {
				return billing.ErrState
			}
			prior[e.EffectID] = e.Target
		}
		allocations := make(map[string]PaidLine, len(funding.Lines))
		for _, l := range funding.Lines {
			allocations[l.LineID] = l
		}
		totals := make(map[string]LineAdjustmentTotal, len(next.Lines))
		for _, l := range next.Lines {
			totals[l.LineID] = l
		}
		recoveredGross := make(map[string]int64, len(recoveries))
		for _, r := range recoveries {
			recoveredGross[r.LineID] = r.Gross
		}
		next.Effects = nil
		changedLines := make(map[string]bool, len(in.Lines))
		for _, line := range in.Lines {
			changedLines[line.LineID] = true
		}
		// A line that was paid at zero (a bundled free benefit) can never be
		// named in an adjustment, because adjustment lines require a positive
		// gross. Its benefits are contingent on the purchase, so they are
		// reversed exactly when every paid line of that purchase has been
		// reversed in full, and never on a partial refund.
		purchaseFullyReversed := false
		for _, line := range quote.Lines {
			paid := allocations[line.ID]
			if paid.Gross <= 0 {
				continue
			}
			total := totals[line.ID]
			if cappedAdjustedGross(paid.Gross, total.RefundedGross, total.ChargebackGross-recoveredGross[line.ID]) != paid.Gross {
				purchaseFullyReversed = false
				break
			}
			purchaseFullyReversed = true
		}
		for _, line := range quote.Lines {
			for _, effect := range line.Offer.Effects {
				row := effects[fulfillmentID(in.Account, intent.ID, line.ID, effect.Key)]
				total := totals[line.ID]
				paid := allocations[line.ID]
				basis := cappedAdjustedGross(paid.Gross, total.RefundedGross, total.ChargebackGross-recoveredGross[line.ID])
				original := int64(0)
				switch {
				case effect.Credit != nil:
					original, err = mul(effect.Credit.Amount, line.Quantity)
				case effect.Plan != nil:
					original, err = mul(effect.Plan.Quantity, line.Quantity)
				case effect.Host != nil:
					original = line.Quantity
				}
				if err != nil {
					return err
				}
				previous := prior[row.ID]
				if previous > original {
					return billing.ErrState
				}
				target := int64(0)
				zeroGross := paid.Gross == 0
				switch {
				case paid.Gross > 0 && basis == paid.Gross:
					target = original
				case effect.Credit != nil && in.CreditPolicy == CreditRefundProportional && paid.Gross > 0:
					target = proportionalTarget(original, basis, paid.Gross)
				case zeroGross && purchaseFullyReversed:
					target = original
				}
				if !changedLines[line.ID] && !(zeroGross && purchaseFullyReversed) {
					target = previous
				}
				target = max(previous, target)
				if target > previous {
					result, err := reversePurchaseEffect(ctx, tx, in, row, original, target, target-previous, now)
					if err != nil {
						return err
					}
					out.Effects = append(out.Effects, result)
				}
				if target > 0 {
					next.Effects = append(next.Effects, EffectAdjustmentTotal{EffectID: row.ID, Target: target})
				}
			}
		}
		next.Revision = state.Revision + 1
		if err := tx.SaveAdjustmentState(ctx, next, state.Revision); err != nil {
			return err
		}
		out.Applied = true
		return tx.InsertAdjustment(ctx, AdjustmentRecord{Input: in, Result: out, CreatedAt: now})
	})
	if err != nil {
		return AdjustmentResult{}, err
	}
	out.Effects = slices.Clone(out.Effects)
	return out, nil
}

func nextAdjustmentState(state AdjustmentState, funding Funding, in AdjustmentInput, recoveries []PaidLine) (AdjustmentState, string, error) {
	next := state
	next.Lines = slices.Clone(state.Lines)
	paid := make(map[string]PaidLine, len(funding.Lines))
	for _, l := range funding.Lines {
		paid[l.LineID] = l
	}
	recovered := make(map[string]PaidLine, len(recoveries))
	if len(recoveries) > maxLines {
		return AdjustmentState{}, "", billing.ErrState
	}
	for _, r := range recoveries {
		if _, ok := paid[r.LineID]; !ok || r.Gross <= 0 || r.Tax < 0 || r.Tax > r.Gross {
			return AdjustmentState{}, "", billing.ErrState
		}
		if _, exists := recovered[r.LineID]; exists {
			return AdjustmentState{}, "", billing.ErrState
		}
		recovered[r.LineID] = r
	}
	totals := make(map[string]LineAdjustmentTotal, len(state.Lines))
	validTotal := func(gross, tax int64, p PaidLine) bool {
		return gross >= 0 && tax >= 0 && tax <= gross && gross <= p.Gross && tax <= p.Tax && gross-tax <= p.Gross-p.Tax
	}
	for _, l := range state.Lines {
		p, ok := paid[l.LineID]
		if !ok || !validTotal(l.RefundedGross, l.RefundedTax, p) || !validTotal(l.ChargebackGross-recovered[l.LineID].Gross, l.ChargebackTax-recovered[l.LineID].Tax, p) {
			return AdjustmentState{}, "", billing.ErrState
		}
		totals[l.LineID] = l
	}
	for id := range recovered {
		if _, ok := totals[id]; !ok {
			return AdjustmentState{}, "", billing.ErrState
		}
	}
	for _, line := range in.Lines {
		p, ok := paid[line.LineID]
		if !ok {
			return AdjustmentState{}, RejectAllocation, nil
		}
		t := totals[line.LineID]
		t.LineID = line.LineID
		gross, tax := t.RefundedGross, t.RefundedTax
		if in.Kind == AdjustmentChargeback {
			gross, tax = t.ChargebackGross-recovered[line.LineID].Gross, t.ChargebackTax-recovered[line.LineID].Tax
		}
		if line.Gross > p.Gross-gross || line.Tax > p.Tax-tax || line.Gross-line.Tax > (p.Gross-p.Tax)-(gross-tax) {
			if in.Kind == AdjustmentChargeback {
				// A preceding recovery may be delivered later. Do not permanently reject
				// this identity while authoritative outstanding exposure is incomplete.
				return AdjustmentState{}, "", billing.ErrConflict
			}
			return AdjustmentState{}, "adjustment_exceeds_payment", nil
		}
		gross += line.Gross
		tax += line.Tax
		if in.Kind == AdjustmentChargeback {
			var err error
			t.ChargebackGross, err = checked.Add(t.ChargebackGross, line.Gross)
			if err != nil {
				return AdjustmentState{}, "", err
			}
			t.ChargebackTax, err = checked.Add(t.ChargebackTax, line.Tax)
			if err != nil {
				return AdjustmentState{}, "", err
			}
		} else {
			t.RefundedGross, t.RefundedTax = gross, tax
		}
		totals[line.LineID] = t
	}
	next.Lines = nil
	for _, p := range funding.Lines {
		if t, ok := totals[p.LineID]; ok {
			next.Lines = append(next.Lines, t)
		}
	}
	return next, "", nil
}
func cappedAdjustedGross(original, refund, chargeback int64) int64 {
	if refund >= original || chargeback >= original-refund {
		return original
	}
	return refund + chargeback
}
func proportionalTarget(quantity, gross, total int64) int64 {
	var n, d big.Int
	n.Mul(big.NewInt(quantity), big.NewInt(gross))
	d.SetInt64(total)
	n.Quo(&n, &d)
	return n.Int64()
}
func adjustmentEffects(intent Intent, quote Quote, rows []Fulfillment, now time.Time) (map[string]Fulfillment, error) {
	if len(rows) != effectCount(quote) || len(rows) > maxQuoteEffects {
		return nil, billing.ErrState
	}
	expected := make(map[string]Fulfillment, len(rows))
	for _, line := range quote.Lines {
		for _, effect := range line.Offer.Effects {
			f, err := expectedFulfillment(intent, line, effect, now)
			if err != nil {
				return nil, err
			}
			expected[f.ID] = f
		}
	}
	actual := make(map[string]Fulfillment, len(rows))
	for _, row := range rows {
		want, ok := expected[row.ID]
		if !ok || row.Validate() != nil || row.Account != intent.Account || row.IntentID != intent.ID || row.Fingerprint() != want.Fingerprint() {
			return nil, billing.ErrState
		}
		if _, ok := actual[row.ID]; ok {
			return nil, billing.ErrState
		}
		if row.Effect.Host == nil && row.State != FulfillmentComplete {
			return nil, billing.ErrState
		}
		actual[row.ID] = row
	}
	return actual, nil
}
func purchaseLot(ctx context.Context, tx Tx, account billing.AccountID, id string) (credit.Lot, error) {
	var lot credit.Lot
	err := tx.Credits().WithinAccount(ctx, account, func(ct credit.Tx) error {
		var ok bool
		var err error
		lot, ok, err = ct.Lot(id)
		if err != nil {
			return err
		}
		if !ok {
			return billing.ErrNotFound
		}
		return nil
	})
	return lot, err
}
func reversePurchaseEffect(ctx context.Context, tx Tx, in AdjustmentInput, row Fulfillment, original, target, delta int64, now time.Time) (EffectAdjustment, error) {
	out := EffectAdjustment{EffectID: row.ID, TargetDelta: delta}
	switch {
	case row.Effect.Credit != nil:
		before, err := purchaseLot(ctx, tx, in.Account, row.ID)
		if err != nil {
			return EffectAdjustment{}, err
		}
		if before.Initial != original || before.Source != "purchase" || before.SourceRef != row.ID || before.Unit != row.Effect.Credit.Unit || before.Scope != row.Effect.Credit.Scope || !before.ValidFrom.Equal(row.EffectiveAt) {
			return EffectAdjustment{}, billing.ErrState
		}
		if before.RevokedAt.IsZero() {
			engine := credit.New(tx.Credits(), func() time.Time { return now })
			op := billing.OperationID(identity.Fingerprint("purchase-adjustment", string(in.Account), in.ID, row.ID))
			if target == original {
				_, err = engine.Revoke(ctx, credit.RevokeInput{Account: in.Account, Operation: op, LotID: row.ID, Reason: in.Reason})
			} else {
				_, err = engine.RevokeAmount(ctx, credit.RevokeAmountInput{Account: in.Account, Operation: op, LotID: row.ID, Reason: in.Reason, Amount: delta})
			}
			if err != nil {
				return EffectAdjustment{}, err
			}
		}
		after, err := purchaseLot(ctx, tx, in.Account, row.ID)
		if err != nil {
			return EffectAdjustment{}, err
		}
		pendingBefore, pendingAfter := before.PendingRevocation, after.PendingRevocation
		if !before.RevokedAt.IsZero() {
			pendingBefore = before.Held
		}
		if !after.RevokedAt.IsZero() {
			pendingAfter = after.Held
		}
		out.RevokedCredits = after.Revoked - before.Revoked
		out.PendingCredits = max(0, pendingAfter-pendingBefore)
		out.ConsumedExposure = after.Consumed
	case row.Effect.Plan != nil:
		service := catalog.NewEntitlement(tx.Entitlements(), func() time.Time { return now })
		old, err := service.Revocation(ctx, in.Account, row.ID)
		if errors.Is(err, billing.ErrNotFound) {
			_, err = service.Revoke(ctx, catalog.Revocation{Account: in.Account, AssignmentID: row.ID, SourceRef: in.ID, Actor: in.Actor, Reason: in.Reason, EffectiveAt: in.OccurredAt})
		} else if err == nil && (old.Account != in.Account || old.AssignmentID != row.ID || old.EffectiveAt.After(in.OccurredAt)) {
			return EffectAdjustment{}, billing.ErrState
		}
		if err != nil {
			return EffectAdjustment{}, err
		}
		out.AccessRevoked = true
	case row.Effect.Host != nil:
		id := reversalID(in.Account, row.ID)
		_, err := tx.Reversal(ctx, id)
		if !errors.Is(err, billing.ErrNotFound) {
			if err != nil {
				return EffectAdjustment{}, err
			}
			return EffectAdjustment{}, billing.ErrState
		}
		reversal := Reversal{Account: in.Account, ID: id, AdjustmentID: in.ID, IntentID: in.IntentID, Original: row, EffectiveAt: in.OccurredAt, CreatedAt: now, State: FulfillmentPending}
		if err := reversal.Validate(); err != nil {
			return EffectAdjustment{}, err
		}
		if err := tx.InsertReversal(ctx, reversal); err != nil {
			return EffectAdjustment{}, err
		}
		if row.State == FulfillmentPending {
			fingerprint := row.Fingerprint()
			row.State = FulfillmentCanceled
			if err := tx.SaveFulfillment(ctx, row, fingerprint); err != nil {
				return EffectAdjustment{}, err
			}
		}
		out.HostReversalID = id
	}
	return out, nil
}
