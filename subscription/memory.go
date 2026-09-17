package subscription

import (
	"context"
	"maps"
	"slices"
	"sync"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/catalog"
)

type memoryKey struct {
	Account billing.AccountID
	Ref     billing.Reference
}
type memoryRepository struct {
	mu         sync.Mutex
	rows       map[memoryKey]Snapshot
	refs       map[billing.Reference]billing.AccountID
	events     map[memoryEventKey]memoryEvent
	lifecycles map[memoryLifecycleKey]Lifecycle
	changes    map[memoryChangeKey]ChangeRecord
	config     MemoryConfig
}

type memoryLifecycleKey struct {
	Account billing.AccountID
	ID      string
}
type memoryChangeKey struct {
	Account   billing.AccountID
	Operation string
}

type memoryEventKey struct {
	Account billing.AccountID
	Ref     billing.Reference
	EventID string
}
type memoryEvent struct {
	Value Event
}

func NewMemoryRepository(config ...MemoryConfig) Repository {
	var options MemoryConfig
	if len(config) != 0 {
		options = config[0]
	}
	return &memoryRepository{rows: map[memoryKey]Snapshot{}, refs: map[billing.Reference]billing.AccountID{}, events: map[memoryEventKey]memoryEvent{}, lifecycles: map[memoryLifecycleKey]Lifecycle{}, changes: map[memoryChangeKey]ChangeRecord{}, config: options}
}
func (r *memoryRepository) WithinAccount(ctx context.Context, account billing.AccountID, fn func(Tx) error) error {
	if fn == nil || !billing.ValidID(string(account)) {
		return billing.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	staged := &memoryRepository{rows: make(map[memoryKey]Snapshot, len(r.rows)), refs: maps.Clone(r.refs), events: make(map[memoryEventKey]memoryEvent, len(r.events)), lifecycles: make(map[memoryLifecycleKey]Lifecycle, len(r.lifecycles)), changes: make(map[memoryChangeKey]ChangeRecord, len(r.changes)), config: r.config}
	for key, value := range r.rows {
		staged.rows[key] = Copy(value)
	}
	for key, value := range r.events {
		staged.events[key] = memoryEvent{Value: Event{Ref: value.Value.Ref, EventID: value.Value.EventID, OccurredAt: value.Value.OccurredAt, Fingerprint: value.Value.Fingerprint, Result: Copy(value.Value.Result)}}
	}
	for key, value := range r.lifecycles {
		staged.lifecycles[key] = copyLifecycle(value)
	}
	for key, value := range r.changes {
		value.Items = slices.Clone(value.Items)
		staged.changes[key] = value
	}
	if r.config.Accounts != nil {
		if _, err := r.config.Accounts.Account(ctx, account); err != nil {
			return err
		}
	}
	if err := fn(&memoryTransaction{repo: staged, account: account}); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	r.rows, r.refs, r.events, r.lifecycles, r.changes = staged.rows, staged.refs, staged.events, staged.lifecycles, staged.changes
	return nil
}

type memoryTransaction struct {
	repo    *memoryRepository
	account billing.AccountID
}

func (t *memoryTransaction) Snapshot(ctx context.Context, ref billing.Reference) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	v, ok := t.repo.rows[memoryKey{t.account, ref}]
	if !ok {
		return Snapshot{}, billing.ErrNotFound
	}
	return Copy(v), nil
}

func (t *memoryTransaction) Event(ctx context.Context, ref billing.Reference, eventID string) (Event, bool, error) {
	if err := ctx.Err(); err != nil {
		return Event{}, false, err
	}
	v, ok := t.repo.events[memoryEventKey{Account: t.account, Ref: ref, EventID: eventID}]
	if !ok {
		return Event{}, false, nil
	}
	return Event{Ref: v.Value.Ref, EventID: v.Value.EventID, OccurredAt: v.Value.OccurredAt, Fingerprint: v.Value.Fingerprint, Result: Copy(v.Value.Result)}, true, nil
}

func (t *memoryTransaction) Owner(ctx context.Context, ref billing.Reference) (billing.AccountID, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	owner, ok := t.repo.refs[ref]
	return owner, ok, nil
}

func (t *memoryTransaction) PublishedPlans(ctx context.Context, ids []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if t.repo.config.Catalog == nil {
		return nil
	}
	for _, id := range ids {
		plan, err := t.repo.config.Catalog.Plan(ctx, id)
		if err != nil {
			return err
		}
		if plan.ID != id || !catalog.ValidPlan(plan) {
			return billing.ErrConflict
		}
	}
	return nil
}

func (t *memoryTransaction) SaveSnapshot(ctx context.Context, in Snapshot) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if in.Account != t.account {
		return billing.ErrConflict
	}
	t.repo.rows[memoryKey{t.account, in.Ref}] = Copy(in)
	t.repo.refs[in.Ref] = t.account
	return nil
}

func (t *memoryTransaction) SaveEvent(ctx context.Context, in Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if in.Result.Account != t.account || in.Result.Ref != in.Ref {
		return billing.ErrConflict
	}
	t.repo.events[memoryEventKey{Account: t.account, Ref: in.Ref, EventID: in.EventID}] = memoryEvent{Value: Event{Ref: in.Ref, EventID: in.EventID, OccurredAt: in.OccurredAt, Fingerprint: in.Fingerprint, Result: Copy(in.Result)}}
	return nil
}

func (t *memoryTransaction) Lifecycle(ctx context.Context, id string) (Lifecycle, error) {
	if err := ctx.Err(); err != nil {
		return Lifecycle{}, err
	}
	v, ok := t.repo.lifecycles[memoryLifecycleKey{t.account, id}]
	if !ok {
		return Lifecycle{}, billing.ErrNotFound
	}
	return copyLifecycle(v), nil
}

func (t *memoryTransaction) SaveLifecycle(ctx context.Context, in Lifecycle, expected int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if in.Account != t.account || in.Revision != expected+1 {
		return billing.ErrConflict
	}
	if err := in.Validate(); err != nil {
		return err
	}
	key := memoryLifecycleKey{t.account, in.ID}
	old, ok := t.repo.lifecycles[key]
	if expected == 0 {
		if ok {
			return billing.ErrConflict
		}
		t.repo.lifecycles[key] = copyLifecycle(in)
		return nil
	}
	if !ok || old.Revision != expected {
		return billing.ErrConflict
	}
	t.repo.lifecycles[key] = copyLifecycle(in)
	return nil
}

func (t *memoryTransaction) Change(ctx context.Context, operation string) (ChangeRecord, bool, error) {
	if err := ctx.Err(); err != nil {
		return ChangeRecord{}, false, err
	}
	v, ok := t.repo.changes[memoryChangeKey{t.account, operation}]
	if !ok {
		return ChangeRecord{}, false, nil
	}
	v.Items = slices.Clone(v.Items)
	return v, true, nil
}

func (t *memoryTransaction) SaveChange(ctx context.Context, in ChangeRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if in.Account != t.account {
		return billing.ErrConflict
	}
	if err := in.Validate(); err != nil {
		return err
	}
	key := memoryChangeKey{t.account, in.Operation}
	old, ok := t.repo.changes[key]
	in.Items = slices.Clone(in.Items)
	if !ok {
		t.repo.changes[key] = in
		return nil
	}
	if old.Fingerprint != in.Fingerprint {
		return billing.ErrConflict
	}
	if old.State == in.State {
		return nil
	}
	if old.State == ChangePlanned && in.State == ChangeSuperseded {
		t.repo.changes[key] = in
		return nil
	}
	return billing.ErrConflict
}
