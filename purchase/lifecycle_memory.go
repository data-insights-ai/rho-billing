package purchase

import (
	"context"

	billing "github.com/data-insights-ai/rho-billing"
)

// lifecycleMemoryState is repository-scoped: provider event/funding identities
// are global to a provider scope, not a host account.
type lifecycleMemoryState struct {
	intents              map[lifecycleIntentKey]Intent
	operations           map[lifecycleOperationKey]lifecycleIntentKey
	commands             map[lifecycleCommandKey]CommandRecord
	events               map[lifecycleEventKey]PaymentRecord
	funding              map[lifecycleFundingKey]Funding
	collections          map[lifecycleCollectionKey]CollectionBinding
	adjustments          map[lifecycleAdjustmentKey]AdjustmentRecord
	providers            map[lifecycleProviderAdjustmentKey]lifecycleAdjustmentKey
	states               map[lifecycleAdjustmentStateKey]AdjustmentState
	reversals            map[lifecycleReversalKey]Reversal
	disputes             map[lifecycleDisputeKey]Dispute
	disputeEvents        map[lifecycleDisputeEventKey]DisputeRecord
	disputeRecoveries    map[lifecycleDisputeRecoveryKey]DisputeRecoveryRecord
	chargebackRecoveries map[lifecycleIntentKey]map[string]PaidLine
}

type lifecycleIntentKey struct {
	account billing.AccountID
	id      string
}

type lifecycleOperationKey struct {
	account   billing.AccountID
	operation string
}

type lifecycleCommandKey struct {
	account   billing.AccountID
	operation string
}

type lifecycleEventKey struct {
	scope   billing.Scope
	eventID string
}

type lifecycleFundingKey struct {
	scope         billing.Scope
	transactionID string
}

type lifecycleCollectionKey struct {
	scope         billing.Scope
	transactionID string
}

func newLifecycleMemoryState() lifecycleMemoryState {
	return lifecycleMemoryState{
		intents:              make(map[lifecycleIntentKey]Intent),
		operations:           make(map[lifecycleOperationKey]lifecycleIntentKey),
		commands:             make(map[lifecycleCommandKey]CommandRecord),
		events:               make(map[lifecycleEventKey]PaymentRecord),
		funding:              make(map[lifecycleFundingKey]Funding),
		collections:          make(map[lifecycleCollectionKey]CollectionBinding),
		adjustments:          make(map[lifecycleAdjustmentKey]AdjustmentRecord),
		providers:            make(map[lifecycleProviderAdjustmentKey]lifecycleAdjustmentKey),
		states:               make(map[lifecycleAdjustmentStateKey]AdjustmentState),
		reversals:            make(map[lifecycleReversalKey]Reversal),
		disputes:             make(map[lifecycleDisputeKey]Dispute),
		disputeEvents:        make(map[lifecycleDisputeEventKey]DisputeRecord),
		disputeRecoveries:    make(map[lifecycleDisputeRecoveryKey]DisputeRecoveryRecord),
		chargebackRecoveries: make(map[lifecycleIntentKey]map[string]PaidLine),
	}
}

func cloneLifecycleMemoryState(in lifecycleMemoryState) lifecycleMemoryState {
	out := newLifecycleMemoryState()
	for key, value := range in.intents {
		out.intents[key] = value
	}
	for key, value := range in.operations {
		out.operations[key] = value
	}
	for key, value := range in.commands {
		out.commands[key] = cloneCommandRecord(value)
	}
	for key, value := range in.events {
		out.events[key] = clonePaymentRecord(value)
	}
	for key, value := range in.funding {
		out.funding[key] = cloneFunding(value)
	}
	for key, value := range in.collections {
		out.collections[key] = cloneCollectionBinding(value)
	}
	for key, value := range in.adjustments {
		out.adjustments[key] = cloneAdjustmentRecord(value)
	}
	for key, value := range in.providers {
		out.providers[key] = value
	}
	for key, value := range in.states {
		out.states[key] = cloneAdjustmentState(value)
	}
	for key, value := range in.reversals {
		out.reversals[key] = cloneReversal(value)
	}
	for key, value := range in.disputes {
		out.disputes[key] = cloneDispute(value)
	}
	for key, value := range in.disputeEvents {
		out.disputeEvents[key] = cloneDisputeRecord(value)
	}
	for key, value := range in.disputeRecoveries {
		out.disputeRecoveries[key] = cloneDisputeRecoveryRecord(value)
	}
	for key, values := range in.chargebackRecoveries {
		cloned := make(map[string]PaidLine, len(values))
		for lineID, value := range values {
			cloned[lineID] = value
		}
		out.chargebackRecoveries[key] = cloned
	}
	return out
}

