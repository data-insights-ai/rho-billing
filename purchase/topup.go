package purchase

import (
	"context"
	"errors"
	"strconv"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/internal/checked"
	"github.com/data-insights-ai/rho-billing/internal/identity"
)

type TopUpAttemptState string

const (
	TopUpPlanned        TopUpAttemptState = "planned"
	TopUpDispatched     TopUpAttemptState = "dispatched"
	TopUpUnknown        TopUpAttemptState = "unknown"
	TopUpActionRequired TopUpAttemptState = "action_required"
	TopUpRejected       TopUpAttemptState = "rejected"
	TopUpGranted        TopUpAttemptState = "granted"
)

type TopUpPolicy struct {
	Account         billing.AccountID
	ID              string
	ConsentRevision int64
	ConsentedAt     time.Time
	ConsentActor    string
	Threshold       int64
	Cooldown        time.Duration
	PurchaseCap     int64
	Currency        string
	Amount          int64
	Unit            billing.Unit
	Enabled         bool
}

type TopUpAttempt struct {
	Account   billing.AccountID
	ID        string
	PolicyID  string
	IntentID  string
	State     TopUpAttemptState
	Amount    int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

type TopUpTx interface {
	TopUpPolicy(context.Context, string) (TopUpPolicy, error)
	SaveTopUpPolicy(context.Context, TopUpPolicy) error
	TopUpAttempt(context.Context, string) (TopUpAttempt, error)
	ActiveTopUpAttempt(context.Context, string) (TopUpAttempt, error)
	SaveTopUpAttempt(context.Context, TopUpAttempt) error
	TopUpAttempts(context.Context, string) ([]TopUpAttempt, error)
}

func (s *Service) ConfigureTopUp(ctx context.Context, policy TopUpPolicy) (TopUpPolicy, error) {
	policy.ConsentedAt = billing.CanonicalTime(policy.ConsentedAt)
	if err := validateTopUpPolicy(policy); err != nil {
		return TopUpPolicy{}, err
	}
	var out TopUpPolicy
	err := s.repo.WithinAccount(ctx, policy.Account, func(tx Tx) error {
		old, err := tx.TopUpPolicy(ctx, policy.ID)
		if err == nil {
			if topUpPolicyIdentity(old) != topUpPolicyIdentity(policy) {
				return billing.ErrConflict
			}
			out = old
			return nil
		}
		if !errors.Is(err, billing.ErrNotFound) {
			return err
		}
		if err := tx.SaveTopUpPolicy(ctx, policy); err != nil {
			return err
		}
		out = policy
		return nil
	})
	if err != nil {
		return TopUpPolicy{}, err
	}
	return out, nil
}

func (s *Service) EvaluateTopUp(ctx context.Context, account billing.AccountID, policyID string, available int64) (TopUpAttempt, error) {
	if !billing.ValidID(string(account)) || !billing.ValidID(policyID) || available < 0 {
		return TopUpAttempt{}, billing.ErrInvalid
	}
	now := billing.CanonicalTime(s.now())
	var out TopUpAttempt
	err := s.repo.WithinAccount(ctx, account, func(tx Tx) error {
		policy, err := tx.TopUpPolicy(ctx, policyID)
		if err != nil {
			return err
		}
		if !policy.Enabled || policy.ConsentRevision <= 0 || policy.ConsentedAt.IsZero() {
			return billing.ErrInvalid
		}
		active, err := tx.ActiveTopUpAttempt(ctx, policyID)
		if err == nil {
			if active.State == TopUpUnknown || active.State == TopUpActionRequired || active.State == TopUpDispatched || active.State == TopUpPlanned {
				out = active
				return billing.ErrConflict
			}
		} else if !errors.Is(err, billing.ErrNotFound) {
			return err
		}
		if available > policy.Threshold {
			return billing.ErrState
		}
		attempts, err := tx.TopUpAttempts(ctx, policyID)
		if err != nil {
			return err
		}
		var spent int64
		for _, attempt := range attempts {
			if attempt.State == TopUpRejected {
				continue
			}
			if policy.Cooldown > 0 && now.Sub(attempt.CreatedAt) < policy.Cooldown && attempt.State != TopUpRejected {
				return billing.ErrState
			}
			if attempt.State == TopUpGranted || attempt.State == TopUpDispatched || attempt.State == TopUpPlanned {
				next, err := checked.Add(spent, attempt.Amount)
				if err != nil {
					return err
				}
				spent = next
			}
		}
		next, err := checked.Add(spent, policy.Amount)
		if err != nil {
			return err
		}
		if next > policy.PurchaseCap {
			return billing.ErrLimit
		}
		out = TopUpAttempt{Account: account, ID: identity.Fingerprint("topup-attempt", policyID, identity.Instant(now)), PolicyID: policyID, State: TopUpPlanned, Amount: policy.Amount, CreatedAt: now, UpdatedAt: now}
		return tx.SaveTopUpAttempt(ctx, out)
	})
	if err != nil {
		return TopUpAttempt{}, err
	}
	return out, nil
}

func (s *Service) DispatchTopUp(ctx context.Context, account billing.AccountID, attemptID, intentID string) (TopUpAttempt, error) {
	return s.transitionTopUp(ctx, account, attemptID, func(_ Tx, attempt TopUpAttempt) (TopUpAttempt, error) {
		if attempt.State != TopUpPlanned {
			return TopUpAttempt{}, billing.ErrState
		}
		if !billing.ValidID(intentID) {
			return TopUpAttempt{}, billing.ErrInvalid
		}
		attempt.IntentID = intentID
		attempt.State = TopUpDispatched
		return attempt, nil
	})
}

func (s *Service) ObserveTopUp(ctx context.Context, account billing.AccountID, attemptID string, state TopUpAttemptState) (TopUpAttempt, error) {
	if state != TopUpUnknown && state != TopUpActionRequired && state != TopUpRejected {
		return TopUpAttempt{}, billing.ErrInvalid
	}
	return s.transitionTopUp(ctx, account, attemptID, func(tx Tx, attempt TopUpAttempt) (TopUpAttempt, error) {
		if attempt.State != TopUpDispatched && attempt.State != TopUpUnknown {
			return TopUpAttempt{}, billing.ErrState
		}
		// A rejection removes the attempt from consented purchase-cap
		// accounting and releases the active-attempt guard, so a stale
		// provider failure must never reject an attempt whose intent was
		// paid. That money is owed as credits, not written off.
		if state == TopUpRejected && attempt.IntentID != "" {
			intent, err := tx.Intent(ctx, attempt.IntentID)
			if err != nil && !errors.Is(err, billing.ErrNotFound) {
				return TopUpAttempt{}, err
			}
			if err == nil && intent.Payment == PaymentPaid {
				return TopUpAttempt{}, billing.ErrConflict
			}
		}
		attempt.State = state
		return attempt, nil
	})
}

func (s *Service) GrantTopUp(ctx context.Context, account billing.AccountID, attemptID string) (TopUpAttempt, error) {
	if !billing.ValidID(string(account)) || !billing.ValidID(attemptID) {
		return TopUpAttempt{}, billing.ErrInvalid
	}
	now := billing.CanonicalTime(s.now())
	var out TopUpAttempt
	err := s.repo.WithinAccount(ctx, account, func(tx Tx) error {
		attempt, err := tx.TopUpAttempt(ctx, attemptID)
		if err != nil {
			return err
		}
		if attempt.Account != account {
			return billing.ErrConflict
		}
		if attempt.State == TopUpGranted {
			out = attempt
			return nil
		}
		if attempt.State != TopUpDispatched || !billing.ValidID(attempt.IntentID) {
			return billing.ErrState
		}
		intent, err := tx.Intent(ctx, attempt.IntentID)
		if err != nil {
			return err
		}
		if intent.Payment != PaymentPaid || intent.Amount != attempt.Amount {
			return billing.ErrState
		}
		attempt.State = TopUpGranted
		attempt.UpdatedAt = now
		if err := tx.SaveTopUpAttempt(ctx, attempt); err != nil {
			return err
		}
		out = attempt
		return nil
	})
	if err != nil {
		return TopUpAttempt{}, err
	}
	return out, nil
}

func (s *Service) transitionTopUp(ctx context.Context, account billing.AccountID, attemptID string, mutate func(Tx, TopUpAttempt) (TopUpAttempt, error)) (TopUpAttempt, error) {
	if !billing.ValidID(string(account)) || !billing.ValidID(attemptID) {
		return TopUpAttempt{}, billing.ErrInvalid
	}
	now := billing.CanonicalTime(s.now())
	var out TopUpAttempt
	err := s.repo.WithinAccount(ctx, account, func(tx Tx) error {
		attempt, err := tx.TopUpAttempt(ctx, attemptID)
		if err != nil {
			return err
		}
		if attempt.Account != account {
			return billing.ErrConflict
		}
		next, err := mutate(tx, attempt)
		if err != nil {
			return err
		}
		next.UpdatedAt = now
		if err := tx.SaveTopUpAttempt(ctx, next); err != nil {
			return err
		}
		out = next
		return nil
	})
	if err != nil {
		return TopUpAttempt{}, err
	}
	return out, nil
}

func validateTopUpPolicy(p TopUpPolicy) error {
	if !billing.ValidID(string(p.Account)) || !billing.ValidID(p.ID) || !billing.ValidID(p.ConsentActor) || p.ConsentRevision <= 0 || p.ConsentedAt.IsZero() || p.Threshold < 0 || p.Cooldown < 0 || p.PurchaseCap <= 0 || p.Amount <= 0 || p.Amount > p.PurchaseCap || !p.Unit.Valid() {
		return billing.ErrInvalid
	}
	if len(p.Currency) != 3 {
		return billing.ErrInvalid
	}
	return nil
}

func topUpPolicyIdentity(p TopUpPolicy) string {
	return identity.Fingerprint(string(p.Account), p.ID, strconv.FormatInt(p.ConsentRevision, 10), identity.Instant(p.ConsentedAt), p.ConsentActor, strconv.FormatInt(p.Threshold, 10), p.Cooldown.String(), strconv.FormatInt(p.PurchaseCap, 10), p.Currency, strconv.FormatInt(p.Amount, 10), p.Unit.Code, strconv.FormatInt(p.Unit.Scale, 10), strconv.FormatBool(p.Enabled))
}
