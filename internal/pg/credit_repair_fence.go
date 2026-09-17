package pg

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/data-insights-ai/rho-billing/credit"
)

const creditRepairSQLState = "RB001"

func mapCreditRepairError(err error) error {
	if err == nil {
		return nil
	}
	pgerr, ok := errors.AsType[*pgconn.PgError](err)
	if ok && pgerr.Code == creditRepairSQLState {
		return credit.ErrRepairInProgress
	}
	return err
}
