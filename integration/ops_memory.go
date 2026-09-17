package integration

import (
	"context"
	"maps"
	"sync"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/catalog"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/purchase"
	"github.com/data-insights-ai/rho-billing/subscription"
	"github.com/data-insights-ai/rho-billing/usage"
)

type memoryRepository struct {
	mu       sync.Mutex
	accounts map[billing.AccountID]*memoryAccount
}

type memoryAccount struct {
	repairs    map[string]RepairRecord
	backfills  map[string]BackfillJob
	tombstones map[string]Tombstone
}

func NewMemoryOpsRepository(accounts ...billing.AccountID) OpsRepository {
	r := &memoryRepository{accounts: make(map[billing.AccountID]*memoryAccount, len(accounts))}
	for _, account := range accounts {
		if !billing.ValidID(string(account)) {
			panic("integration: invalid test account")
		}
		r.accounts[account] = &memoryAccount{repairs: map[string]RepairRecord{}, backfills: map[string]BackfillJob{}, tombstones: map[string]Tombstone{}}
	}
	return r
}

func (r *memoryRepository) WithinAccount(ctx context.Context, account billing.AccountID, fn func(OpsTx) error) error {
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
	staged := &memoryAccount{repairs: maps.Clone(row.repairs), backfills: maps.Clone(row.backfills), tombstones: maps.Clone(row.tombstones)}
	if err := fn(&memoryTx{account: account, state: staged}); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	r.accounts[account] = staged
	return nil
}

type memoryTx struct {
	account billing.AccountID
	state   *memoryAccount
}

func (tx *memoryTx) Repair(ctx context.Context, id string) (RepairRecord, error) {
	if err := ctx.Err(); err != nil {
		return RepairRecord{}, err
	}
	row, ok := tx.state.repairs[id]
	if !ok {
		return RepairRecord{}, billing.ErrNotFound
	}
	return row, nil
}

func (tx *memoryTx) SaveRepair(ctx context.Context, record RepairRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tx.state.repairs[record.ID] = record
	return nil
}

func (tx *memoryTx) Backfill(ctx context.Context, id string) (BackfillJob, error) {
	if err := ctx.Err(); err != nil {
		return BackfillJob{}, err
	}
	row, ok := tx.state.backfills[id]
	if !ok {
		return BackfillJob{}, billing.ErrNotFound
	}
	return row, nil
}

func (tx *memoryTx) SaveBackfill(ctx context.Context, job BackfillJob) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tx.state.backfills[job.ID] = job
	return nil
}

func tombstoneKey(kind, id string) string { return kind + ":" + id }

func (tx *memoryTx) Tombstone(ctx context.Context, kind, id string) (Tombstone, error) {
	if err := ctx.Err(); err != nil {
		return Tombstone{}, err
	}
	row, ok := tx.state.tombstones[tombstoneKey(kind, id)]
	if !ok {
		return Tombstone{}, billing.ErrNotFound
	}
	return row, nil
}

func (tx *memoryTx) SaveTombstone(ctx context.Context, stone Tombstone) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tx.state.tombstones[tombstoneKey(string(stone.Kind), stone.ID)] = stone
	return nil
}

func (tx *memoryTx) Tombstones(ctx context.Context, after string, limit int) ([]Tombstone, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make([]Tombstone, 0, len(tx.state.tombstones))
	for _, stone := range tx.state.tombstones {
		key := tombstoneKey(string(stone.Kind), stone.ID)
		if key > after {
			out = append(out, stone)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

type memoryInbox struct {
	mu         sync.Mutex
	pending    []Message
	processing map[string]memoryClaim
	processed  map[string]struct{}
}

type memoryClaim struct {
	claim Claim
}

func newMemoryInbox() *memoryInbox {
	return &memoryInbox{processing: map[string]memoryClaim{}, processed: map[string]struct{}{}}
}

func (q *memoryInbox) Receive(_ context.Context, message Message) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.pending = append(q.pending, message)
	return nil
}

func (q *memoryInbox) Claim(ctx context.Context, direction Direction, worker string, now time.Time, lease time.Duration) (Claim, bool, error) {
	if err := ctx.Err(); err != nil {
		return Claim{}, false, err
	}
	if direction != Inbound || lease <= 0 || now.IsZero() {
		return Claim{}, false, billing.ErrInvalid
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.pending) == 0 {
		return Claim{}, false, nil
	}
	msg := q.pending[0]
	q.pending = q.pending[1:]
	claim := Claim{Message: msg, Fence: 1, Worker: worker, Deadline: now.Add(lease), Attempt: 1}
	q.processing[msg.ID] = memoryClaim{claim: claim}
	return claim, true, nil
}

func (q *memoryInbox) ProcessInbox(ctx context.Context, claim Claim, callback func(Session) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	held := func() bool {
		q.mu.Lock()
		defer q.mu.Unlock()
		got, ok := q.processing[claim.Message.ID]
		return ok && got.claim.Fence == claim.Fence && got.claim.Worker == claim.Worker
	}
	if !held() {
		return billing.ErrConflict
	}
	if err := callback(stubSession{}); err != nil {
		return err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	// The claim is re-checked under the same lock that commits it. The callback
	// runs unlocked, so without this a claim stolen mid-handler would still be
	// committed here — and this double models the real store, where letting a
	// stale claim commit is exactly the double-processing the fence prevents.
	got, ok := q.processing[claim.Message.ID]
	if !ok || got.claim.Fence != claim.Fence || got.claim.Worker != claim.Worker {
		return billing.ErrConflict
	}
	delete(q.processing, claim.Message.ID)
	q.processed[claim.Message.ID] = struct{}{}
	return nil
}

func (q *memoryInbox) Fail(ctx context.Context, claim Claim, reason string, availableAt time.Time, maxAttempts int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if reason == "" || maxAttempts < 1 || availableAt.IsZero() {
		return billing.ErrInvalid
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.processing, claim.Message.ID)
	if claim.Attempt < maxAttempts {
		q.pending = append(q.pending, claim.Message)
	}
	return nil
}

func (q *memoryInbox) Processed(id string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	_, ok := q.processed[id]
	return ok
}

type stubSession struct{}

func (stubSession) Credits() credit.Repository                  { return nil }
func (stubSession) Purchases() purchase.Repository              { return nil }
func (stubSession) Entitlements() catalog.EntitlementRepository { return nil }
func (stubSession) Settlements() usage.SettlementRepository     { return nil }
func (stubSession) Allowances() credit.AllowanceRepository      { return nil }
func (stubSession) Usage() usage.Repository                     { return nil }
func (stubSession) Subscriptions() subscription.Repository      { return nil }
func (stubSession) Limits() credit.LimitRepository              { return nil }
func (stubSession) Enqueue(context.Context, Message) error {
	return billing.ErrInvalid
}

var _ Session = stubSession{}
var _ Inbox = (*memoryInbox)(nil)
var _ OpsRepository = (*memoryRepository)(nil)
