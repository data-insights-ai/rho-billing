package usage

import (
	"cmp"
	"context"
	"slices"
	"sync"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"

	"github.com/data-insights-ai/rho-billing/subscription"
)

type MemoryConfig struct {
	Accounts      []billing.AccountID
	Rules         RuleResolver
	Subscriptions func(context.Context, billing.AccountID, billing.Reference) (subscription.Snapshot, error)
	Reservations  func(context.Context, billing.AccountID, string) (credit.Reservation, error)
}

type memoryRepository struct {
	mu       sync.Mutex
	accounts map[billing.AccountID]map[string]Record
	costs    map[billing.AccountID]map[string]CostRecord
	config   MemoryConfig
}

func NewMemoryRepository(config MemoryConfig) Repository {
	if config.Rules == nil {
		panic("usage: nil rule resolver")
	}
	r := &memoryRepository{accounts: make(map[billing.AccountID]map[string]Record), costs: make(map[billing.AccountID]map[string]CostRecord), config: config}
	for _, account := range config.Accounts {
		if !billing.ValidID(string(account)) {
			panic("usage: invalid test account")
		}
		if _, exists := r.accounts[account]; exists {
			panic("usage: duplicate test account")
		}
		r.accounts[account] = make(map[string]Record)
		r.costs[account] = make(map[string]CostRecord)
	}
	return r
}

func (r *memoryRepository) WithinAccount(ctx context.Context, account billing.AccountID, fn func(Tx) error) error {
	if fn == nil {
		return billing.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !billing.ValidID(string(account)) {
		return billing.ErrInvalid
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	rows, ok := r.accounts[account]
	if !ok {
		return billing.ErrNotFound
	}
	staged := cloneRows(rows)
	if r.costs[account] == nil {
		r.costs[account] = make(map[string]CostRecord)
	}
	stagedCosts := cloneCosts(r.costs[account])
	tx := &memoryTx{repo: r, account: account, rows: staged, costs: stagedCosts}
	if err := fn(tx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	r.accounts[account] = staged
	r.costs[account] = stagedCosts
	return nil
}

type memoryTx struct {
	repo    *memoryRepository
	account billing.AccountID
	rows    map[string]Record
	costs   map[string]CostRecord
}

func (tx *memoryTx) Record(ctx context.Context, id string) (Record, error) {
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	record, ok := tx.rows[id]
	if !ok {
		return Record{}, billing.ErrNotFound
	}
	return Copy(record), nil
}

func (tx *memoryTx) Rule(ctx context.Context, version string) (*Rule, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return tx.repo.config.Rules(ctx, version)
}

func (tx *memoryTx) Subscription(ctx context.Context, reference billing.Reference) (subscription.Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return subscription.Snapshot{}, err
	}
	if tx.repo.config.Subscriptions == nil {
		return subscription.Snapshot{}, billing.ErrNotFound
	}
	return tx.repo.config.Subscriptions(ctx, tx.account, reference)
}

func (tx *memoryTx) Reservation(ctx context.Context, id string) (credit.Reservation, error) {
	if err := ctx.Err(); err != nil {
		return credit.Reservation{}, err
	}
	if tx.repo.config.Reservations == nil {
		return credit.Reservation{}, billing.ErrNotFound
	}
	return tx.repo.config.Reservations(ctx, tx.account, id)
}

func (tx *memoryTx) Candidates(ctx context.Context, target Observation, after string, limit int) ([]Record, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit < 1 || limit > 1000 {
		return nil, billing.ErrInvalid
	}
	if target.Account != tx.account {
		return nil, billing.ErrConflict
	}
	rows := make([]Record, 0, len(tx.rows))
	for _, row := range tx.rows {
		if row.Observation.ID <= after || row.Observation.Account != tx.account || row.Observation.Source != target.Source || row.Observation.Scope != target.Scope || !candidateTemporalIntersection(target, row.Observation) {
			continue
		}
		rows = append(rows, Copy(row))
	}
	slices.SortFunc(rows, func(a, b Record) int {
		return cmp.Compare(a.Observation.ID, b.Observation.ID)
	})
	return rows[:min(limit, len(rows))], nil
}

func (tx *memoryTx) Save(ctx context.Context, record Record) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if record.Observation.Account != tx.account {
		return billing.ErrConflict
	}
	if _, exists := tx.rows[record.Observation.ID]; exists {
		return billing.ErrConflict
	}
	tx.rows[record.Observation.ID] = Copy(record)
	return nil
}

func (tx *memoryTx) Page(ctx context.Context, period billing.Period, after string, limit int) ([]Record, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !period.Valid() || limit < 1 || limit > 1000 {
		return nil, billing.ErrInvalid
	}
	rows := make([]Record, 0, len(tx.rows))
	for _, row := range tx.rows {
		if row.Observation.ID > after && period.Contains(row.Observation.OccurredAt) {
			rows = append(rows, Copy(row))
		}
	}
	slices.SortFunc(rows, func(a, b Record) int { return cmp.Compare(a.Observation.ID, b.Observation.ID) })
	return rows[:min(limit, len(rows))], nil
}

func (tx *memoryTx) Cost(ctx context.Context, id string) (CostRecord, error) {
	if err := ctx.Err(); err != nil {
		return CostRecord{}, err
	}
	if tx.costs == nil {
		return CostRecord{}, billing.ErrNotFound
	}
	record, ok := tx.costs[id]
	if !ok {
		return CostRecord{}, billing.ErrNotFound
	}
	return CopyCost(record), nil
}

func (tx *memoryTx) SaveCost(ctx context.Context, record CostRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if record.Account != tx.account {
		return billing.ErrConflict
	}
	if tx.costs == nil {
		tx.costs = make(map[string]CostRecord)
	}
	if _, exists := tx.costs[record.ID]; exists {
		return billing.ErrConflict
	}
	if record.CorrectsID != "" {
		for _, existing := range tx.costs {
			if existing.CorrectsID == record.CorrectsID {
				return billing.ErrConflict
			}
		}
	}
	tx.costs[record.ID] = CopyCost(record)
	return nil
}

func cloneRows(rows map[string]Record) map[string]Record {
	copyRows := make(map[string]Record, len(rows))
	for id, record := range rows {
		copyRows[id] = Copy(record)
	}
	return copyRows
}

func cloneCosts(rows map[string]CostRecord) map[string]CostRecord {
	copyRows := make(map[string]CostRecord, len(rows))
	for id, record := range rows {
		copyRows[id] = CopyCost(record)
	}
	return copyRows
}

func candidateTemporalIntersection(target, candidate Observation) bool {
	targetInterval, candidateInterval := target.Interval.Valid(), candidate.Interval.Valid()
	switch {
	case targetInterval && candidateInterval:
		return target.Interval.Start.Before(candidate.Interval.End) && candidate.Interval.Start.Before(target.Interval.End)
	case targetInterval:
		return target.Interval.Contains(candidate.OccurredAt)
	case candidateInterval:
		return candidate.Interval.Contains(target.OccurredAt)
	default:
		return false
	}
}
