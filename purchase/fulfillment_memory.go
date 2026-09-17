package purchase

import (
	"context"
	"slices"
	"strings"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/catalog"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/usage"
)

type boundCreditRepository struct {
	account billing.AccountID
	tx      credit.Tx
}

func (r boundCreditRepository) WithinAccount(ctx context.Context, account billing.AccountID, fn func(credit.Tx) error) error {
	if fn == nil || account != r.account {
		return billing.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.tx == nil {
		return billing.ErrInvalid
	}
	return fn(r.tx)
}

func (r boundCreditRepository) History(ctx context.Context, account billing.AccountID, after int64, limit int) ([]credit.Entry, error) {
	if account != r.account {
		return nil, billing.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.tx == nil {
		return nil, billing.ErrInvalid
	}
	return r.tx.History(after, limit)
}

type boundEntitlementRepository struct {
	account billing.AccountID
	state   *memoryState
}

func (r boundEntitlementRepository) WithinAccount(ctx context.Context, account billing.AccountID, fn func(catalog.EntitlementTx) error) error {
	if fn == nil || account != r.account {
		return billing.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn(boundEntitlementTx{account: account, state: r.state})
}

type boundEntitlementTx struct {
	account billing.AccountID
	state   *memoryState
}

func (t boundEntitlementTx) Assignment(ctx context.Context, id string) (catalog.Assignment, error) {
	if err := ctx.Err(); err != nil {
		return catalog.Assignment{}, err
	}
	v, ok := t.state.assignments[id]
	if !ok {
		return catalog.Assignment{}, billing.ErrNotFound
	}
	if v.Account != t.account {
		return catalog.Assignment{}, billing.ErrConflict
	}
	return cloneEntitlementAssignment(v), nil
}

func (t boundEntitlementTx) InsertAssignment(ctx context.Context, in catalog.Assignment) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if in.Account != t.account {
		return billing.ErrConflict
	}
	if err := in.Validate(); err != nil {
		return err
	}
	plan, ok := t.state.plans[in.Plan.PlanVersionID]
	if !ok {
		return billing.ErrNotFound
	}
	if plan.ID != in.Plan.PlanVersionID || !catalog.ValidPlan(plan) {
		return billing.ErrState
	}
	if old, ok := t.state.assignments[in.Plan.ID]; ok {
		if old.Fingerprint() == in.Fingerprint() {
			return nil
		}
		return billing.ErrConflict
	}
	t.state.assignments[in.Plan.ID] = cloneEntitlementAssignment(in)
	return nil
}

func (t boundEntitlementTx) Revocation(ctx context.Context, id string) (catalog.Revocation, error) {
	if err := ctx.Err(); err != nil {
		return catalog.Revocation{}, err
	}
	v, ok := t.state.assignmentRevocations[id]
	if !ok {
		return catalog.Revocation{}, billing.ErrNotFound
	}
	if v.Account != t.account || v.AssignmentID != id {
		return catalog.Revocation{}, billing.ErrConflict
	}
	return v, nil
}

func (t boundEntitlementTx) InsertRevocation(ctx context.Context, in catalog.Revocation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if in.Account != t.account {
		return billing.ErrConflict
	}
	if err := in.Validate(); err != nil {
		return err
	}
	assignment, ok := t.state.assignments[in.AssignmentID]
	if !ok {
		return billing.ErrNotFound
	}
	if assignment.Account != t.account || assignment.Plan.ID != in.AssignmentID || in.EffectiveAt.Before(assignment.Plan.Effective.Start) {
		return billing.ErrConflict
	}
	if old, ok := t.state.assignmentRevocations[in.AssignmentID]; ok {
		if old.Fingerprint() == in.Fingerprint() {
			return nil
		}
		return billing.ErrConflict
	}
	t.state.assignmentRevocations[in.AssignmentID] = in
	return nil
}

func (t boundEntitlementTx) Plan(ctx context.Context, id string) (catalog.PlanVersion, error) {
	if err := ctx.Err(); err != nil {
		return catalog.PlanVersion{}, err
	}
	plan, ok := t.state.plans[id]
	if !ok {
		return catalog.PlanVersion{}, billing.ErrNotFound
	}
	return clonePlanVersion(plan), nil
}

func (t *memoryTx) Credits() credit.Repository {
	return boundCreditRepository{account: t.account, tx: t.creditTx}
}

func (t *memoryTx) Entitlements() catalog.EntitlementRepository {
	return boundEntitlementRepository{account: t.account, state: t.state}
}

func (t *memoryTx) Settlement(ctx context.Context, id string) (usage.BatchSummary, error) {
	if err := ctx.Err(); err != nil {
		return usage.BatchSummary{}, err
	}
	batch, ok := t.state.settlements[id]
	if !ok {
		return usage.BatchSummary{}, billing.ErrNotFound
	}
	if batch.Account != t.account {
		return usage.BatchSummary{}, billing.ErrConflict
	}
	return batch, nil
}

func (t *memoryTx) SettlementFunding(ctx context.Context, id string) (SettlementFunding, error) {
	if err := ctx.Err(); err != nil {
		return SettlementFunding{}, err
	}
	funding, ok := t.state.settlementFunding[id]
	if !ok {
		return SettlementFunding{}, billing.ErrNotFound
	}
	if funding.Account != t.account {
		return SettlementFunding{}, billing.ErrConflict
	}
	return funding, nil
}

func (t *memoryTx) InsertSettlementFunding(ctx context.Context, in SettlementFunding) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if in.Account != t.account {
		return billing.ErrConflict
	}
	if err := in.Validate(); err != nil {
		return err
	}
	if _, ok := t.lifecycle.intents[lifecycleIntentKey{account: in.Account, id: in.IntentID}]; !ok {
		return billing.ErrNotFound
	}
	if _, ok := t.state.settlements[in.BatchID]; !ok {
		return billing.ErrNotFound
	}
	if old, ok := t.state.settlementFunding[in.BatchID]; ok {
		if old.Fingerprint() == in.Fingerprint() {
			return nil
		}
		return billing.ErrConflict
	}
	for _, old := range t.state.settlementFunding {
		if old.EffectID == in.EffectID {
			return billing.ErrConflict
		}
	}

	t.state.settlementFunding[in.BatchID] = in
	return nil
}

func (t *memoryTx) Fulfillment(ctx context.Context, id string) (Fulfillment, error) {
	if err := ctx.Err(); err != nil {
		return Fulfillment{}, err
	}
	v, ok := t.state.fulfillments[id]
	if !ok {
		return Fulfillment{}, billing.ErrNotFound
	}
	if v.Account != t.account {
		return Fulfillment{}, billing.ErrConflict
	}
	return cloneFulfillment(v), nil
}

func (t *memoryTx) Fulfillments(ctx context.Context, intentID string) ([]Fulfillment, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make([]Fulfillment, 0)
	for _, value := range t.state.fulfillments {
		if value.Account == t.account && value.IntentID == intentID {
			if len(out) == maxQuoteEffects {
				return nil, billing.ErrInvalid
			}
			out = append(out, cloneFulfillment(value))
		}
	}
	slices.SortFunc(out, func(a, b Fulfillment) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

func (t *memoryTx) InsertFulfillment(ctx context.Context, in Fulfillment) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if in.Account != t.account {
		return billing.ErrConflict
	}
	if err := in.Validate(); err != nil {
		return err
	}
	if _, ok := t.lifecycle.intents[lifecycleIntentKey{account: in.Account, id: in.IntentID}]; !ok {
		return billing.ErrNotFound
	}
	intent := t.lifecycle.intents[lifecycleIntentKey{account: in.Account, id: in.IntentID}]
	quote, ok := t.state.quotes[intent.QuoteID]
	if !ok {
		return billing.ErrNotFound
	}
	var matched *QuoteLine
	for i := range quote.Lines {
		if quote.Lines[i].ID == in.LineID {
			matched = &quote.Lines[i]
			break
		}
	}
	if matched == nil {
		return billing.ErrNotFound
	}
	var effectFound bool
	for _, effect := range matched.Offer.Effects {
		if equalEffect(effect, in.Effect) {
			effectFound = true
			break
		}
	}
	if !effectFound {
		return billing.ErrConflict
	}
	if old, ok := t.state.fulfillments[in.ID]; ok {
		if old.RecordFingerprint() == in.RecordFingerprint() {
			return nil
		}
		return billing.ErrConflict
	}
	t.state.fulfillments[in.ID] = cloneFulfillment(in)
	return nil
}

func (t *memoryTx) SaveFulfillment(ctx context.Context, in Fulfillment, expected string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	old, ok := t.state.fulfillments[in.ID]
	if !ok {
		return billing.ErrNotFound
	}
	if old.Account != t.account || old.State != FulfillmentPending || (in.State != FulfillmentComplete && in.State != FulfillmentCanceled) || old.Fingerprint() != expected || old.Fingerprint() != in.Fingerprint() {
		return billing.ErrConflict
	}
	if billing.CanonicalTime(old.CreatedAt) != billing.CanonicalTime(in.CreatedAt) {
		return billing.ErrConflict
	}
	if err := in.Validate(); err != nil {
		return err
	}
	t.state.fulfillments[in.ID] = cloneFulfillment(in)
	return nil
}

func clonePlanVersion(in catalog.PlanVersion) catalog.PlanVersion {
	out := in
	out.Entitlements = slices.Clone(in.Entitlements)
	out.Allowances = slices.Clone(in.Allowances)
	return out
}

func cloneEntitlementAssignment(in catalog.Assignment) catalog.Assignment {
	out := in
	return out
}

var _ FulfillmentTx = (*memoryTx)(nil)

// AssignmentsPage lists this account's assignments in id order. The in-memory
// store keeps them in a map, so it sorts to give the same stable order the
// durable store's keyset paging produces.
func (t boundEntitlementTx) AssignmentsPage(ctx context.Context, after string, limit int) ([]catalog.Assignment, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit < 1 {
		return nil, billing.ErrInvalid
	}
	ids := make([]string, 0, len(t.state.assignments))
	for id, v := range t.state.assignments {
		if v.Account == t.account && id > after {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	out := make([]catalog.Assignment, 0, min(len(ids), limit))
	for _, id := range ids {
		if len(out) == limit {
			break
		}
		out = append(out, cloneEntitlementAssignment(t.state.assignments[id]))
	}
	return out, nil
}

// RevocationsPage lists this account's revocations in assignment-id order.
func (t boundEntitlementTx) RevocationsPage(ctx context.Context, after string, limit int) ([]catalog.Revocation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit < 1 {
		return nil, billing.ErrInvalid
	}
	ids := make([]string, 0, len(t.state.assignmentRevocations))
	for id, v := range t.state.assignmentRevocations {
		if v.Account == t.account && id > after {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	out := make([]catalog.Revocation, 0, min(len(ids), limit))
	for _, id := range ids {
		if len(out) == limit {
			break
		}
		out = append(out, t.state.assignmentRevocations[id])
	}
	return out, nil
}