func (t *memoryTx) lifecycleReady() error {
	if t.lifecycle == nil {
		return billing.ErrInvalid
	}
	return nil
}

func (t *memoryTx) Intent(ctx context.Context, id string) (Intent, error) {
	if err := ctx.Err(); err != nil {
		return Intent{}, err
	}
	if err := t.lifecycleReady(); err != nil || !billing.ValidID(id) {
		if err != nil {
			return Intent{}, err
		}
		return Intent{}, billing.ErrInvalid
	}
	intent, ok := t.lifecycle.intents[lifecycleIntentKey{account: t.account, id: id}]
	if !ok {
		return Intent{}, billing.ErrNotFound
	}
	return intent, nil
}

func (t *memoryTx) IntentByOperation(ctx context.Context, operation string) (Intent, error) {
	if err := ctx.Err(); err != nil {
		return Intent{}, err
	}
	if err := t.lifecycleReady(); err != nil {
		return Intent{}, err
	}
	if !billing.ValidID(operation) {
		return Intent{}, billing.ErrInvalid
	}
	key, ok := t.lifecycle.operations[lifecycleOperationKey{account: t.account, operation: operation}]
	if !ok {
		return Intent{}, billing.ErrNotFound
	}
	return t.lifecycle.intents[key], nil
}

func (t *memoryTx) InsertIntent(ctx context.Context, intent Intent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := t.lifecycleReady(); err != nil {
		return err
	}
	if intent.Account != t.account {
		return billing.ErrConflict
	}
	if err := intent.Validate(); err != nil {
		return err
	}
	quote, ok := t.state.quotes[intent.QuoteID]
	if !ok {
		return billing.ErrNotFound
	}
	if quote.Account != intent.Account || quote.Fingerprint() != intent.QuoteFingerprint {
		return billing.ErrConflict
	}
	key := lifecycleIntentKey{account: intent.Account, id: intent.ID}
	if old, ok := t.lifecycle.intents[key]; ok {
		if old.Fingerprint() == intent.Fingerprint() {
			return nil
		}
		return billing.ErrConflict
	}
	operationKey := lifecycleOperationKey{account: intent.Account, operation: intent.Operation}
	if oldKey, ok := t.lifecycle.operations[operationKey]; ok && oldKey != key {
		return billing.ErrConflict
	}
	t.lifecycle.intents[key] = intent
	t.lifecycle.operations[operationKey] = key
	return nil
}

func (t *memoryTx) SaveIntent(ctx context.Context, intent Intent, expectedRevision int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := t.lifecycleReady(); err != nil {
		return err
	}
	if intent.Account != t.account {
		return billing.ErrConflict
	}
	key := lifecycleIntentKey{account: intent.Account, id: intent.ID}
	old, ok := t.lifecycle.intents[key]
	if !ok {
		return billing.ErrNotFound
	}
	if old.Revision != expectedRevision {
		return billing.ErrConflict
	}
	if intent.Revision != expectedRevision+1 {
		return billing.ErrConflict
	}
	if old.Fingerprint() != intent.Fingerprint() {
		return billing.ErrConflict
	}
	if billing.CanonicalTime(old.CreatedAt) != billing.CanonicalTime(intent.CreatedAt) {
		return billing.ErrConflict
	}
	if err := intent.Validate(); err != nil {
		return err
	}
	t.lifecycle.intents[key] = intent
	return nil
}

func (t *memoryTx) Command(ctx context.Context, operation string) (CommandRecord, error) {
	if err := ctx.Err(); err != nil {
		return CommandRecord{}, err
	}
	if err := t.lifecycleReady(); err != nil {
		return CommandRecord{}, err
	}
	if !billing.ValidID(operation) {
		return CommandRecord{}, billing.ErrInvalid
	}
	for key, value := range t.lifecycle.commands {
		if key.account == t.account && key.operation == operation {
			return cloneCommandRecord(value), nil
		}
	}
	return CommandRecord{}, billing.ErrNotFound
}

