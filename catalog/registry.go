package catalog

import (
	"cmp"
	"context"
	"slices"
	"strconv"
	"sync"

	"github.com/data-insights-ai/rho-billing"

	"github.com/data-insights-ai/rho-billing/internal/identity"
)

// Implementations must preserve the same conflict and copy semantics as Registry.
type Repository interface {
	PublishPlan(context.Context, PlanVersion) error
	Plan(context.Context, string) (PlanVersion, error)
	PutPriceMapping(context.Context, PriceMapping) error
	PriceMapping(context.Context, MappingScope, string) (PriceMapping, error)
	PriceMappingAtRevision(context.Context, MappingScope, string, int64) (PriceMapping, error)
}

type Registry struct {
	mu       sync.RWMutex
	plans    map[string]PlanVersion
	versions map[planVersionKey]string
	mappings map[mappingKey]map[int64]PriceMapping
	units    map[string]int64
}

type planVersionKey struct {
	planID  string
	version int64
}

type mappingKey struct {
	provider    string
	account     billing.AccountID
	environment string
	merchant    string
	price       string
}

func NewRegistry() *Registry {
	return &Registry{
		plans:    make(map[string]PlanVersion),
		versions: make(map[planVersionKey]string),
		mappings: make(map[mappingKey]map[int64]PriceMapping),
	}
}

// Re-publishing identical content is idempotent; changing content under an
// existing ID is a conflict.
func (r *Registry) PublishPlan(ctx context.Context, plan PlanVersion) error {
	if err := contextErr(ctx); err != nil {
		return err
	}
	copyPlan, err := clonePlan(plan)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.plans == nil {
		r.plans = make(map[string]PlanVersion)
	}
	if r.versions == nil {
		r.versions = make(map[planVersionKey]string)
	}
	if previous, ok := r.plans[copyPlan.ID]; ok {
		if previous.fingerprint() != copyPlan.fingerprint() {
			return billing.ErrConflict
		}
		return nil
	}
	versionKey := planVersionKey{planID: copyPlan.PlanID, version: copyPlan.Version}
	if previousID, ok := r.versions[versionKey]; ok && previousID != copyPlan.ID {
		return billing.ErrConflict
	}
	// Credit/limit units retain one scale across every historical plan version.
	units := make(map[string]int64)
	for _, a := range copyPlan.Allowances {
		if scale, ok := units[a.Unit.Code]; ok && scale != a.Unit.Scale {
			return billing.ErrConflict
		}
		units[a.Unit.Code] = a.Unit.Scale
	}
	for _, e := range copyPlan.Entitlements {
		if e.Kind != EntitlementLimit {
			continue
		}
		if scale, ok := units[e.Unit.Code]; ok && scale != e.Unit.Scale {
			return billing.ErrConflict
		}
		units[e.Unit.Code] = e.Unit.Scale
	}
	for code, scale := range units {
		if prior, ok := r.units[code]; ok && prior != scale {
			return billing.ErrConflict
		}
	}
	if r.units == nil {
		r.units = make(map[string]int64)
	}
	for code, scale := range units {
		r.units[code] = scale
	}
	r.plans[copyPlan.ID] = copyPlan
	r.versions[versionKey] = copyPlan.ID
	return nil
}

func (r *Registry) Plan(ctx context.Context, id string) (PlanVersion, error) {
	if err := contextErr(ctx); err != nil {
		return PlanVersion{}, err
	}
	if !billing.ValidID(id) {
		return PlanVersion{}, billing.ErrInvalid
	}
	r.mu.RLock()
	plan, ok := r.plans[id]
	r.mu.RUnlock()
	if !ok {
		return PlanVersion{}, billing.ErrNotFound
	}
	return clonePlanValue(plan), nil
}

// Revisions must advance by one so stale writers cannot silently replace a tenant mapping.
func (r *Registry) PutPriceMapping(ctx context.Context, mapping PriceMapping) error {
	if err := contextErr(ctx); err != nil {
		return err
	}
	if err := mapping.validate(); err != nil {
		return err
	}
	key := keyFor(mapping)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.mappings == nil {
		r.mappings = make(map[mappingKey]map[int64]PriceMapping)
	}
	if mapping.Target.Kind == TargetPlanVersion || mapping.Target.Kind == TargetAddOn {
		if _, ok := r.plans[mapping.Target.ID]; !ok {
			return billing.ErrNotFound
		}
	}
	history, ok := r.mappings[key]
	if !ok {
		if mapping.Revision != 1 {
			return billing.ErrConflict
		}
		r.mappings[key] = map[int64]PriceMapping{mapping.Revision: mapping}
		return nil
	}
	if previous, exists := history[mapping.Revision]; exists && mapping.fingerprint() == previous.fingerprint() {
		return nil
	}
	latest := int64(0)
	for revision := range history {
		latest = max(latest, revision)
	}
	if mapping.Revision != latest+1 {
		return billing.ErrConflict
	}
	history[mapping.Revision] = mapping
	return nil
}

