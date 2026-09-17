package pg

import (
	"context"
	"fmt"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/integration"
)

// PruneDeliveryPayloads removes raw payload bytes only from terminal deliveries
// completed before before. IDs, fingerprints, outcomes, references, and domain
// evidence remain durable, so a replay cannot recreate the financial effect.
// The host decides retention/export policy; pending, dead-letter, and unknown
// deliveries are never pruned. Limit bounds rows per call (1..1000).
func (s *Store) PruneDeliveryPayloads(ctx context.Context, account billing.AccountID, direction integration.Direction, before time.Time, limit int) (int64, error) {
	if s == nil || s.db == nil || !billing.ValidID(string(account)) || before.IsZero() || limit < 1 || limit > 1000 {
		return 0, billing.ErrInvalid
	}
	table, err := queueTable(direction)
	if err != nil {
		return 0, err
	}
	terminal, completed := "state='processed'", "processed_at"
	if direction == integration.Outbound {
		terminal = "state IN ('completed','rejected')"
		completed = "completed_at"
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if err := lockSettlementAccount(ctx, tx, account); err != nil {
		return 0, err
	}
	// SQL identifiers and predicates above come only from fixed library constants.
	query := fmt.Sprintf(`WITH picked AS (SELECT account_id,message_id FROM %s
 WHERE account_id=$1 AND %s AND %s<$2 AND payload_pruned_at IS NULL
 ORDER BY %s,message_id LIMIT $3 FOR UPDATE)
 UPDATE %s d SET payload=''::bytea,payload_pruned_at=clock_timestamp()
 FROM picked p WHERE d.account_id=p.account_id AND d.message_id=p.message_id`, table, terminal, completed, completed, table)
	result, err := tx.ExecContext(ctx, query, string(account), billing.CanonicalTime(before), limit)
	if err != nil {
		return 0, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return count, nil
}
