package credit

import (
	"math"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/catalog"

	"github.com/data-insights-ai/rho-billing/internal/identity"
)

func prepareGrant(schedule Schedule, plan catalog.PlanVersion, definition catalog.AllowanceDefinition, period billing.Period, eligibility Eligibility) (GrantInput, error) {
	if !period.Valid() {
		return GrantInput{}, billing.ErrInvalid
	}
	if definition.Scope != catalog.AllowanceAccount {
		return GrantInput{}, ErrMemberScope
	}
	if err := eligibility.Covers(period); err != nil {
		return GrantInput{}, err
	}
	amount, err := scaledAmount(definition.Amount, schedule.Assignment.Quantity)
	if err != nil {
		return GrantInput{}, err
	}
	key := grantKey(schedule.Account, schedule.Assignment, plan, definition, period)
	expires := period.End
	if definition.Validity > 0 {
		if candidate := period.Start.Add(definition.Validity); candidate.Before(expires) {
			expires = candidate
		}
	}
	return GrantInput{
		Account: schedule.Account, Operation: billing.OperationID("allowance-op-" + key), LotID: "allowance-lot-" + key,
		Unit: definition.Unit, Amount: amount, Scope: string(definition.SpendScope), Source: "allowance", SourceRef: key,
		ValidFrom: period.Start, ExpiresAt: expires,
	}, nil
}

func scaledAmount(amount, quantity int64) (int64, error) {
	if amount <= 0 || quantity <= 0 || (amount > 0 && quantity > math.MaxInt64/amount) {
		if amount > 0 && quantity > math.MaxInt64/amount {
			return 0, billing.ErrOverflow
		}
		return 0, billing.ErrInvalid
	}
	return amount * quantity, nil
}

func grantKey(account billing.AccountID, assignment catalog.PlanAssignment, plan catalog.PlanVersion, definition catalog.AllowanceDefinition, period billing.Period) string {
	return identity.Fingerprint("allowance-grant", string(account), assignment.ID, assignment.PlanVersionID, plan.ID, definition.ID, identity.Instant(period.Start), identity.Instant(period.End))
}
