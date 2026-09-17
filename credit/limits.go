package credit

import (
	"context"
	"errors"
	"strconv"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/internal/checked"
	"github.com/data-insights-ai/rho-billing/internal/identity"
)

type Basis string

const (
	BasisBillable Basis = "billable"
	BasisInternal Basis = "internal"
	BasisTopUp    Basis = "topup"
)

type HoldState string

const (
	HoldHeld     HoldState = "held"
	HoldSettled  HoldState = "settled"
	HoldReleased HoldState = "released"
)

type Budget struct {
	Account  billing.AccountID
	ID       string
	Actor    string
	Project  string
	Basis    Basis
	Currency string
	Period   billing.Period
	Amount   int64
}

type Hold struct {
	Account  billing.AccountID
	ID       string
	GroupID  string
	BudgetID string
	Actor    string
	Project  string
	Basis    Basis
	Currency string
	Amount   int64
	Settled  int64
	State    HoldState
}

type HoldSet struct {
	GroupID string
	Bound   int64
	Holds   []Hold
}

type BudgetInput struct {
	Account  billing.AccountID
	ID       string
	Actor    string
	Project  string
	Basis    Basis
	Currency string
	Period   billing.Period
	Amount   int64
}

type BudgetReserveInput struct {
	Account  billing.AccountID
	GroupID  string
	Actor    string
	Project  string
	Basis    Basis
	Currency string
	Amount   int64
	Period   billing.Period
}

type BudgetSettleInput struct {
	Account billing.AccountID
	GroupID string
	Actual  int64
}

type BudgetReleaseInput struct {
	Account billing.AccountID
	GroupID string
}

type HybridPolicy struct {
	Enabled  bool
	MaxDebt  int64
	Basis    Basis
	Currency string
}

type HybridBudgetReserveInput struct {
	BudgetReserveInput
	PrepaidAvailable int64
	Policy           HybridPolicy
}

type RateLimitInput struct {
	Key    string
	Window billing.Period
	Limit  int64
}

// Allow must not debit billable usage or create budget holds.
type RateLimiter interface {
	Allow(context.Context, RateLimitInput) error
}

type LimitRepository interface {
	WithinAccount(context.Context, billing.AccountID, func(LimitTx) error) error
}

type LimitTx interface {
	Budget(context.Context, string) (Budget, error)
	SaveBudget(context.Context, Budget) error
	Budgets(context.Context) ([]Budget, error)
	Hold(context.Context, string) (Hold, error)
	HoldsByGroup(context.Context, string) ([]Hold, error)
	HoldsByBudget(context.Context, string) ([]Hold, error)
	SaveHold(context.Context, Hold) error
}

type LimitService struct {
	repo    LimitRepository
	limiter RateLimiter
	now     func() time.Time
}

func NewLimits(repo LimitRepository, now func() time.Time) *LimitService {
	if repo == nil {
		panic("credit: nil limit repository")
	}
	if now == nil {
		now = time.Now
	}
	return &LimitService{repo: repo, now: now}
}

func (s *LimitService) WithRateLimiter(limiter RateLimiter) *LimitService {
	s.limiter = limiter
	return s
}

func (s *LimitService) Configure(ctx context.Context, in BudgetInput) (Budget, error) {
	budget, err := prepareBudget(in)
	if err != nil {
		return Budget{}, err
	}
	var out Budget
	err = s.repo.WithinAccount(ctx, in.Account, func(tx LimitTx) error {
		old, err := tx.Budget(ctx, budget.ID)
		if err == nil {
			if budgetIdentity(old) != budgetIdentity(budget) {
				return billing.ErrConflict
			}
			out = old
			return nil
		}
		if !errors.Is(err, billing.ErrNotFound) {
			return err
		}
		if err := tx.SaveBudget(ctx, budget); err != nil {
			return err
		}
		out = budget
		return nil
	})
	if err != nil {
		return Budget{}, err
	}
	return out, nil
}

func (s *LimitService) Reserve(ctx context.Context, in BudgetReserveInput) (HoldSet, error) {
	if err := ctx.Err(); err != nil {
		return HoldSet{}, err
	}
	return s.reserve(ctx, in)
}

