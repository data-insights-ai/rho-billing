package pg

import (
	"context"
	"database/sql"
	"errors"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
)

func (s *session) allowanceSuccessorEffectiveAt(ctx context.Context, scheduleID string) (time.Time, error) {
	var at time.Time
	err := s.tx.QueryRowContext(ctx, `SELECT effective_at FROM billing_allowance_successors
 WHERE account_id=$1 AND schedule_id=$2`, string(s.account), scheduleID).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	return billing.CanonicalTime(at), err
}
