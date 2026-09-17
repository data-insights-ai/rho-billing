package host_test

import (
	"context"
	"errors"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/integration"
	"github.com/data-insights-ai/rho-billing/postgres"
)

// A host can compose a schedule change with its other billing effects without
// importing storage internals or opening a nested transaction.
func putScheduleInSession(ctx context.Context, session integration.Session, schedule credit.Schedule, revision int64, now func() time.Time) (credit.Schedule, error) {
	return credit.NewAllowances(session.Allowances(), now).PutSchedule(ctx, schedule, revision)
}

func TestAllowanceConstructionAndValidationAreLocal(t *testing.T) {
	service := credit.NewAllowances(postgres.New(nil).Allowances(), nil)
	if _, err := service.PutSchedule(t.Context(), credit.Schedule{}, 0); !errors.Is(err, billing.ErrInvalid) {
		t.Fatalf("invalid schedule: %v", err)
	}
	if err := service.RecordEligibility(t.Context(), credit.EligibilityObservation{}); !errors.Is(err, billing.ErrInvalid) {
		t.Fatalf("invalid eligibility: %v", err)
	}
	if _, err := service.AdvanceCheckpoint(t.Context(), credit.CheckpointRequest{}); !errors.Is(err, billing.ErrInvalid) {
		t.Fatalf("invalid issuance request: %v", err)
	}
}
