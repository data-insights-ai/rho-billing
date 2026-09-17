package purchase

import (
	"bytes"
	"context"
	"sync"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/catalog"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/usage"
)

type memoryRepository struct {
	mu          sync.Mutex
	lifecycleMu sync.Mutex
	accounts    map[billing.AccountID]*memoryAccount
	lifecycle   lifecycleMemoryState
	credits     credit.Repository
}

type memoryAccount struct {
	mu    sync.Mutex
	state memoryState
}

type memoryState struct {
	offers                map[revisionKey]Offer
	prices                map[revisionKey]Price
	quotes                map[string]Quote
	plans                 map[string]catalog.PlanVersion
	assignments           map[string]catalog.Assignment
	assignmentRevocations map[string]catalog.Revocation
	settlements           map[string]usage.BatchSummary
	fulfillments          map[string]Fulfillment
	settlementFunding     map[string]SettlementFunding
	topUpPolicies         map[string]TopUpPolicy
	topUpAttempts         map[string]TopUpAttempt
}

type revisionKey struct {
	id      string
	version int64
}

// NewMemoryRepository is process-local reference storage for tests, not a durable store.
func NewMemoryRepository(accounts ...ReferenceAccount) Repository {
	ids := make([]billing.AccountID, len(accounts))
	r := &memoryRepository{accounts: make(map[billing.AccountID]*memoryAccount, len(accounts)), lifecycle: newLifecycleMemoryState()}
	for i, fixture := range accounts {
		account := fixture.Account
		if !billing.ValidID(string(account)) {
			panic("purchase: invalid test account")
		}
		if _, exists := r.accounts[account]; exists {
			panic("purchase: duplicate test account")
		}
		ids[i] = account
		state := newMemoryState()
		for _, plan := range fixture.Plans {
			if !catalog.ValidPlan(plan) {
				panic("purchase: invalid reference plan")
			}
			if _, exists := state.plans[plan.ID]; exists {
				panic("purchase: duplicate reference plan")
			}
			state.plans[plan.ID] = clonePlanVersion(plan)
		}
		for _, batch := range fixture.Settlements {
			if batch.Account != account || !billing.ValidID(batch.ID) {
				panic("purchase: invalid reference settlement")
			}
			if _, exists := state.settlements[batch.ID]; exists {
				panic("purchase: duplicate reference settlement")
			}
			state.settlements[batch.ID] = batch
		}
		r.accounts[account] = &memoryAccount{state: state}
	}
	r.credits = credit.NewMemoryRepository(ids...)
	return r
}

func newMemoryState() memoryState {
	return memoryState{
		offers:                make(map[revisionKey]Offer),
		prices:                make(map[revisionKey]Price),
		quotes:                make(map[string]Quote),
		plans:                 make(map[string]catalog.PlanVersion),
		assignments:           make(map[string]catalog.Assignment),
		assignmentRevocations: make(map[string]catalog.Revocation),
		settlements:           make(map[string]usage.BatchSummary),
		fulfillments:          make(map[string]Fulfillment),
		settlementFunding:     make(map[string]SettlementFunding),
		topUpPolicies:         make(map[string]TopUpPolicy),
		topUpAttempts:         make(map[string]TopUpAttempt),
	}
}

func (r *memoryRepository) account(account billing.AccountID) (*memoryAccount, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.accounts[account]
	if !ok {
		return nil, billing.ErrNotFound
	}
	return a, nil
}

