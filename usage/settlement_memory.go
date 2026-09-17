package usage

import (
	"context"
	"slices"
	"sync"

	billing "github.com/data-insights-ai/rho-billing"
)

type settlementMemoryRepository struct {
	mu       sync.Mutex
	accounts map[billing.AccountID]*settlementMemoryAccount
}

type settlementMemoryAccount struct {
	mu    sync.Mutex
	state settlementMemoryState
}

type settlementMemoryState struct {
	batches   map[string]Batch
	attempts  map[string]Attempt
	outcomes  map[billing.OperationID]Outcome
	claims    map[string]string
	pending   map[string]string
	sequences map[string]int64
	watermark int64
	closes    map[string]CloseJob
	nonlinear map[string]map[string]struct{}
	usage     []Record
}

type ReferenceAccount struct {
	ID    billing.AccountID
	Usage []Record
}

func NewMemorySettlementRepository(accounts ...ReferenceAccount) SettlementRepository {
	r := &settlementMemoryRepository{accounts: make(map[billing.AccountID]*settlementMemoryAccount)}
	for _, fixture := range accounts {
		account := fixture.ID
		if !billing.ValidID(string(account)) {
			panic("usage: invalid test account")
		}
		if _, exists := r.accounts[account]; exists {
			panic("usage: duplicate test account")
		}
		state := newSettlementMemoryState()
		ids := make(map[string]bool)
		for _, record := range fixture.Usage {
			if record.Observation.Account != account || ids[record.Observation.ID] || ValidateStoredRecord(record) != nil || record.Fingerprint != Identity(record) {
				panic("usage: invalid test usage")
			}
			ids[record.Observation.ID] = true
			state.usage = append(state.usage, Copy(record))
			state.watermark++
			state.sequences[record.Observation.ID] = state.watermark
		}
		r.accounts[account] = &settlementMemoryAccount{state: state}
	}
	return r
}

func newSettlementMemoryState() settlementMemoryState {
	return settlementMemoryState{batches: map[string]Batch{}, attempts: map[string]Attempt{}, outcomes: map[billing.OperationID]Outcome{}, claims: map[string]string{}, pending: map[string]string{}, sequences: map[string]int64{}, closes: map[string]CloseJob{}, nonlinear: map[string]map[string]struct{}{}}
}

func cloneSettlementState(state settlementMemoryState) settlementMemoryState {
	out := newSettlementMemoryState()
	for _, record := range state.usage {
		out.usage = append(out.usage, Copy(record))
	}
	for id, batch := range state.batches {
		out.batches[id] = cloneBatch(batch)
	}
	for id, attempt := range state.attempts {
		out.attempts[id] = attempt
	}
	for op, outcome := range state.outcomes {
		out.outcomes[op] = Outcome{Fingerprint: outcome.Fingerprint, Error: outcome.Error, Batch: outcome.Batch, Attempt: outcome.Attempt}
	}
	for usageID, batchID := range state.claims {
		out.claims[usageID] = batchID
	}
	for usageID, batchID := range state.pending {
		out.pending[usageID] = batchID
	}
	for usageID, sequence := range state.sequences {
		out.sequences[usageID] = sequence
	}
	out.watermark = state.watermark
	for id, job := range state.closes {
		out.closes[id] = job
	}
	for jobID, keys := range state.nonlinear {
		out.nonlinear[jobID] = map[string]struct{}{}
		for key := range keys {
			out.nonlinear[jobID][key] = struct{}{}
		}
	}
	return out
}

func (r *settlementMemoryRepository) account(id billing.AccountID) (*settlementMemoryAccount, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	account, ok := r.accounts[id]
	if !ok {
		return nil, billing.ErrNotFound
	}
	return account, nil
}

func (r *settlementMemoryRepository) WithinAccount(ctx context.Context, id billing.AccountID, fn func(SettlementTx) error) error {
	account, err := r.account(id)
	if err != nil {
		return err
	}
	account.mu.Lock()
	defer account.mu.Unlock()
	if err = ctx.Err(); err != nil {
		return err
	}
	state := cloneSettlementState(account.state)
	if err = fn(&settlementMemoryTx{state: &state}); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	account.state = state
	return nil
}

func (r *settlementMemoryRepository) Attempt(ctx context.Context, account billing.AccountID, id string) (Attempt, error) {
	var out Attempt
	err := r.WithinAccount(ctx, account, func(tx SettlementTx) error {
		attempt, ok, err := tx.Attempt(id)
		if !ok && err == nil {
			return billing.ErrNotFound
		}
		out = attempt
		return err
	})
	return out, err
}

type settlementMemoryTx struct{ state *settlementMemoryState }