func (t *memoryTx) InsertCommand(ctx context.Context, record CommandRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := t.lifecycleReady(); err != nil {
		return err
	}
	in := record.Input
	if err := record.Validate(); err != nil {
		return err
	}
	if in.Account != t.account || !billing.ValidID(in.IntentID) || !billing.ValidID(in.Operation) {
		return billing.ErrConflict
	}
	if _, ok := t.lifecycle.intents[lifecycleIntentKey{account: in.Account, id: in.IntentID}]; !ok {
		return billing.ErrNotFound
	}
	key := lifecycleCommandKey{account: in.Account, operation: in.Operation}
	if old, ok := t.lifecycle.commands[key]; ok {
		if equalCommandRecord(old, record) {
			return nil
		}
		return billing.ErrConflict
	}
	t.lifecycle.commands[key] = cloneCommandRecord(record)
	return nil
}

func (t *memoryTx) PaymentEvent(ctx context.Context, scope billing.Scope, id string) (PaymentRecord, error) {
	if err := ctx.Err(); err != nil {
		return PaymentRecord{}, err
	}
	if err := t.lifecycleReady(); err != nil {
		return PaymentRecord{}, err
	}
	record, ok := t.lifecycle.events[lifecycleEventKey{scope: scope, eventID: id}]
	if !ok {
		return PaymentRecord{}, billing.ErrNotFound
	}
	if record.Fact.Account != t.account {
		return PaymentRecord{}, billing.ErrConflict
	}
	return clonePaymentRecord(record), nil
}

func (t *memoryTx) InsertPaymentEvent(ctx context.Context, record PaymentRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := t.lifecycleReady(); err != nil {
		return err
	}
	if err := record.Validate(); err != nil {
		return err
	}
	if record.Fact.Account != t.account || !record.Fact.Scope.Valid() || !billing.ValidID(record.Fact.EventID) {
		return billing.ErrConflict
	}
	if _, ok := t.lifecycle.intents[lifecycleIntentKey{account: record.Fact.Account, id: record.Fact.IntentID}]; !ok {
		return billing.ErrNotFound
	}
	key := lifecycleEventKey{scope: record.Fact.Scope, eventID: record.Fact.EventID}
	if old, ok := t.lifecycle.events[key]; ok {
		if old.Fact.Account != record.Fact.Account {
			return billing.ErrConflict
		}
		if equalPaymentRecord(old, record) {
			return nil
		}
		return billing.ErrConflict
	}
	t.lifecycle.events[key] = clonePaymentRecord(record)
	return nil
}

func (t *memoryTx) Funding(ctx context.Context, scope billing.Scope, transactionID string) (Funding, error) {
	if err := ctx.Err(); err != nil {
		return Funding{}, err
	}
	if err := t.lifecycleReady(); err != nil {
		return Funding{}, err
	}
	funding, ok := t.lifecycle.funding[lifecycleFundingKey{scope: scope, transactionID: transactionID}]
	if !ok {
		return Funding{}, billing.ErrNotFound
	}
	if funding.Account != t.account {
		return Funding{}, billing.ErrConflict
	}
	return cloneFunding(funding), nil
}

func (t *memoryTx) InsertFunding(ctx context.Context, funding Funding) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := t.lifecycleReady(); err != nil {
		return err
	}
	if err := funding.Validate(); err != nil {
		return err
	}
	if funding.Account != t.account || !funding.Scope.Valid() || !billing.ValidID(funding.TransactionID) || !billing.ValidID(funding.IntentID) {
		return billing.ErrConflict
	}
	if _, ok := t.lifecycle.intents[lifecycleIntentKey{account: funding.Account, id: funding.IntentID}]; !ok {
		return billing.ErrNotFound
	}
	key := lifecycleFundingKey{scope: funding.Scope, transactionID: funding.TransactionID}
	if old, ok := t.lifecycle.funding[key]; ok {
		if old.Account != funding.Account {
			return billing.ErrConflict
		}
		if old.Fingerprint() == funding.Fingerprint() {
			return nil
		}
		return billing.ErrConflict
	}
	for existingKey, existing := range t.lifecycle.funding {
		if existingKey.scope == funding.Scope && existing.Account == funding.Account && existing.IntentID == funding.IntentID && existingKey.transactionID != funding.TransactionID {
			return billing.ErrConflict
		}
	}
	t.lifecycle.funding[key] = cloneFunding(funding)
	return nil
}

func cloneCommandRecord(in CommandRecord) CommandRecord { return in }

func equalCommandRecord(a, b CommandRecord) bool {
	return a.Fingerprint() == b.Fingerprint()
}

func equalPaymentRecord(a, b PaymentRecord) bool {
	return a.Fact.Fingerprint() == b.Fact.Fingerprint() && a.Result == b.Result
}

var _ LifecycleTx = (*memoryTx)(nil)