func (r *memoryRepository) WithinAccount(ctx context.Context, account billing.AccountID, fn func(Tx) error) error {
	if fn == nil {
		return billing.ErrInvalid
	}
	r.lifecycleMu.Lock()
	defer r.lifecycleMu.Unlock()
	a, err := r.account(account)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	copy := cloneMemoryState(a.state)
	lifecycle := cloneLifecycleMemoryState(r.lifecycle)
	err = r.credits.WithinAccount(ctx, account, func(creditTx credit.Tx) error {
		tx := &memoryTx{account: account, state: &copy, lifecycle: &lifecycle, creditTx: creditTx, newReversals: make(map[lifecycleReversalKey]struct{}), newDisputeEvents: make(map[lifecycleDisputeEventKey]struct{}), newDisputeRecoveries: make(map[lifecycleDisputeRecoveryKey]struct{})}
		if err := fn(tx); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := tx.validateNewReversalReferences(); err != nil {
			return err
		}
		if err := tx.validateNewDisputeReferences(); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	a.state = copy
	r.lifecycle = lifecycle
	return nil
}

type memoryTx struct {
	account              billing.AccountID
	state                *memoryState
	lifecycle            *lifecycleMemoryState
	creditTx             credit.Tx
	newReversals         map[lifecycleReversalKey]struct{}
	newDisputeEvents     map[lifecycleDisputeEventKey]struct{}
	newDisputeRecoveries map[lifecycleDisputeRecoveryKey]struct{}
}

func (t *memoryTx) Offer(ctx context.Context, revision Revision) (Offer, error) {
	if err := ctx.Err(); err != nil {
		return Offer{}, err
	}
	offer, ok := t.state.offers[revisionKey{id: revision.ID, version: revision.Version}]
	if !ok {
		return Offer{}, billing.ErrNotFound
	}
	if offer.Account != t.account {
		return Offer{}, billing.ErrConflict
	}
	return cloneOffer(offer), nil
}

func (t *memoryTx) InsertOffer(ctx context.Context, offer Offer) error {
	if err := offer.Validate(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if offer.Account != t.account {
		return billing.ErrConflict
	}
	key := revisionKey{id: offer.Revision.ID, version: offer.Revision.Version}
	if old, ok := t.state.offers[key]; ok {
		if equalOffer(old, offer) {
			return nil
		}
		return billing.ErrConflict
	}
	t.state.offers[key] = cloneOffer(offer)
	return nil
}

func (t *memoryTx) Price(ctx context.Context, revision Revision) (Price, error) {
	if err := ctx.Err(); err != nil {
		return Price{}, err
	}
	price, ok := t.state.prices[revisionKey{id: revision.ID, version: revision.Version}]
	if !ok {
		return Price{}, billing.ErrNotFound
	}
	if price.Account != t.account {
		return Price{}, billing.ErrConflict
	}
	return price, nil
}

func (t *memoryTx) InsertPrice(ctx context.Context, price Price) error {
	if err := price.Validate(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if price.Account != t.account {
		return billing.ErrConflict
	}
	offer, ok := t.state.offers[revisionKey{id: price.Offer.ID, version: price.Offer.Version}]
	if !ok {
		return billing.ErrNotFound
	}
	if offer.Account != t.account {
		return billing.ErrConflict
	}
	key := revisionKey{id: price.Revision.ID, version: price.Revision.Version}
	if old, ok := t.state.prices[key]; ok {
		if old == price {
			return nil
		}
		return billing.ErrConflict
	}
	t.state.prices[key] = price
	return nil
}

func (t *memoryTx) Quote(ctx context.Context, id string) (Quote, error) {
	if err := ctx.Err(); err != nil {
		return Quote{}, err
	}
	quote, ok := t.state.quotes[id]
	if !ok {
		return Quote{}, billing.ErrNotFound
	}
	if quote.Account != t.account {
		return Quote{}, billing.ErrConflict
	}
	return cloneQuote(quote), nil
}

func (t *memoryTx) InsertQuote(ctx context.Context, quote Quote) error {
	if err := quote.Validate(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if quote.Account != t.account {
		return billing.ErrConflict
	}
	if old, ok := t.state.quotes[quote.ID]; ok {
		if equalQuote(old, quote) {
			return nil
		}
		return billing.ErrConflict
	}
	t.state.quotes[quote.ID] = cloneQuote(quote)
	return nil
}

func (t *memoryTx) TopUpPolicy(ctx context.Context, id string) (TopUpPolicy, error) {
	if err := ctx.Err(); err != nil {
		return TopUpPolicy{}, err
	}
	policy, ok := t.state.topUpPolicies[id]
	if !ok {
		return TopUpPolicy{}, billing.ErrNotFound
	}
	return policy, nil
}

func (t *memoryTx) SaveTopUpPolicy(ctx context.Context, policy TopUpPolicy) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if policy.Account != t.account {
		return billing.ErrConflict
	}
	t.state.topUpPolicies[policy.ID] = policy
	return nil
}

func (t *memoryTx) TopUpAttempt(ctx context.Context, id string) (TopUpAttempt, error) {
	if err := ctx.Err(); err != nil {
		return TopUpAttempt{}, err
	}
	attempt, ok := t.state.topUpAttempts[id]
	if !ok {
		return TopUpAttempt{}, billing.ErrNotFound
	}
	return attempt, nil
}

func (t *memoryTx) ActiveTopUpAttempt(ctx context.Context, policyID string) (TopUpAttempt, error) {
	if err := ctx.Err(); err != nil {
		return TopUpAttempt{}, err
	}
	for _, attempt := range t.state.topUpAttempts {
		if attempt.PolicyID == policyID && topUpActive(attempt.State) {
			return attempt, nil
		}
	}
	return TopUpAttempt{}, billing.ErrNotFound
}

func (t *memoryTx) SaveTopUpAttempt(ctx context.Context, attempt TopUpAttempt) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if attempt.Account != t.account {
		return billing.ErrConflict
	}
	if topUpActive(attempt.State) {
		for _, old := range t.state.topUpAttempts {
			if old.PolicyID == attempt.PolicyID && old.ID != attempt.ID && topUpActive(old.State) {
				return billing.ErrConflict
			}
		}
	}
	t.state.topUpAttempts[attempt.ID] = attempt
	return nil
}

func (t *memoryTx) TopUpAttempts(ctx context.Context, policyID string) ([]TopUpAttempt, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make([]TopUpAttempt, 0)
	for _, attempt := range t.state.topUpAttempts {
		if attempt.PolicyID == policyID {
			out = append(out, attempt)
		}
	}
	return out, nil
}

func topUpActive(state TopUpAttemptState) bool {
	return state == TopUpPlanned || state == TopUpDispatched || state == TopUpUnknown || state == TopUpActionRequired
}

func cloneMemoryState(in memoryState) memoryState {
	out := newMemoryState()
	for key, offer := range in.offers {
		out.offers[key] = cloneOffer(offer)
	}
	for key, price := range in.prices {
		out.prices[key] = price
	}
	for id, quote := range in.quotes {
		out.quotes[id] = cloneQuote(quote)
	}
	for id, plan := range in.plans {
		out.plans[id] = clonePlanVersion(plan)
	}
	for id, assignment := range in.assignments {
		out.assignments[id] = cloneEntitlementAssignment(assignment)
	}
	for id, revocation := range in.assignmentRevocations {
		out.assignmentRevocations[id] = revocation
	}
	for id, batch := range in.settlements {
		out.settlements[id] = batch
	}
	for id, fulfillment := range in.fulfillments {
		out.fulfillments[id] = cloneFulfillment(fulfillment)
	}
	for id, funding := range in.settlementFunding {
		out.settlementFunding[id] = funding
	}
	for id, policy := range in.topUpPolicies {
		out.topUpPolicies[id] = policy
	}
	for id, attempt := range in.topUpAttempts {
		out.topUpAttempts[id] = attempt
	}
	return out
}

func equalOffer(a, b Offer) bool {
	if a.Account != b.Account || a.Revision != b.Revision || a.Name != b.Name || !a.PublishedAt.Equal(b.PublishedAt) || len(a.Effects) != len(b.Effects) {
		return false
	}
	for i := range a.Effects {
		if !equalEffect(a.Effects[i], b.Effects[i]) {
			return false
		}
	}
	return true
}

func equalEffect(a, b Effect) bool {
	if a.Key != b.Key || (a.Credit == nil) != (b.Credit == nil) || (a.Plan == nil) != (b.Plan == nil) || (a.Host == nil) != (b.Host == nil) || (a.Settlement == nil) != (b.Settlement == nil) {
		return false
	}
	if a.Credit != nil && *a.Credit != *b.Credit {
		return false
	}
	if a.Plan != nil && *a.Plan != *b.Plan {
		return false
	}
	if a.Host != nil && (a.Host.Kind != b.Host.Kind || !bytes.Equal(a.Host.Payload, b.Host.Payload)) {
		return false
	}
	return true
}

func equalQuote(a, b Quote) bool {
	if a.Account != b.Account || a.ID != b.ID || !a.CreatedAt.Equal(b.CreatedAt) || !a.ValidUntil.Equal(b.ValidUntil) || a.Currency != b.Currency || a.TaxTreatment != b.TaxTreatment || a.Amount != b.Amount || len(a.Lines) != len(b.Lines) {
		return false
	}
	for i := range a.Lines {
		left, right := a.Lines[i], b.Lines[i]
		if left.QuoteLineInput != right.QuoteLineInput || left.Amount != right.Amount || !equalOffer(left.Offer, right.Offer) || left.PriceSnapshot != right.PriceSnapshot {
			return false
		}
	}
	return true
}

var _ Repository = (*memoryRepository)(nil)
var _ Tx = (*memoryTx)(nil)
