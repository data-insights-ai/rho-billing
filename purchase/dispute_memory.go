package purchase

import (
	"context"
	"slices"
	"strings"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/internal/checked"
)

type lifecycleDisputeKey struct {
	scope billing.Scope
	id    string
}
type lifecycleDisputeEventKey struct {
	scope billing.Scope
	id    string
}
type lifecycleDisputeRecoveryKey struct {
	scope billing.Scope
	id    string
}

func cloneDispute(in Dispute) Dispute {
	in.Recovered = slices.Clone(in.Recovered)
	return in
}
func cloneDisputeFact(in DisputeFact) DisputeFact {
	in.Recovery = cloneDisputeRecovery(in.Recovery)
	return in
}
func cloneDisputeRecovery(in *DisputeRecovery) *DisputeRecovery {
	if in == nil {
		return nil
	}
	out := *in
	out.Lines = slices.Clone(in.Lines)
	return &out
}
func cloneDisputeRecord(in DisputeRecord) DisputeRecord {
	in.Fact = cloneDisputeFact(in.Fact)
	return in
}
func cloneDisputeRecoveryRecord(in DisputeRecoveryRecord) DisputeRecoveryRecord {
	in.Recovery.Lines = slices.Clone(in.Recovery.Lines)
	return in
}

func (t *memoryTx) Dispute(ctx context.Context, scope billing.Scope, id string) (Dispute, error) {
	if err := ctx.Err(); err != nil {
		return Dispute{}, err
	}
	if !scope.Valid() || !billing.ValidID(id) {
		return Dispute{}, billing.ErrInvalid
	}
	v, ok := t.lifecycle.disputes[lifecycleDisputeKey{scope: scope, id: id}]
	if !ok {
		return Dispute{}, billing.ErrNotFound
	}
	if v.Account != t.account {
		return Dispute{}, billing.ErrConflict
	}
	return cloneDispute(v), nil
}

func (t *memoryTx) DisputeEvent(ctx context.Context, scope billing.Scope, id string) (DisputeRecord, error) {
	if err := ctx.Err(); err != nil {
		return DisputeRecord{}, err
	}
	if !scope.Valid() || !billing.ValidID(id) {
		return DisputeRecord{}, billing.ErrInvalid
	}
	v, ok := t.lifecycle.disputeEvents[lifecycleDisputeEventKey{scope: scope, id: id}]
	if !ok {
		return DisputeRecord{}, billing.ErrNotFound
	}
	if v.Fact.Account != t.account {
		return DisputeRecord{}, billing.ErrConflict
	}
	return cloneDisputeRecord(v), nil
}

func (t *memoryTx) InsertDisputeEvent(ctx context.Context, in DisputeRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := in.Validate(); err != nil {
		return err
	}
	if in.Fact.Account != t.account {
		return billing.ErrConflict
	}
	key := lifecycleDisputeEventKey{scope: in.Fact.Scope, id: in.Fact.EventID}
	if old, ok := t.lifecycle.disputeEvents[key]; ok {
		if old.Fingerprint() == in.Fingerprint() {
			return nil
		}
		return billing.ErrConflict
	}
	t.lifecycle.disputeEvents[key] = cloneDisputeRecord(in)
	if t.newDisputeEvents != nil {
		t.newDisputeEvents[key] = struct{}{}
	}
	return nil
}

func (t *memoryTx) SaveDispute(ctx context.Context, in Dispute, expected int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := in.Validate(); err != nil {
		return err
	}
	if in.Account != t.account || expected < 0 || in.Revision != expected+1 {
		return billing.ErrConflict
	}
	if err := t.validateDisputeReferences(in); err != nil {
		return err
	}
	key := lifecycleDisputeKey{scope: in.Scope, id: in.ID}
	old, exists := t.lifecycle.disputes[key]
	if expected == 0 {
		if exists {
			if old.Fingerprint() == in.Fingerprint() {
				return nil
			}
			return billing.ErrConflict
		}
		if err := t.disputeDebitUnique(in); err != nil {
			return err
		}
		t.lifecycle.disputes[key] = cloneDispute(in)
		return nil
	}
	if !exists || old.Account != t.account || old.Revision != expected || old.TermsFingerprint() != in.TermsFingerprint() || billing.CanonicalTime(old.CreatedAt) != billing.CanonicalTime(in.CreatedAt) {
		return billing.ErrConflict
	}
	if old.DebitAdjustmentID != "" && old.DebitAdjustmentID != in.DebitAdjustmentID {
		return billing.ErrConflict
	}
	if err := t.disputeDebitUnique(in); err != nil {
		return err
	}
	t.lifecycle.disputes[key] = cloneDispute(in)
	return nil
}

func (t *memoryTx) validateDisputeReferences(in Dispute) error {
	intent, ok := t.lifecycle.intents[lifecycleIntentKey{account: in.Account, id: in.IntentID}]
	if !ok {
		return billing.ErrNotFound
	}
	if intent.Account != in.Account || intent.ID != in.IntentID || intent.Scope != in.Scope || intent.TransactionID != in.TransactionID || intent.Currency != in.Currency {
		return billing.ErrConflict
	}
	if in.DebitAdjustmentID == "" {
		return nil
	}
	adjustment, ok := t.lifecycle.adjustments[lifecycleAdjustmentKey{account: in.Account, id: in.DebitAdjustmentID}]
	if !ok {
		return billing.ErrNotFound
	}
	if adjustment.Input.Account != in.Account || adjustment.Input.IntentID != in.IntentID || adjustment.Input.Scope != in.Scope || adjustment.Input.TransactionID != in.TransactionID || adjustment.Input.Currency != in.Currency || !adjustment.Result.Applied || adjustment.Input.Kind != AdjustmentChargeback {
		return billing.ErrConflict
	}
	return nil
}

