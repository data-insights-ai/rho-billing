package pg

import (
	"context"
	"fmt"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/purchase"
)

// ChargebackRecoveries reads bounded totals derived from immutable dispute
// recovery records. The projection is maintained in the recovery insert's
// transaction; it does not replace or rewrite the source evidence.
func (t *purchaseTx) ChargebackRecoveries(ctx context.Context, intentID string) ([]purchase.PaidLine, error) {
	if !billing.ValidID(intentID) {
		return nil, billing.ErrInvalid
	}
	intent, err := t.Intent(ctx, intentID)
	if err != nil {
		return nil, err
	}
	if intent.Account != t.session.account || intent.ID != intentID {
		return nil, billing.ErrConflict
	}
	rows, err := t.session.tx.QueryContext(ctx, `
		SELECT line_id,gross,tax
		FROM billing_purchase_chargeback_recovery_totals
		WHERE account_id=$1 AND intent_id=$2
		ORDER BY line_id
		LIMIT 101`, string(t.session.account), intentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	lines := make([]purchase.PaidLine, 0)
	for rows.Next() {
		var line purchase.PaidLine
		if err := rows.Scan(&line.LineID, &line.Gross, &line.Tax); err != nil {
			return nil, err
		}
		if !billing.ValidID(line.LineID) || line.Gross <= 0 || line.Tax < 0 || line.Tax > line.Gross {
			return nil, fmt.Errorf("purchase chargeback recovery projection: %w", billing.ErrInvalid)
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(lines) > 100 {
		return nil, fmt.Errorf("purchase chargeback recovery projection exceeds bound: %w", billing.ErrOverflow)
	}
	return lines, nil
}
