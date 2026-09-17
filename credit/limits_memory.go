package credit

import (
	"cmp"
	"context"
	"maps"
	"slices"
	"sync"

	billing "github.com/data-insights-ai/rho-billing"
)

type limitMemoryRepository struct {
	mu       sync.Mutex
	accounts map[billing.AccountID]*limitMemoryAccount
}

type limitMemoryAccount struct {
	budgets map[string]Budget
	holds   map[string]Hold
}

func NewMemoryLimitRepository(accounts ...billing.AccountID) LimitRepository {
	r := &limitMemoryRepository{accounts: make(map[billing.AccountID]*limitMemoryAccount, len(accounts))}
	for _, account := range accounts {
		if !billing.ValidID(string(account)) {
			panic("credit: invalid test account")
		}
		if _, exists := r.accounts[account]; exists {
			panic("credit: duplicate test account")
		}
		r.accounts[account] = &limitMemoryAccount{budgets: map[string]Budget{}, holds: map[string]Hold{}}
	}
	return r
}

func (r *limitMemoryRepository) WithinAccount(ctx context.Context, account billing.AccountID, fn func(LimitTx) error) error {
	if fn == nil || !billing.ValidID(string(account)) {
		return billing.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	row, ok := r.accounts[account]
	if !ok {
		return billing.ErrNotFound
	}
	staged := &limitMemoryAccount{budgets: maps.Clone(row.budgets), holds: maps.Clone(row.holds)}
	if err := fn(&limitMemoryTx{account: account, state: staged}); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	r.accounts[account] = staged
	return nil
}

type limitMemoryTx struct {
	account billing.AccountID
	state   *limitMemoryAccount
}

func (tx *limitMemoryTx) Budget(ctx context.Context, id string) (Budget, error) {
	if err := ctx.Err(); err != nil {
		return Budget{}, err
	}
	b, ok := tx.state.budgets[id]
	if !ok {
		return Budget{}, billing.ErrNotFound
	}
	return b, nil
}

func (tx *limitMemoryTx) SaveBudget(ctx context.Context, budget Budget) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if budget.Account != tx.account {
		return billing.ErrConflict
	}
	if old, ok := tx.state.budgets[budget.ID]; ok && budgetIdentity(old) != budgetIdentity(budget) {
		return billing.ErrConflict
	}
	tx.state.budgets[budget.ID] = budget
	return nil
}

func (tx *limitMemoryTx) Budgets(ctx context.Context) ([]Budget, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make([]Budget, 0, len(tx.state.budgets))
	for _, b := range tx.state.budgets {
		out = append(out, b)
	}
	slices.SortFunc(out, func(a, b Budget) int { return cmp.Compare(a.ID, b.ID) })
	return out, nil
}

func (tx *limitMemoryTx) Hold(ctx context.Context, id string) (Hold, error) {
	if err := ctx.Err(); err != nil {
		return Hold{}, err
	}
	h, ok := tx.state.holds[id]
	if !ok {
		return Hold{}, billing.ErrNotFound
	}
	return h, nil
}

func (tx *limitMemoryTx) HoldsByGroup(ctx context.Context, groupID string) ([]Hold, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make([]Hold, 0)
	for _, h := range tx.state.holds {
		if h.GroupID == groupID {
			out = append(out, h)
		}
	}
	slices.SortFunc(out, func(a, b Hold) int { return cmp.Compare(a.ID, b.ID) })
	return out, nil
}

func (tx *limitMemoryTx) HoldsByBudget(ctx context.Context, budgetID string) ([]Hold, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make([]Hold, 0)
	for _, h := range tx.state.holds {
		if h.BudgetID == budgetID {
			out = append(out, h)
		}
	}
	return out, nil
}

func (tx *limitMemoryTx) SaveHold(ctx context.Context, hold Hold) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if hold.Account != tx.account {
		return billing.ErrConflict
	}
	tx.state.holds[hold.ID] = hold
	return nil
}