func (t *memoryTx) disputeDebitUnique(in Dispute) error {
	if in.DebitAdjustmentID == "" {
		return nil
	}
	for key, old := range t.lifecycle.disputes {
		if key.scope == in.Scope && old.DebitAdjustmentID == in.DebitAdjustmentID && key.id != in.ID {
			return billing.ErrConflict
		}
	}
	return nil
}

func (t *memoryTx) ChargebackRecoveries(ctx context.Context, intentID string) ([]PaidLine, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := t.lifecycleReady(); err != nil {
		return nil, err
	}
	if !billing.ValidID(intentID) {
		return nil, billing.ErrInvalid
	}
	key := lifecycleIntentKey{account: t.account, id: intentID}
	if _, ok := t.lifecycle.intents[key]; !ok {
		return nil, billing.ErrNotFound
	}
	values := t.lifecycle.chargebackRecoveries[key]
	if len(values) > maxDisputeLines {
		return nil, billing.ErrOverflow
	}
	out := make([]PaidLine, 0, len(values))
	for _, value := range values {
		if !billing.ValidID(value.LineID) || value.Gross <= 0 || value.Tax < 0 || value.Tax > value.Gross {
			return nil, billing.ErrState
		}
		out = append(out, value)
	}
	slices.SortFunc(out, func(a, b PaidLine) int { return strings.Compare(a.LineID, b.LineID) })
	return out, nil
}

func (t *memoryTx) DisputeRecovery(ctx context.Context, scope billing.Scope, id string) (DisputeRecoveryRecord, error) {
	if err := ctx.Err(); err != nil {
		return DisputeRecoveryRecord{}, err
	}
	if !scope.Valid() || !billing.ValidID(id) {
		return DisputeRecoveryRecord{}, billing.ErrInvalid
	}
	v, ok := t.lifecycle.disputeRecoveries[lifecycleDisputeRecoveryKey{scope: scope, id: id}]
	if !ok {
		return DisputeRecoveryRecord{}, billing.ErrNotFound
	}
	if v.Account != t.account {
		return DisputeRecoveryRecord{}, billing.ErrConflict
	}
	return cloneDisputeRecoveryRecord(v), nil
}

func (t *memoryTx) InsertDisputeRecovery(ctx context.Context, in DisputeRecoveryRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := in.Validate(); err != nil {
		return err
	}
	if in.Account != t.account {
		return billing.ErrConflict
	}
	key := lifecycleDisputeRecoveryKey{scope: in.Scope, id: in.Recovery.ID}
	if old, ok := t.lifecycle.disputeRecoveries[key]; ok {
		if old.RecoveryFingerprint() == in.RecoveryFingerprint() {
			return nil
		}
		return billing.ErrConflict
	}
	intentKey := lifecycleIntentKey{account: in.Account, id: in.IntentID}
	if _, ok := t.lifecycle.intents[intentKey]; !ok {
		return billing.ErrNotFound
	}
	current := t.lifecycle.chargebackRecoveries[intentKey]
	next := make(map[string]PaidLine, len(current))
	for lineID, value := range current {
		next[lineID] = value
	}
	for _, line := range in.Recovery.Lines {
		old := next[line.LineID]
		if _, exists := next[line.LineID]; !exists && len(next) >= maxDisputeLines {
			return billing.ErrOverflow
		}
		gross, err := checked.Add(old.Gross, line.Gross)
		if err != nil {
			return err
		}
		tax, err := checked.Add(old.Tax, line.Tax)
		if err != nil {
			return err
		}
		next[line.LineID] = PaidLine{LineID: line.LineID, Gross: gross, Tax: tax}
	}
	t.lifecycle.chargebackRecoveries[intentKey] = next
	t.lifecycle.disputeRecoveries[key] = cloneDisputeRecoveryRecord(in)
	if t.newDisputeRecoveries != nil {
		t.newDisputeRecoveries[key] = struct{}{}
	}
	return nil
}

func (t *memoryTx) validateNewDisputeReferences() error {
	for key := range t.newDisputeEvents {
		record, ok := t.lifecycle.disputeEvents[key]
		if !ok {
			return billing.ErrConflict
		}
		caseValue, ok := t.lifecycle.disputes[lifecycleDisputeKey{scope: key.scope, id: record.Fact.DisputeID}]
		if !ok {
			return billing.ErrNotFound
		}
		if caseValue.Account != record.Fact.Account || caseValue.Account != t.account || caseValue.Scope != key.scope || caseValue.IntentID != record.Fact.IntentID || caseValue.TransactionID != record.Fact.TransactionID || caseValue.Currency != record.Fact.Currency || caseValue.Amount != record.Fact.Amount {
			return billing.ErrConflict
		}
	}
	for key := range t.newDisputeRecoveries {
		record, ok := t.lifecycle.disputeRecoveries[key]
		if !ok {
			return billing.ErrConflict
		}
		caseValue, ok := t.lifecycle.disputes[lifecycleDisputeKey{scope: key.scope, id: record.DisputeID}]
		if !ok {
			return billing.ErrNotFound
		}
		if caseValue.Account != t.account || record.Account != t.account || caseValue.Scope != key.scope || caseValue.IntentID != record.IntentID || caseValue.TransactionID != record.TransactionID || caseValue.DebitAdjustmentID != record.Recovery.AdjustmentID {
			return billing.ErrConflict
		}
	}
	return nil
}

var _ DisputeTx = (*memoryTx)(nil)
