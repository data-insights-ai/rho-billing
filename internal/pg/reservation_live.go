package pg

import (
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
)

func (t *transaction) DueReservations(at time.Time, limit int) ([]credit.Reservation, error) {
	if at.IsZero() || limit < 1 || limit > 1001 {
		return nil, billing.ErrInvalid
	}
	return t.reservations(`
 SELECT reservation_id,actor,unit,scope,created_at,deadline,state,authorized,
 consumed,limit_period_start,limit_period_end,evidence
 FROM billing_reservations WHERE account_id=$1 AND state='held' AND deadline<=$2
 ORDER BY deadline,reservation_id LIMIT $3`, t.accountID(), databaseTime(at), limit)
}

func (t *transaction) CommittedForLimit(actor, unit string, period billing.Period, at time.Time) (int64, error) {
	var value string
	err := t.tx.QueryRowContext(t.ctx, `
 SELECT COALESCE(SUM(amount),0) FROM (
 SELECT consumed::numeric AS amount FROM billing_reservations
 WHERE account_id=$1 AND actor=$2 AND unit=$3 AND state='settled'
 AND created_at >= $4 AND created_at < $5
 UNION ALL
 SELECT authorized::numeric FROM billing_reservations
 WHERE account_id=$1 AND actor=$2 AND unit=$3 AND state='held' AND deadline>$6
 AND created_at >= $4 AND created_at < $5
 ) exposure`, t.accountID(), actor, unit, databaseTime(period.Start), databaseTime(period.End), databaseTime(at)).Scan(&value)
	if err != nil {
		return 0, err
	}
	return projectionNumericInt64(value)
}