func (s *LimitService) ReserveHybrid(ctx context.Context, in HybridBudgetReserveInput) (HoldSet, error) {
	if err := ctx.Err(); err != nil {
		return HoldSet{}, err
	}
	if in.PrepaidAvailable < 0 {
		return HoldSet{}, billing.ErrInvalid
	}
	debt := in.Amount - in.PrepaidAvailable
	if debt <= 0 {
		return HoldSet{GroupID: in.GroupID, Bound: 0}, nil
	}
	if !in.Policy.Enabled {
		return HoldSet{}, billing.ErrInsufficient
	}
	if in.Policy.MaxDebt < 0 || (in.Policy.Currency != "" && in.Policy.Currency != in.Currency) || (in.Policy.Basis != "" && in.Policy.Basis != in.Basis) {
		return HoldSet{}, billing.ErrInvalid
	}
	if debt > in.Policy.MaxDebt {
		return HoldSet{}, billing.ErrLimit
	}
	in.Amount = debt
	return s.reserve(ctx, in.BudgetReserveInput)
}

func (s *LimitService) Authorize(ctx context.Context, in BudgetReserveInput, limit RateLimitInput) (HoldSet, error) {
	if err := ctx.Err(); err != nil {
		return HoldSet{}, err
	}
	if s.limiter != nil {
		if err := s.limiter.Allow(ctx, limit); err != nil {
			return HoldSet{}, err
		}
	}
	return s.reserve(ctx, in)
}

func (s *LimitService) reserve(ctx context.Context, in BudgetReserveInput) (HoldSet, error) {
	in.Period.Start = billing.CanonicalTime(in.Period.Start)
	in.Period.End = billing.CanonicalTime(in.Period.End)
	if !billing.ValidID(string(in.Account)) || !billing.ValidID(in.GroupID) || !validBasis(in.Basis) || !validCurrency(in.Currency) || in.Amount <= 0 || !in.Period.Valid() {
		return HoldSet{}, billing.ErrInvalid
	}
	if in.Actor != "" && !billing.ValidID(in.Actor) {
		return HoldSet{}, billing.ErrInvalid
	}
	if in.Project != "" && !billing.ValidID(in.Project) {
		return HoldSet{}, billing.ErrInvalid
	}
	var out HoldSet
	err := s.repo.WithinAccount(ctx, in.Account, func(tx LimitTx) error {
		existing, err := tx.HoldsByGroup(ctx, in.GroupID)
		if err != nil {
			return err
		}
		if len(existing) > 0 {
			if existing[0].Amount != in.Amount || existing[0].Basis != in.Basis || existing[0].Currency != in.Currency || existing[0].Actor != in.Actor || existing[0].Project != in.Project {
				return billing.ErrConflict
			}
			// Only a still-held group is a live authorization. Returning a
			// settled or released group as a fresh Bound hands back budget
			// exposure that was already accounted for and released, so the same
			// units could be authorized twice without any limit check.
			for _, hold := range existing {
				if hold.State != HoldHeld {
					return billing.ErrState
				}
			}
			out = HoldSet{GroupID: in.GroupID, Bound: in.Amount, Holds: existing}
			return nil
		}
		budgets, err := tx.Budgets(ctx)
		if err != nil {
			return err
		}
		matched := matchingBudgets(budgets, in)
		if len(matched) == 0 {
			return billing.ErrNotFound
		}
		holds := make([]Hold, 0, len(matched))
		for _, budget := range matched {
			used, err := exposure(ctx, tx, budget.ID)
			if err != nil {
				return err
			}
			next, err := checked.Add(used, in.Amount)
			if err != nil {
				return err
			}
			if next > budget.Amount {
				return billing.ErrLimit
			}
			hold := Hold{
				Account: in.Account, ID: holdID(in.GroupID, budget.ID), GroupID: in.GroupID, BudgetID: budget.ID,
				Actor: in.Actor, Project: in.Project, Basis: in.Basis, Currency: in.Currency, Amount: in.Amount, State: HoldHeld,
			}
			if err := tx.SaveHold(ctx, hold); err != nil {
				return err
			}
			holds = append(holds, hold)
		}
		out = HoldSet{GroupID: in.GroupID, Bound: in.Amount, Holds: holds}
		return nil
	})
	if err != nil {
		return HoldSet{}, err
	}
	return out, nil
}

func (s *LimitService) Settle(ctx context.Context, in BudgetSettleInput) (HoldSet, error) {
	if !billing.ValidID(string(in.Account)) || !billing.ValidID(in.GroupID) || in.Actual < 0 {
		return HoldSet{}, billing.ErrInvalid
	}
	var out HoldSet
	err := s.repo.WithinAccount(ctx, in.Account, func(tx LimitTx) error {
		holds, err := tx.HoldsByGroup(ctx, in.GroupID)
		if err != nil {
			return err
		}
		if len(holds) == 0 {
			return billing.ErrNotFound
		}
		updated := make([]Hold, 0, len(holds))
		for _, hold := range holds {
			if hold.State == HoldSettled && hold.Settled == in.Actual {
				updated = append(updated, hold)
				continue
			}
			if hold.State != HoldHeld {
				return billing.ErrState
			}
			if in.Actual > hold.Amount {
				return billing.ErrLimit
			}
			hold.Settled = in.Actual
			hold.State = HoldSettled
			if err := tx.SaveHold(ctx, hold); err != nil {
				return err
			}
			updated = append(updated, hold)
		}
		out = HoldSet{GroupID: in.GroupID, Bound: updated[0].Amount, Holds: updated}
		return nil
	})
	if err != nil {
		return HoldSet{}, err
	}
	return out, nil
}

