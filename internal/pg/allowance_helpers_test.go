package pg

import (
	"context"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
)

func issueDueForTest(ctx context.Context, service *credit.AllowanceService, request allowanceIssueRequest) (allowanceIssueResult, error) {
	out, err := service.AdvanceCheckpoint(ctx, credit.CheckpointRequest{
		Account: request.Account, ID: "test-worker", Now: request.Now,
		Limit: request.Limit, MaxIssuances: request.MaxIssuances,
		MaxPeriods: request.MaxPeriods, Evidence: request.Evidence,
	})
	return allowanceIssueResult{Issuances: out.Issuances, Next: out.NextSchedule, NextPeriod: out.NextPeriod, NextDefinition: out.NextDefinition, HasMore: out.HasMore}, err
}

// Legacy fixture-shaped inputs remain test-local; production hosts resume a
// named checkpoint instead of saving caller-owned cursors.
type allowanceIssueRequest struct {
	Account         billing.AccountID
	Now             time.Time
	After           string
	AfterPeriod     time.Time
	AfterDefinition string
	Limit           int
	MaxIssuances    int
	MaxPeriods      int
	Evidence        []credit.EligibilityObservation
}
type allowanceIssueResult struct {
	Issuances      []credit.Issuance
	Next           string
	NextPeriod     time.Time
	NextDefinition string
	HasMore        bool
}