func (tx *settlementMemoryTx) BatchHeader(id string) (BatchRecord, bool, error) {
	b, ok := tx.state.batches[id]
	if !ok {
		return BatchRecord{}, false, nil
	}
	return BatchRecord{Summary: batchSummary(b), Fingerprint: b.Fingerprint}, true, nil
}
func (tx *settlementMemoryTx) UsageByIDs(ids []string) ([]Record, error) {
	if len(ids) > 1000 {
		return nil, billing.ErrInvalid
	}
	wanted := map[string]struct{}{}
	for _, id := range ids {
		wanted[id] = struct{}{}
	}
	out := make([]Record, 0, len(ids))
	for _, r := range tx.state.usage {
		if _, ok := wanted[r.Observation.ID]; ok {
			out = append(out, Copy(r))
		}
	}
	if len(out) != len(wanted) {
		return nil, billing.ErrNotFound
	}
	return out, nil
}
func (tx *settlementMemoryTx) OriginalUsageIDs(batchID string, ids []string) ([]string, error) {
	b, ok := tx.state.batches[batchID]
	if !ok {
		return nil, billing.ErrNotFound
	}
	wanted := map[string]struct{}{}
	for _, id := range ids {
		wanted[id] = struct{}{}
	}
	var out []string
	for _, l := range b.Lines {
		if _, ok := wanted[l.UsageID]; ok {
			out = append(out, l.UsageID)
		}
	}
	return out, nil
}
func (tx *settlementMemoryTx) UsageClaims(req []UsageClaimRequest) (map[string]string, error) {
	out := map[string]string{}
	for _, r := range req {
		owner := ""
		if b, ok := tx.state.claims[r.UsageID]; ok {
			owner = b
		}
		if b, ok := tx.state.pending[r.UsageID]; ok {
			if owner != "" && owner != b {
				return nil, billing.ErrConflict
			}
			owner = b
		}
		if owner != "" {
			out[r.UsageID] = owner
		}
	}
	return out, nil
}
func (tx *settlementMemoryTx) ReserveUsageClaims(batchID string, req []UsageClaimRequest) error {
	for _, r := range req {
		if old, ok := tx.state.claims[r.UsageID]; ok && old != batchID {
			return billing.ErrConflict
		}
		if old, ok := tx.state.pending[r.UsageID]; ok && old != batchID {
			return billing.ErrConflict
		}
		tx.state.pending[r.UsageID] = batchID
	}
	return nil
}
func (tx *settlementMemoryTx) InsertCorrectionBatch(record BatchRecord, lines []ChargeLine) error {
	b := batchFromSummary(record.Summary)
	b.Fingerprint = record.Fingerprint
	b.Lines = slices.Clone(lines)
	tx.state.batches[b.ID] = b
	for _, l := range lines {
		if l.Kind == "usage" {
			tx.state.claims[l.UsageID] = b.ID
			delete(tx.state.pending, l.UsageID)
		}
	}
	return nil
}
func (tx *settlementMemoryTx) UpdateBatchState(summary BatchSummary, expected int64) error {
	b, ok := tx.state.batches[summary.ID]
	if !ok {
		return billing.ErrNotFound
	}
	if b.Revision != expected {
		return billing.ErrConflict
	}
	b.State = summary.State
	b.Revision = summary.Revision
	b.UpdatedAt = summary.UpdatedAt
	tx.state.batches[b.ID] = b
	return nil
}

func (tx *settlementMemoryTx) Attempt(id string) (Attempt, bool, error) {
	attempt, ok := tx.state.attempts[id]
	return attempt, ok, nil
}

func (tx *settlementMemoryTx) PutAttempt(attempt Attempt) error {
	tx.state.attempts[attempt.ID] = attempt
	return nil
}

func (tx *settlementMemoryTx) FindAttempt(provider, key string) (Attempt, bool, error) {
	for _, attempt := range tx.state.attempts {
		if attempt.Provider == provider && attempt.IdempotencyKey == key {
			return attempt, true, nil
		}
	}
	return Attempt{}, false, nil
}

func (tx *settlementMemoryTx) Outcome(op billing.OperationID) (Outcome, bool, error) {
	outcome, ok := tx.state.outcomes[op]
	if !ok {
		return Outcome{}, false, nil
	}
	return Outcome{Fingerprint: outcome.Fingerprint, Error: outcome.Error, Batch: outcome.Batch, Attempt: outcome.Attempt}, true, nil
}

func (tx *settlementMemoryTx) PutOutcome(op billing.OperationID, outcome Outcome) error {
	if _, exists := tx.state.outcomes[op]; exists {
		return billing.ErrConflict
	}
	tx.state.outcomes[op] = Outcome{Fingerprint: outcome.Fingerprint, Error: outcome.Error, Batch: outcome.Batch, Attempt: outcome.Attempt}
	return nil
}
