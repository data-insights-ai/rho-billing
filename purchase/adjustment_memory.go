package purchase

import (
	"context"
	"slices"
	"strings"

	billing "github.com/data-insights-ai/rho-billing"
)

type lifecycleAdjustmentKey struct {
	account billing.AccountID
	id      string
}

type lifecycleProviderAdjustmentKey struct {
	scope billing.Scope
	id    string
}

type lifecycleAdjustmentStateKey struct {
	account billing.AccountID
	intent  string
}

type lifecycleReversalKey struct {
	account billing.AccountID
	id      string
}

func cloneAdjustmentRecord(in AdjustmentRecord) AdjustmentRecord {
	in.Input.Lines = slices.Clone(in.Input.Lines)
	in.Result.Effects = slices.Clone(in.Result.Effects)
	return in
}

func cloneAdjustmentState(in AdjustmentState) AdjustmentState {
	in.Lines = slices.Clone(in.Lines)
	in.Effects = slices.Clone(in.Effects)
	return in
}

func cloneReversal(in Reversal) Reversal {
	in.Original = cloneFulfillment(in.Original)
	return in
}

func (t *memoryTx) validateNewReversalReferences() error {
	for key := range t.newReversals {
		reversal, ok := t.lifecycle.reversals[key]
		if !ok {
			return billing.ErrConflict
		}
		adjustment, ok := t.lifecycle.adjustments[lifecycleAdjustmentKey{account: key.account, id: reversal.AdjustmentID}]
		if !ok {
			return billing.ErrNotFound
		}
		if adjustment.Input.Account != key.account || adjustment.Input.IntentID != reversal.IntentID {
			return billing.ErrConflict
		}
	}
	return nil
}

func (t *memoryTx) Adjustment(ctx context.Context, id string) (AdjustmentRecord, error) {
	if err := ctx.Err(); err != nil {
		return AdjustmentRecord{}, err
	}
	if err := t.lifecycleReady(); err != nil {
		return AdjustmentRecord{}, err
	}
	if !billing.ValidID(id) {
		return AdjustmentRecord{}, billing.ErrInvalid
	}
	v, ok := t.lifecycle.adjustments[lifecycleAdjustmentKey{account: t.account, id: id}]
	if !ok {
		return AdjustmentRecord{}, billing.ErrNotFound
	}
	return cloneAdjustmentRecord(v), nil
}

func (t *memoryTx) ProviderAdjustment(ctx context.Context, scope billing.Scope, providerID string) (AdjustmentRecord, error) {
	if err := ctx.Err(); err != nil {
		return AdjustmentRecord{}, err
	}
	if err := t.lifecycleReady(); err != nil {
		return AdjustmentRecord{}, err
	}
	if !scope.Valid() || !billing.ValidID(providerID) {
		return AdjustmentRecord{}, billing.ErrInvalid
	}
	key := lifecycleProviderAdjustmentKey{scope: scope, id: providerID}
	local, ok := t.lifecycle.providers[key]
	if !ok {
		return AdjustmentRecord{}, billing.ErrNotFound
	}
	if local.account != t.account {
		return AdjustmentRecord{}, billing.ErrConflict
	}
	v, ok := t.lifecycle.adjustments[local]
	if !ok {
		return AdjustmentRecord{}, billing.ErrConflict
	}
	return cloneAdjustmentRecord(v), nil
}

func (t *memoryTx) InsertAdjustment(ctx context.Context, in AdjustmentRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := t.lifecycleReady(); err != nil {
		return err
	}
	if err := in.Validate(); err != nil {
		return err
	}
	if in.Input.Account != t.account {
		return billing.ErrConflict
	}
	if _, ok := t.lifecycle.intents[lifecycleIntentKey{account: t.account, id: in.Input.IntentID}]; !ok {
		return billing.ErrNotFound
	}
	local := lifecycleAdjustmentKey{account: in.Input.Account, id: in.Input.ID}
	if old, ok := t.lifecycle.adjustments[local]; ok {
		if old.Fingerprint() == in.Fingerprint() {
			return nil
		}
		return billing.ErrConflict
	}
	providerKey := lifecycleProviderAdjustmentKey{scope: in.Input.Scope, id: in.Input.ProviderAdjustmentID}
	if old, ok := t.lifecycle.providers[providerKey]; ok {
		if old.account != t.account {
			return billing.ErrConflict
		}
		stored, exists := t.lifecycle.adjustments[old]
		if exists && stored.Fingerprint() == in.Fingerprint() {
			return nil
		}
		return billing.ErrConflict
	}
	t.lifecycle.adjustments[local] = cloneAdjustmentRecord(in)
	t.lifecycle.providers[providerKey] = local
	return nil
}

