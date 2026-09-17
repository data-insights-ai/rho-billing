package purchase

import (
	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/catalog"
	"github.com/data-insights-ai/rho-billing/usage"
)

type ReferenceAccount struct {
	Account     billing.AccountID
	Plans       []catalog.PlanVersion
	Settlements []usage.BatchSummary
}
