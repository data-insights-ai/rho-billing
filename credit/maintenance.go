package credit

import (
	"context"
	"errors"

	billing "github.com/data-insights-ai/rho-billing"
)

const reservationSweepLimit = 1000

// ErrMaintenanceRequired means due work exceeds one page. No business outcome
// is recorded; it is not a Rejection. Run Sweep until HasMore is false, then
// retry the original operation ID.
var ErrMaintenanceRequired = errors.New("credit: maintenance required")

var ErrRepairInProgress = errors.New("credit: repair in progress")

// ErrAllocationBudget is retryable work exhaustion, never an insufficient-funds
// Rejection, and does not persist an operation outcome.
var ErrAllocationBudget = errors.New("credit: allocation work budget exceeded")

type SweepResult struct {
	Reservations int
	HasMore      bool
}

func (e *Engine) Sweep(ctx context.Context, account billing.AccountID) (SweepResult, error) {
	var result SweepResult
	err := e.repo.WithinAccount(ctx, account, func(tx Tx) error {
		s, err := loadMaintenance(tx, billing.CanonicalTime(e.now()), "system:expiry")
		if err != nil {
			return err
		}
		result.Reservations = len(s.reservations)
		result.HasMore = s.maintenanceMore
		if err := s.sweep(); err != nil {
			return err
		}
		return s.flush(tx)
	})
	if err != nil {
		return SweepResult{}, err
	}
	return result, nil
}