func (t *memoryTx) AdjustmentState(ctx context.Context, intentID string) (AdjustmentState, error) {
	if err := ctx.Err(); err != nil {
		return AdjustmentState{}, err
	}
	if err := t.lifecycleReady(); err != nil {
		return AdjustmentState{}, err
	}
	if !billing.ValidID(intentID) {
		return AdjustmentState{}, billing.ErrInvalid
	}
	v, ok := t.lifecycle.states[lifecycleAdjustmentStateKey{account: t.account, intent: intentID}]
	if !ok {
		return AdjustmentState{}, billing.ErrNotFound
	}
	return cloneAdjustmentState(v), nil
}

func (t *memoryTx) SaveAdjustmentState(ctx context.Context, in AdjustmentState, expected int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := t.lifecycleReady(); err != nil {
		return err
	}
	if err := in.Validate(); err != nil {
		return err
	}
	if in.Account != t.account || expected < 0 || in.Revision != expected+1 {
		return billing.ErrConflict
	}
	if _, ok := t.lifecycle.intents[lifecycleIntentKey{account: t.account, id: in.IntentID}]; !ok {
		return billing.ErrNotFound
	}
	key := lifecycleAdjustmentStateKey{account: in.Account, intent: in.IntentID}
	old, exists := t.lifecycle.states[key]
	if expected == 0 {
		if exists {
			if old.Fingerprint() == in.Fingerprint() {
				return nil
			}
			return billing.ErrConflict
		}
		t.lifecycle.states[key] = cloneAdjustmentState(in)
		return nil
	}
	if !exists || old.Revision != expected {
		return billing.ErrConflict
	}
	t.lifecycle.states[key] = cloneAdjustmentState(in)
	return nil
}

func (t *memoryTx) Reversal(ctx context.Context, id string) (Reversal, error) {
	if err := ctx.Err(); err != nil {
		return Reversal{}, err
	}
	if err := t.lifecycleReady(); err != nil {
		return Reversal{}, err
	}
	if !billing.ValidID(id) {
		return Reversal{}, billing.ErrInvalid
	}
	v, ok := t.lifecycle.reversals[lifecycleReversalKey{account: t.account, id: id}]
	if !ok {
		return Reversal{}, billing.ErrNotFound
	}
	return cloneReversal(v), nil
}

func (t *memoryTx) Reversals(ctx context.Context, intentID string) ([]Reversal, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := t.lifecycleReady(); err != nil {
		return nil, err
	}
	if !billing.ValidID(intentID) {
		return nil, billing.ErrInvalid
	}
	out := make([]Reversal, 0)
	for key, value := range t.lifecycle.reversals {
		if key.account != t.account || value.IntentID != intentID {
			continue
		}
		if len(out) == 1000 {
			return nil, billing.ErrInvalid
		}
		out = append(out, cloneReversal(value))
	}
	slices.SortFunc(out, func(a, b Reversal) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

func (t *memoryTx) InsertReversal(ctx context.Context, in Reversal) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := t.lifecycleReady(); err != nil {
		return err
	}
	if err := in.Validate(); err != nil {
		return err
	}
	if in.Account != t.account {
		return billing.ErrConflict
	}
	if _, ok := t.lifecycle.intents[lifecycleIntentKey{account: t.account, id: in.IntentID}]; !ok {
		return billing.ErrNotFound
	}
	original, ok := t.state.fulfillments[in.Original.ID]
	if !ok {
		return billing.ErrNotFound
	}
	if original.Account != t.account || original.RecordFingerprint() != in.Original.RecordFingerprint() {
		return billing.ErrConflict
	}
	key := lifecycleReversalKey{account: in.Account, id: in.ID}
	if old, ok := t.lifecycle.reversals[key]; ok {
		if old.RecordFingerprint() == in.RecordFingerprint() {
			return nil
		}
		return billing.ErrConflict
	}
	t.lifecycle.reversals[key] = cloneReversal(in)
	if t.newReversals != nil {
		t.newReversals[key] = struct{}{}
	}
	return nil
}

func (t *memoryTx) SaveReversal(ctx context.Context, in Reversal, expected string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := t.lifecycleReady(); err != nil {
		return err
	}
	if err := in.Validate(); err != nil {
		return err
	}
	if in.Account != t.account || in.State != FulfillmentComplete {
		return billing.ErrConflict
	}
	key := lifecycleReversalKey{account: in.Account, id: in.ID}
	old, ok := t.lifecycle.reversals[key]
	if !ok {
		return billing.ErrNotFound
	}
	if old.State != FulfillmentPending || old.Fingerprint() != expected || old.Fingerprint() != in.Fingerprint() || !old.CreatedAt.Equal(in.CreatedAt) {
		return billing.ErrConflict
	}
	t.lifecycle.reversals[key] = cloneReversal(in)
	return nil
}

var _ AdjustmentTx = (*memoryTx)(nil)
