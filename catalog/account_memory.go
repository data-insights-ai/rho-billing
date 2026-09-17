package catalog

import (
	"context"
	"sync"

	billing "github.com/data-insights-ai/rho-billing"
)

type refKey struct {
	Ref  billing.Reference
	Kind string
}

func key(r AccountReference) refKey {
	return refKey{Ref: r.Ref, Kind: r.Kind}
}

type memoryRepository struct {
	mu       sync.Mutex
	accounts map[billing.AccountID]Account
	subjects map[string]billing.AccountID
	refs     map[refKey]billing.AccountID
}

func NewMemoryAccountRepository() AccountRepository {
	return &memoryRepository{accounts: map[billing.AccountID]Account{}, subjects: map[string]billing.AccountID{}, refs: map[refKey]billing.AccountID{}}
}
func (r *memoryRepository) CreateAccount(ctx context.Context, id billing.AccountID, subject string) error {
	if !billing.ValidID(string(id)) || !billing.ValidID(subject) {
		return billing.ErrInvalid
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if a, ok := r.accounts[id]; ok {
		if a.Subject != subject {
			return billing.ErrConflict
		}
		return nil
	}
	if _, ok := r.subjects[subject]; ok {
		return billing.ErrConflict
	}
	r.accounts[id] = Account{id, subject}
	r.subjects[subject] = id
	return nil
}
func (r *memoryRepository) Account(ctx context.Context, id billing.AccountID) (Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Account{}, err
	}
	a, ok := r.accounts[id]
	if !ok {
		return Account{}, billing.ErrNotFound
	}
	return a, nil
}
func (r *memoryRepository) Link(ctx context.Context, ref AccountReference) error {
	if !ref.Valid() {
		return billing.ErrInvalid
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, ok := r.accounts[ref.Account]; !ok {
		return billing.ErrNotFound
	}
	k := key(ref)
	if a, ok := r.refs[k]; ok && a != ref.Account {
		return billing.ErrConflict
	}
	r.refs[k] = ref.Account
	return nil
}

func (r *memoryRepository) Resolve(ctx context.Context, ref AccountReference) (Account, error) {
	if !ref.Valid() {
		return Account{}, billing.ErrInvalid
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Account{}, err
	}
	a, ok := r.refs[key(ref)]
	if !ok || a != ref.Account {
		return Account{}, billing.ErrNotFound
	}
	return r.accounts[a], nil
}