func (r *Registry) PriceMapping(ctx context.Context, scope MappingScope, externalPriceID string) (PriceMapping, error) {
	if err := contextErr(ctx); err != nil {
		return PriceMapping{}, err
	}
	if err := scope.validate(); err != nil || !billing.ValidID(externalPriceID) {
		return PriceMapping{}, billing.ErrInvalid
	}
	r.mu.RLock()
	history, ok := r.mappings[mappingKey{
		provider: scope.Provider, account: scope.Account, environment: scope.Environment,
		merchant: scope.Merchant, price: externalPriceID,
	}]
	if !ok {
		r.mu.RUnlock()
		return PriceMapping{}, billing.ErrNotFound
	}
	latest := int64(0)
	for revision := range history {
		latest = max(latest, revision)
	}
	mapping := history[latest]
	r.mu.RUnlock()
	return mapping, nil
}

func (r *Registry) PriceMappingAtRevision(ctx context.Context, scope MappingScope, externalPriceID string, revision int64) (PriceMapping, error) {
	if err := contextErr(ctx); err != nil {
		return PriceMapping{}, err
	}
	if revision <= 0 {
		return PriceMapping{}, billing.ErrInvalid
	}
	if err := scope.validate(); err != nil || !billing.ValidID(externalPriceID) {
		return PriceMapping{}, billing.ErrInvalid
	}
	r.mu.RLock()
	history, ok := r.mappings[mappingKey{provider: scope.Provider, account: scope.Account, environment: scope.Environment, merchant: scope.Merchant, price: externalPriceID}]
	mapping, found := history[revision]
	r.mu.RUnlock()
	if !ok || !found {
		return PriceMapping{}, billing.ErrNotFound
	}
	return mapping, nil
}

func contextErr(ctx context.Context) error {
	if ctx == nil {
		return billing.ErrInvalid
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func keyFor(m PriceMapping) mappingKey {
	return mappingKey{
		provider: m.Scope.Provider, account: m.Scope.Account, environment: m.Scope.Environment,
		merchant: m.Scope.Merchant, price: m.ExternalPriceID,
	}
}

func (m PriceMapping) fingerprint() string {
	return identity.Fingerprint(m.Scope.Provider, string(m.Scope.Account), m.Scope.Environment,
		m.Scope.Merchant, m.ExternalPriceID, string(rune(m.Target.Kind)), m.Target.ID, formatInt(m.Revision))
}

func (p PlanVersion) fingerprint() string {
	fields := []string{p.ID, p.PlanID, formatInt(p.Version), identity.Instant(p.PublishedAt)}
	for _, e := range p.Entitlements {
		fields = append(fields, e.Key, formatInt(int64(e.Kind)), formatInt(int64(e.Aggregation)),
			formatBool(e.Enabled), formatInt(e.Amount), e.Unit.Code, formatInt(e.Unit.Scale))
	}
	for _, a := range p.Allowances {
		fields = append(fields, a.ID, a.Unit.Code, formatInt(a.Unit.Scale), formatInt(a.Amount),
			formatInt(int64(a.Recurrence)), identity.Instant(a.Anchor), formatInt(int64(a.Validity)), formatInt(int64(a.Scope)), string(a.SpendScope))
	}
	return identity.Fingerprint(fields...)
}

// Fingerprint returns the canonical content identity used for immutable
// publication comparisons.
func (p PlanVersion) Fingerprint() string { return p.fingerprint() }

func ValidPlan(p PlanVersion) bool { return p.validate() == nil }

func clonePlan(plan PlanVersion) (PlanVersion, error) {
	if err := plan.validate(); err != nil {
		return PlanVersion{}, err
	}
	copyPlan := clonePlanValue(plan)
	slices.SortFunc(copyPlan.Entitlements, func(a, b EntitlementDefinition) int { return cmp.Compare(a.Key, b.Key) })
	slices.SortFunc(copyPlan.Allowances, func(a, b AllowanceDefinition) int { return cmp.Compare(a.ID, b.ID) })
	return copyPlan, nil
}

func clonePlanValue(plan PlanVersion) PlanVersion {
	plan.Entitlements = append([]EntitlementDefinition(nil), plan.Entitlements...)
	plan.Allowances = append([]AllowanceDefinition(nil), plan.Allowances...)
	return plan
}

func formatInt(value int64) string { return strconv.FormatInt(value, 10) }
func formatBool(value bool) string { return strconv.FormatBool(value) }
