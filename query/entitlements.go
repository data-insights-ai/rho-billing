package query

import (
	"context"
	"errors"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/catalog"
	"github.com/data-insights-ai/rho-billing/subscription"
)

// Entitlements is the answer to the question a host asks on nearly every
// request: what may this account do right now?
type Entitlements struct {
	Account billing.AccountID
	AsOf    time.Time
	// Subscribed reports whether a subscription lifecycle exists at all. An
	// account that never subscribed is not an error and not a failure to look
	// up — it simply holds nothing, and a host must be able to tell that apart
	// from "the database was unreachable", because the two call for opposite
	// behaviour.
	Subscribed bool
	// Active reports whether access is currently granted. A lapsed or paused
	// subscription is still Subscribed, but not Active.
	Active bool
	// PlanVersions are the catalog plan versions assigned to this account.
	PlanVersions []string
	// Resolved is the aggregated entitlement set: feature flags OR-ed, limits
	// MAX-ed or summed per their declared aggregation.
	Resolved catalog.EntitlementSnapshot
}

// Feature reports whether an enabled feature entitlement is present.
func (e Entitlements) Feature(key string) bool {
	ent, ok := e.Resolved.Get(key)
	return ok && ent.Kind == catalog.EntitlementFeature && ent.Enabled
}

// Limit reports a limit entitlement's aggregated amount and its unit.
//
// Presence is the grant. A limit definition carries Enabled false by
// construction — only features use that flag — so testing it here would report
// "no limit" for every correctly declared limit.
func (e Entitlements) Limit(key string) (int64, billing.Unit, bool) {
	ent, ok := e.Resolved.Get(key)
	if !ok || ent.Kind != catalog.EntitlementLimit {
		return 0, billing.Unit{}, false
	}
	return ent.Amount, ent.Unit, true
}

// Entitlements resolves what an account is entitled to right now.
//
// The answer comes from the catalog assignments purchase fulfilment writes —
// the durable record of what was actually granted — minus anything revoked.
// That is the only place a grant is recorded, so it is the only honest place to
// read one from.
//
// subscriptionID is the host's own identifier for the subscription lifecycle,
// whatever it passed to Activate, typically its tenant or organization id. It
// is optional: pass "" for an account whose entitlements do not come from a
// subscription (a one-off purchase, a manual grant), and Subscribed stays
// false. No provider reference is needed either way — this reads the
// application system of record and never contacts a payment provider.
func (s *Service) Entitlements(ctx context.Context, account billing.AccountID, subscriptionID string) (Entitlements, error) {
	if err := ctx.Err(); err != nil {
		return Entitlements{}, err
	}
	if s == nil || s.deps.Entitlements == nil || s.deps.Catalog == nil {
		return Entitlements{}, billing.ErrInvalid
	}
	if !billing.ValidID(string(account)) {
		return Entitlements{}, billing.ErrInvalid
	}
	if subscriptionID != "" && !billing.ValidID(subscriptionID) {
		return Entitlements{}, billing.ErrInvalid
	}
	at := billing.CanonicalTime(s.now())
	out := Entitlements{Account: account, AsOf: at, Resolved: catalog.EntitlementSnapshot{At: at}}

	if subscriptionID != "" {
		if s.deps.Subscriptions == nil {
			return Entitlements{}, billing.ErrInvalid
		}
		life, err := s.deps.Subscriptions.Lifecycle(ctx, account, subscriptionID)
		switch {
		case err == nil:
			out.Subscribed = true
			out.Active = life.Access == subscription.AccessActive || life.Access == subscription.AccessInGrace
		case errors.Is(err, billing.ErrNotFound):
			// Not subscribed is not an error. A host must be able to tell it
			// apart from "the database was unreachable", because the two call
			// for opposite behaviour.
		default:
			return Entitlements{}, err
		}
	}

	holdings, err := s.deps.Entitlements.Holdings(ctx, account)
	if err != nil {
		return Entitlements{}, err
	}

	versions := make([]catalog.PlanVersion, 0, len(holdings.Assignments))
	seen := make(map[string]struct{}, len(holdings.Assignments))
	for _, assignment := range holdings.Assignments {
		out.PlanVersions = append(out.PlanVersions, assignment.PlanVersionID)
		if _, ok := seen[assignment.PlanVersionID]; ok {
			continue
		}
		seen[assignment.PlanVersionID] = struct{}{}
		version, err := s.deps.Catalog.Plan(ctx, assignment.PlanVersionID)
		if err != nil {
			// An assignment naming a plan the catalog does not have is a real
			// inconsistency, not an empty entitlement set: answering "you get
			// nothing" would silently downgrade a paying customer.
			return Entitlements{}, err
		}
		versions = append(versions, version)
	}

	resolved, err := catalog.ResolveEntitlements(catalog.EntitlementInput{
		At: at, Assignments: holdings.Assignments, Versions: versions, Revocations: holdings.Revocations,
	})
	if err != nil {
		return Entitlements{}, err
	}
	out.Resolved = resolved
	return out, nil
}
