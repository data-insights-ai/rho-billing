package purchase

import (
	"context"
	"errors"
	"math"
	"slices"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
)

func (s *Service) CreateIntent(ctx context.Context, in IntentInput) (Intent, error) {
	if s.repo == nil || s.now == nil {
		return Intent{}, billing.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return Intent{}, err
	}
	now := billing.CanonicalTime(s.now())
	in.ExpiresAt = billing.CanonicalTime(in.ExpiresAt)
	if err := in.Validate(); err != nil {
		return Intent{}, err
	}
	if now.IsZero() {
		return Intent{}, billing.ErrInvalid
	}
	var out Intent
	err := s.repo.WithinAccount(ctx, in.Account, func(tx Tx) error {
		if old, err := tx.Intent(ctx, in.ID); err == nil {
			if old.Validate() != nil || old.Account != in.Account || old.ID != in.ID || !sameIntentInput(old.IntentInput, in) {
				return billing.ErrConflict
			}
			out = normalizeIntent(old)
			return nil
		} else if !errors.Is(err, billing.ErrNotFound) {
			return err
		}
		if old, err := tx.IntentByOperation(ctx, in.Operation); err == nil {
			if old.Validate() != nil || old.Account != in.Account || old.Operation != in.Operation || !sameIntentInput(old.IntentInput, in) {
				return billing.ErrConflict
			}
			out = normalizeIntent(old)
			return nil
		} else if !errors.Is(err, billing.ErrNotFound) {
			return err
		}
		quote, err := tx.Quote(ctx, in.QuoteID)
		if err != nil {
			return err
		}
		if quote.Validate() != nil || quote.Account != in.Account || quote.ID != in.QuoteID {
			return billing.ErrState
		}
		if quote.Fingerprint() != in.QuoteFingerprint {
			return billing.ErrConflict
		}
		if now.Before(quote.CreatedAt) {
			return billing.ErrState
		}
		if !now.Before(quote.ValidUntil) || !in.ExpiresAt.After(now) {
			return billing.ErrExpired
		}
		if in.ExpiresAt.After(quote.ValidUntil) {
			return billing.ErrInvalid
		}
		out = Intent{IntentInput: in, Currency: quote.Currency, TaxTreatment: quote.TaxTreatment, Amount: quote.Amount, Command: CommandPlanned, Payment: PaymentPending, Fulfillment: FulfillmentPending, Revision: 1, CreatedAt: now, UpdatedAt: now}
		if err := out.Validate(); err != nil {
			return err
		}
		if err := tx.InsertIntent(ctx, out); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return Intent{}, err
	}
	return out, nil
}

func (s *Service) Intent(ctx context.Context, account billing.AccountID, id string) (Intent, error) {
	if s.repo == nil || !billing.ValidID(string(account)) || !billing.ValidID(id) {
		return Intent{}, billing.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return Intent{}, err
	}
	var out Intent
	err := s.repo.WithinAccount(ctx, account, func(tx Tx) error {
		v, err := tx.Intent(ctx, id)
		if err != nil {
			return err
		}
		if v.Account != account || v.ID != id || v.Validate() != nil {
			return billing.ErrState
		}
		out = normalizeIntent(v)
		return nil
	})
	if err != nil {
		return Intent{}, err
	}
	return out, nil
}

// RecordCommand performs no network I/O inside WithinAccount.
func (s *Service) RecordCommand(ctx context.Context, in CommandInput) (Intent, error) {
	if s.repo == nil || s.now == nil {
		return Intent{}, billing.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return Intent{}, err
	}
	now := billing.CanonicalTime(s.now())
	in.OccurredAt = billing.CanonicalTime(in.OccurredAt)
	if err := in.Validate(); err != nil || now.IsZero() {
		return Intent{}, billing.ErrInvalid
	}
	var out Intent
	err := s.repo.WithinAccount(ctx, in.Account, func(tx Tx) error {
		if old, err := tx.Command(ctx, in.Operation); err == nil {
			if old.Validate() != nil || old.Input.Fingerprint() != in.Fingerprint() {
				return billing.ErrConflict
			}
			out = normalizeIntent(old.Result)
			return nil
		} else if !errors.Is(err, billing.ErrNotFound) {
			return err
		}
		intent, err := tx.Intent(ctx, in.IntentID)
		if err != nil {
			return err
		}
		if intent.Validate() != nil || intent.Account != in.Account || intent.ID != in.IntentID || intent.Revision != in.ExpectedRevision {
			return billing.ErrConflict
		}
		if now.Before(intent.UpdatedAt) || in.OccurredAt.Before(intent.CreatedAt) {
			return billing.ErrState
		}
		if in.State == CommandDispatched && !now.Before(intent.ExpiresAt) {
			return billing.ErrExpired
		}
		if err := commandTransition(intent.Command, in.State); err != nil {
			return err
		}
		if commandTransitionRequiresEvidence(intent.Command, in.State) && in.EvidenceReference == "" {
			return billing.ErrInvalid
		}
		if (in.State == CommandAccepted || in.State == CommandReconciled) && in.ProviderReference == "" {
			return billing.ErrInvalid
		}
		intent.Command = in.State
		intent.UpdatedAt = now
		if intent.Revision == math.MaxInt64 {
			return billing.ErrOverflow
		}
		intent.Revision++
		if err := tx.SaveIntent(ctx, intent, in.ExpectedRevision); err != nil {
			return err
		}
		if err := tx.InsertCommand(ctx, CommandRecord{Input: in, Result: intent}); err != nil {
			return err
		}
		out = normalizeIntent(intent)
		return nil
	})
	if err != nil {
		return Intent{}, err
	}
	return out, nil
}

// ApplyPayment: Applied only means this observation was accepted, not that a
// transfer or product effect ran. Check Rejection even when error is nil.
func (s *Service) ApplyPayment(ctx context.Context, fact PaymentFact) (PaymentResult, error) {
	if s.repo == nil || s.now == nil {
		return PaymentResult{}, billing.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return PaymentResult{}, err
	}
	now := billing.CanonicalTime(s.now())
	if now.IsZero() {
		return PaymentResult{}, billing.ErrInvalid
	}
	if err := fact.Validate(); err != nil {
		return PaymentResult{}, err
	}
	fact = normalizePaymentFact(fact)
	if err := fact.Validate(); err != nil {
		return PaymentResult{}, err
	}
	var out PaymentResult
	err := s.repo.WithinAccount(ctx, fact.Account, func(tx Tx) error {
		if old, err := tx.PaymentEvent(ctx, fact.Scope, fact.EventID); err == nil {
			if old.Validate() != nil || old.Fact.Fingerprint() != fact.Fingerprint() {
				return billing.ErrConflict
			}
			out = old.Result
			return nil
		} else if !errors.Is(err, billing.ErrNotFound) {
			return err
		}
		intent, err := tx.Intent(ctx, fact.IntentID)
		if err != nil {
			return err
		}
		out = PaymentResult{Account: fact.Account, IntentID: fact.IntentID, EventID: fact.EventID, TransactionID: fact.TransactionID}
		if intent.Validate() != nil || intent.Account != fact.Account || intent.ID != fact.IntentID {
			return billing.ErrState
		}
		if now.Before(intent.UpdatedAt) {
			return billing.ErrState
		}
		if intent.Scope != fact.Scope {
			return recordPaymentRejection(ctx, tx, fact, &out, RejectScope)
		}
		if fact.Currency != intent.Currency {
			return recordPaymentRejection(ctx, tx, fact, &out, RejectCurrency)
		}
		if !fact.Paid() && fact.OccurredAt.Before(intent.CreatedAt) {
			return recordPaymentRejection(ctx, tx, fact, &out, RejectCollectionTime)
		}
		if fact.Status != FactPaid && fact.Status != FactCompleted {
			return applyNonPaid(ctx, tx, fact, intent, &out, now)
		}
		return applyPaid(ctx, tx, fact, intent, &out, now)
	})
	if err != nil {
		return PaymentResult{}, err
	}
	return out, nil
}

func (s *Service) Funding(ctx context.Context, account billing.AccountID, scope billing.Scope, txid string) (Funding, error) {
	if err := ctx.Err(); err != nil {
		return Funding{}, err
	}
	if !billing.ValidID(string(account)) || !scope.Valid() || !billing.ValidID(txid) {
		return Funding{}, billing.ErrInvalid
	}
	var out Funding
	err := s.repo.WithinAccount(ctx, account, func(tx Tx) error {
		v, err := tx.Funding(ctx, scope, txid)
		if err != nil {
			return err
		}
		if v.Account != account || v.Scope != scope || v.TransactionID != txid {
			return billing.ErrState
		}
		if v.Validate() != nil {
			return billing.ErrState
		}
		out = normalizeFunding(v)
		return nil
	})
	if err != nil {
		return Funding{}, err
	}
	return out, nil
}

func recordPaymentRejection(ctx context.Context, tx LifecycleTx, fact PaymentFact, result *PaymentResult, reason string) error {
	result.Rejection = reason
	return tx.InsertPaymentEvent(ctx, PaymentRecord{Fact: clonePaymentFact(fact), Result: *result})
}

func applyNonPaid(ctx context.Context, tx Tx, fact PaymentFact, intent Intent, result *PaymentResult, now time.Time) error {
	if intent.Payment == PaymentPaid {
		return recordPaymentRejection(ctx, tx, fact, result, RejectStaleObservation)
	}
	if fact.OccurredAt.Before(intent.LastPaymentAt) {
		return recordPaymentRejection(ctx, tx, fact, result, RejectStaleObservation)
	}
	if fact.OccurredAt.Equal(intent.LastPaymentAt) && intent.LastPaymentEventID != "" && intent.LastPaymentEventID != fact.EventID && paymentState(fact.Status) != intent.Payment {
		return recordPaymentRejection(ctx, tx, fact, result, RejectEqualTimeConflict)
	}
	intent.Payment = paymentState(fact.Status)
	intent.LastPaymentAt = fact.OccurredAt
	intent.LastPaymentEventID = fact.EventID
	intent.UpdatedAt = now
	if intent.Revision == math.MaxInt64 {
		return billing.ErrOverflow
	}
	intent.Revision++
	if err := tx.SaveIntent(ctx, intent, intent.Revision-1); err != nil {
		return err
	}
	result.Applied = true
	result.TransactionID = fact.TransactionID
	return tx.InsertPaymentEvent(ctx, PaymentRecord{Fact: clonePaymentFact(fact), Result: *result})
}

func applyPaid(ctx context.Context, tx Tx, fact PaymentFact, intent Intent, result *PaymentResult, now time.Time) error {
	quote, err := tx.Quote(ctx, intent.QuoteID)
	if err != nil {
		return err
	}
	if quote.Validate() != nil || quote.Account != intent.Account || quote.ID != intent.QuoteID || quote.Fingerprint() != intent.QuoteFingerprint {
		return billing.ErrState
	}
	// The provider's figures are taken as given. It owns the price, the
	// tax, the discount and the collection, and the customer agreed to
	// them on its checkout. What they bought is known from the intent, so
	// there is nothing here to recompute and nothing to compare against.
	//
	// The quote is still read above, because it decides what the money
	// buys; it is not a second opinion on the amount.
	if old, err := tx.Funding(ctx, fact.Scope, fact.TransactionID); err == nil {
		if old.Validate() != nil || old.Account != fact.Account || old.Scope != fact.Scope || old.TransactionID != fact.TransactionID {
			return billing.ErrState
		}
		old = normalizeFunding(old)
		if old.IntentID != intent.ID {
			return recordPaymentRejection(ctx, tx, fact, result, RejectTransactionOwner)
		}
		if intent.Payment != PaymentPaid || intent.TransactionID != old.TransactionID || !intent.PaidAt.Equal(old.PaidAt) {
			return billing.ErrState
		}
		if !fact.CollectedAt.Equal(old.PaidAt) || old.Account != fact.Account || old.Scope != fact.Scope || old.TransactionID != fact.TransactionID || old.Currency != fact.Currency || old.Gross != fact.Gross || old.Tax != fact.Tax || !equalPaidLines(old.Lines, fact.Lines) {
			return recordPaymentRejection(ctx, tx, fact, result, RejectAllocation)
		}
		result.Applied = true
		result.TransactionID = fact.TransactionID
		return tx.InsertPaymentEvent(ctx, PaymentRecord{Fact: clonePaymentFact(fact), Result: *result})
	} else if !errors.Is(err, billing.ErrNotFound) {
		return err
	} else if intent.Payment == PaymentPaid {
		return recordPaymentRejection(ctx, tx, fact, result, RejectAlreadyFunded)
	} else {
		// A collection cannot predate the intent it funds: that is not a
		// late payment, it is a payment for something else.
		//
		// There is deliberately no upper bound. The intent expires with
		// the quote, thirty minutes, and that window is how long the price
		// we showed stands — not how long the customer has to finish
		// paying. A card that goes to a 3-D Secure challenge while its
		// owner looks for their phone, or a bank redirect, routinely
		// settles later than that, and refusing it means the provider
		// took the money and we gave them nothing. That is the failure
		// this system has already had twice, from other causes.
		//
		// What the window was guarding is covered elsewhere: the
		// transaction is bound to this intent by the provider, the intent
		// can only be funded once, and a payment that arrives for an
		// already-funded intent is refused above.
		if fact.CollectedAt.Before(intent.CreatedAt) {
			return recordPaymentRejection(ctx, tx, fact, result, RejectCollectionTime)
		}
		if err := tx.InsertFunding(ctx, Funding{Account: fact.Account, Scope: fact.Scope, TransactionID: fact.TransactionID, IntentID: intent.ID, Currency: fact.Currency, Gross: fact.Gross, Tax: fact.Tax, Discount: fact.Discount, PaidAt: fact.CollectedAt, Lines: copyPaidLines(fact.Lines)}); err != nil {
			return err
		}
	}
	intent.Payment = PaymentPaid
	intent.PaidAt = fact.CollectedAt
	intent.LastPaymentAt = fact.OccurredAt
	intent.LastPaymentEventID = fact.EventID
	intent.TransactionID = fact.TransactionID
	intent.UpdatedAt = now
	if intent.Revision == math.MaxInt64 {
		return billing.ErrOverflow
	}
	intent.Revision++
	complete, err := applyFulfillments(ctx, tx, intent, quote, Funding{Account: fact.Account, Scope: fact.Scope, TransactionID: fact.TransactionID, IntentID: intent.ID, Currency: fact.Currency, Gross: fact.Gross, Tax: fact.Tax, Discount: fact.Discount, PaidAt: fact.CollectedAt, Lines: fact.Lines}, now)
	if err != nil {
		return err
	}
	if complete {
		intent.Fulfillment = FulfillmentComplete
	} else {
		intent.Fulfillment = FulfillmentPending
	}

	if err := tx.SaveIntent(ctx, intent, intent.Revision-1); err != nil {
		return err
	}
	result.Applied = true
	result.TransactionID = fact.TransactionID
	return tx.InsertPaymentEvent(ctx, PaymentRecord{Fact: clonePaymentFact(fact), Result: *result})
}

func copyPaidLines(in []PaidLine) []PaidLine { return slices.Clone(in) }
func paymentState(s PaymentFactStatus) PaymentState {
	switch s {
	case FactActionRequired:
		return PaymentActionRequired
	case FactFailed:
		return PaymentFailed
	default:
		return PaymentPending
	}
}
func commandTransition(from, to CommandState) error {
	if from == CommandPlanned && (to == CommandDispatched || to == CommandUnknown || to == CommandRejected || to == CommandReconciled) {
		return nil
	}
	if from == CommandDispatched && (to == CommandAccepted || to == CommandRejected || to == CommandUnknown || to == CommandReconciled) {
		return nil
	}
	if from == CommandUnknown && (to == CommandUnknown || to == CommandRejected || to == CommandReconciled) {
		return nil
	}
	return billing.ErrState
}

func commandTransitionRequiresEvidence(from, to CommandState) bool {
	return to == CommandReconciled ||
		from == CommandPlanned && (to == CommandUnknown || to == CommandRejected) ||
		from == CommandUnknown && (to == CommandUnknown || to == CommandRejected)
}
func equalPaidLines(a, b []PaidLine) bool { return slices.Equal(a, b) }

func sameIntentInput(a, b IntentInput) bool {
	a.ExpiresAt = billing.CanonicalTime(a.ExpiresAt)
	b.ExpiresAt = billing.CanonicalTime(b.ExpiresAt)
	return a.Account == b.Account && a.ID == b.ID && a.Operation == b.Operation && a.QuoteID == b.QuoteID && a.QuoteFingerprint == b.QuoteFingerprint && a.Scope == b.Scope && a.Actor == b.Actor && a.Reason == b.Reason && a.ExpiresAt.Equal(b.ExpiresAt)
}