func (s *LimitService) Release(ctx context.Context, in BudgetReleaseInput) (HoldSet, error) {
	if !billing.ValidID(string(in.Account)) || !billing.ValidID(in.GroupID) {
		return HoldSet{}, billing.ErrInvalid
	}
	var out HoldSet
	err := s.repo.WithinAccount(ctx, in.Account, func(tx LimitTx) error {
		holds, err := tx.HoldsByGroup(ctx, in.GroupID)
		if err != nil {
			return err
		}
		if len(holds) == 0 {
			return billing.ErrNotFound
		}
		updated := make([]Hold, 0, len(holds))
		for _, hold := range holds {
			if hold.State == HoldReleased {
				updated = append(updated, hold)
				continue
			}
			if hold.State != HoldHeld {
				return billing.ErrState
			}
			hold.State = HoldReleased
			if err := tx.SaveHold(ctx, hold); err != nil {
				return err
			}
			updated = append(updated, hold)
		}
		out = HoldSet{GroupID: in.GroupID, Holds: updated}
		return nil
	})
	if err != nil {
		return HoldSet{}, err
	}
	return out, nil
}

func prepareBudget(in BudgetInput) (Budget, error) {
	in.Period.Start = billing.CanonicalTime(in.Period.Start)
	in.Period.End = billing.CanonicalTime(in.Period.End)
	if !billing.ValidID(string(in.Account)) || !billing.ValidID(in.ID) || !validBasis(in.Basis) || !validCurrency(in.Currency) || in.Amount < 0 || !in.Period.Valid() {
		return Budget{}, billing.ErrInvalid
	}
	if in.Actor != "" && !billing.ValidID(in.Actor) {
		return Budget{}, billing.ErrInvalid
	}
	if in.Project != "" && !billing.ValidID(in.Project) {
		return Budget{}, billing.ErrInvalid
	}
	return Budget{Account: in.Account, ID: in.ID, Actor: in.Actor, Project: in.Project, Basis: in.Basis, Currency: in.Currency, Period: in.Period, Amount: in.Amount}, nil
}

func matchingBudgets(budgets []Budget, in BudgetReserveInput) []Budget {
	matched := make([]Budget, 0, len(budgets))
	for _, budget := range budgets {
		if budget.Basis != in.Basis || budget.Currency != in.Currency {
			continue
		}
		if budget.Actor != "" && budget.Actor != in.Actor {
			continue
		}
		if budget.Project != "" && budget.Project != in.Project {
			continue
		}
		if !budget.Period.Start.Before(in.Period.End) || !in.Period.Start.Before(budget.Period.End) {
			continue
		}
		matched = append(matched, budget)
	}
	return matched
}

func exposure(ctx context.Context, tx LimitTx, budgetID string) (int64, error) {
	holds, err := tx.HoldsByBudget(ctx, budgetID)
	if err != nil {
		return 0, err
	}
	var used int64
	for _, hold := range holds {
		switch hold.State {
		case HoldHeld:
			used, err = checked.Add(used, hold.Amount)
		case HoldSettled:
			used, err = checked.Add(used, hold.Settled)
		default:
			continue
		}
		if err != nil {
			return 0, err
		}
	}
	return used, nil
}

func holdID(groupID, budgetID string) string {
	return identity.Fingerprint("hold", groupID, budgetID)
}

func budgetIdentity(b Budget) string {
	return identity.Fingerprint(string(b.Account), b.ID, b.Actor, b.Project, string(b.Basis), b.Currency, identity.Instant(b.Period.Start), identity.Instant(b.Period.End), strconv.FormatInt(b.Amount, 10))
}

func validBasis(b Basis) bool {
	return b == BasisBillable || b == BasisInternal || b == BasisTopUp
}

func validCurrency(currency string) bool {
	if len(currency) != 3 {
		return false
	}
	for i := range currency {
		if currency[i] < 'A' || currency[i] > 'Z' {
			return false
		}
	}
	return true
}
