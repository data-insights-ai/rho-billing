package pg

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/integration"
	"github.com/data-insights-ai/rho-billing/internal/checked"
)

const (
	inboxTable            = "billing_inbox"
	outboxTable           = "billing_outbox"
	outboundRecoveryLimit = 100
)

func queueMigrationSQL() ([]byte, error) {
	return migrationFiles.ReadFile("migrations/001_release.sql")
}

func normalizedMessage(message integration.Message) (integration.Message, string, error) {
	if err := message.Validate(); err != nil {
		return integration.Message{}, "", err
	}
	message.OccurredAt = databaseTime(message.OccurredAt)
	return message, message.Fingerprint(), nil
}

func queueTable(direction integration.Direction) (string, error) {
	switch direction {
	case integration.Inbound:
		return inboxTable, nil
	case integration.Outbound:
		return outboxTable, nil
	default:
		return "", billing.ErrInvalid
	}
}

func validateWorker(worker string) error {
	if !billing.ValidID(worker) {
		return fmt.Errorf("%w: invalid worker", billing.ErrInvalid)
	}
	return nil
}

// Moves expired outbound attempts to unknown. Unknown outcomes require
// reconciliation; they must never be selected as a fresh outbound claim.
// Ordered row locks let concurrent workers split recovery without a table-wide lock.
func recoverExpiredOutbound(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `
		WITH stale AS (
			SELECT account_id, message_id
			FROM billing_outbox
			WHERE state = 'processing' AND lease_deadline <= clock_timestamp()
			ORDER BY lease_deadline, account_id, message_id
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		)
		UPDATE billing_outbox AS outbox
		SET state = 'unknown', worker = NULL, lease_deadline = NULL,
			last_error = CASE WHEN outbox.last_error = '' THEN 'lease expired' ELSE outbox.last_error END
		FROM stale
		WHERE outbox.account_id = stale.account_id
		  AND outbox.message_id = stale.message_id
		  AND outbox.state = 'processing'
		  AND outbox.lease_deadline <= clock_timestamp()`, outboundRecoveryLimit)
	return err
}

func (s *Store) Receive(ctx context.Context, message integration.Message) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("%w: nil database", billing.ErrInvalid)
	}
	message, fingerprint, err := normalizedMessage(message)
	if err != nil {
		return err
	}
	if message.Direction != integration.Inbound {
		return fmt.Errorf("%w: receive requires inbound message", billing.ErrInvalid)
	}
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO billing_inbox (
			account_id, message_id, provider, merchant, environment, kind, direction,
			occurred_at, payload, fingerprint, available_at
		) VALUES ($1, $2, $3, $4, $5, $6, 'inbound', $7, $8, $9, $10)
		ON CONFLICT (account_id, message_id) DO NOTHING`,
		string(message.Account), message.ID, message.Scope.Provider, message.Scope.Merchant,
		message.Scope.Environment, message.Kind, message.OccurredAt, message.Payload, fingerprint, s.now(),
	)
	if err != nil {
		return err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if inserted == 1 {
		return nil
	}
	var existing string
	if err := s.db.QueryRowContext(ctx, `
		SELECT fingerprint FROM billing_inbox
		WHERE account_id = $1 AND message_id = $2`, string(message.Account), message.ID).Scan(&existing); err != nil {
		return err
	}
	if existing != fingerprint {
		return billing.ErrConflict
	}
	return nil
}

// Enqueue is deliberately unavailable outside an Atomic session.
func (s *session) Enqueue(ctx context.Context, message integration.Message) error {
	if s == nil || s.tx == nil || s.ctx == nil {
		return fmt.Errorf("%w: invalid session", billing.ErrInvalid)
	}
	if ctx == nil {
		ctx = s.ctx
	}
	message, fingerprint, err := normalizedMessage(message)
	if err != nil {
		return err
	}
	if message.Direction != integration.Outbound {
		return fmt.Errorf("%w: enqueue requires outbound message", billing.ErrInvalid)
	}
	if message.Account != s.account {
		return billing.ErrConflict
	}
	result, err := s.tx.ExecContext(ctx, `
		INSERT INTO billing_outbox (
			account_id, message_id, provider, merchant, environment, kind, direction,
			occurred_at, payload, fingerprint, available_at
		) VALUES ($1, $2, $3, $4, $5, $6, 'outbound', $7, $8, $9, $10)
			ON CONFLICT (account_id, message_id) DO NOTHING`,
		string(message.Account), message.ID, message.Scope.Provider, message.Scope.Merchant,
		message.Scope.Environment, message.Kind, message.OccurredAt, message.Payload, fingerprint, s.store.now(),
	)
	if err != nil {
		return err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if inserted == 1 {
		return nil
	}
	var existing string
	if err := s.tx.QueryRowContext(ctx, `
		SELECT fingerprint FROM billing_outbox
		WHERE account_id = $1 AND message_id = $2`, string(message.Account), message.ID).Scan(&existing); err != nil {
		return err
	}
	if existing != fingerprint {
		return billing.ErrConflict
	}
	return nil
}

// available_at is compared to now so tests can inject domain time.
// lease_deadline is assigned from clock_timestamp() so an injected past clock
// cannot expire a live worker.
func (s *Store) Claim(ctx context.Context, direction integration.Direction, worker string, now time.Time, lease time.Duration) (integration.Claim, bool, error) {
	if s == nil || s.db == nil {
		return integration.Claim{}, false, fmt.Errorf("%w: nil database", billing.ErrInvalid)
	}
	if err := validateWorker(worker); err != nil || now.IsZero() || lease <= 0 {
		if err != nil {
			return integration.Claim{}, false, err
		}
		return integration.Claim{}, false, fmt.Errorf("%w: invalid claim clock or lease", billing.ErrInvalid)
	}
	table, err := queueTable(direction)
	if err != nil {
		return integration.Claim{}, false, err
	}
	now = databaseTime(now)
	leaseSeconds := int64(lease / time.Second)
	if leaseSeconds < 1 {
		return integration.Claim{}, false, fmt.Errorf("%w: invalid claim clock or lease", billing.ErrInvalid)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return integration.Claim{}, false, err
	}
	defer tx.Rollback()
	if direction == integration.Outbound {
		if err := recoverExpiredOutbound(ctx, tx); err != nil {
			return integration.Claim{}, false, err
		}
	}
	claimCondition := "state = 'pending' AND available_at <= $1"
	if direction == integration.Inbound {
		claimCondition += " OR (state = 'processing' AND lease_deadline <= clock_timestamp())"
	}
	query := fmt.Sprintf(`
		SELECT account_id, message_id, provider, merchant, environment, kind,
		       occurred_at, payload, fingerprint, state, attempts, fence,
		       available_at, lease_deadline
		FROM %s
		WHERE (%s)
		ORDER BY available_at, received_at, account_id, message_id
		LIMIT 1 FOR UPDATE SKIP LOCKED`, table, claimCondition)
	var account, messageID, provider, merchant, environment, kind, fingerprint, state string
	var occurredAt, availableAt time.Time
	var payload []byte
	var attempts, fence int64
	var oldDeadline sql.NullTime
	err = tx.QueryRowContext(ctx, query, now).Scan(
		&account, &messageID, &provider, &merchant, &environment, &kind,
		&occurredAt, &payload, &fingerprint, &state, &attempts, &fence, &availableAt, &oldDeadline,
	)
	if errors.Is(err, sql.ErrNoRows) {
		if err := tx.Commit(); err != nil {
			return integration.Claim{}, false, err
		}
		return integration.Claim{}, false, nil
	}
	if err != nil {
		return integration.Claim{}, false, err
	}
	nextFence, err := checked.Add(fence, 1)
	if err != nil {
		return integration.Claim{}, false, err
	}
	update := fmt.Sprintf(`
		UPDATE %s
		SET state = 'processing', attempts = attempts + 1, fence = $1,
			worker = $2, lease_deadline = clock_timestamp() + make_interval(secs => $3)
		WHERE account_id = $4 AND message_id = $5
		RETURNING lease_deadline`, table)
	var deadline time.Time
	if err := tx.QueryRowContext(ctx, update, nextFence, worker, leaseSeconds, account, messageID).Scan(&deadline); err != nil {
		return integration.Claim{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return integration.Claim{}, false, err
	}
	_ = oldDeadline
	return integration.Claim{
		Message: integration.Message{
			Account: billing.AccountID(account), ID: messageID,
			Scope: billing.Scope{Provider: provider, Merchant: merchant, Environment: environment}, Kind: kind,
			Direction: direction, OccurredAt: occurredAt.UTC(), Payload: append([]byte(nil), payload...),
		},
		Fence: nextFence, Worker: worker, Deadline: deadline, Attempt: int(attempts + 1),
	}, true, nil
}

func (s *Store) ProcessInbox(ctx context.Context, claim integration.Claim, callback func(integration.Session) error) error {
	if s == nil || s.db == nil || callback == nil {
		return fmt.Errorf("%w: nil database or callback", billing.ErrInvalid)
	}
	if claim.Message.Direction != integration.Inbound || claim.Fence <= 0 || validateWorker(claim.Worker) != nil {
		return fmt.Errorf("%w: invalid inbox claim", billing.ErrInvalid)
	}
	message, fingerprint, err := normalizedMessage(claim.Message)
	if err != nil {
		return err
	}
	err = s.Atomic(ctx, message.Account, func(scope integration.Session) error {
		sess, ok := scope.(*session)
		if !ok || sess.tx == nil {
			return fmt.Errorf("%w: invalid postgres session", billing.ErrInvalid)
		}
		var storedFingerprint, state, worker string
		var fence int64
		var deadline sql.NullTime
		var active bool
		if err := sess.tx.QueryRowContext(ctx, `
			SELECT fingerprint, state, COALESCE(worker,''), fence, lease_deadline,
			       COALESCE(lease_deadline > clock_timestamp(),false)
			FROM billing_inbox
			WHERE account_id = $1 AND message_id = $2
			FOR UPDATE`, string(message.Account), message.ID).Scan(
			&storedFingerprint, &state, &worker, &fence, &deadline, &active,
		); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return billing.ErrNotFound
			}
			return err
		}
		if storedFingerprint != fingerprint || state != "processing" || worker != claim.Worker || fence != claim.Fence || !active {
			return billing.ErrConflict
		}
		if err := callback(sess); err != nil {
			return err
		}
		result, err := sess.tx.ExecContext(ctx, `
			UPDATE billing_inbox
			SET state = 'processed', worker = NULL, lease_deadline = NULL,
				processed_at = clock_timestamp(), last_error = ''
			WHERE account_id = $1 AND message_id = $2 AND state = 'processing'
			  AND worker = $3 AND fence = $4 AND lease_deadline > clock_timestamp()`,
			string(message.Account), message.ID, claim.Worker, claim.Fence)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return billing.ErrConflict
		}
		_ = deadline
		return nil
	})
	return err
}

func (s *Store) Fail(ctx context.Context, claim integration.Claim, reason string, availableAt time.Time, maxAttempts int) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("%w: nil database", billing.ErrInvalid)
	}
	if reason == "" || maxAttempts < 1 {
		return fmt.Errorf("%w: invalid delivery failure", billing.ErrInvalid)
	}
	message, fingerprint, err := normalizedMessage(claim.Message)
	if err != nil || claim.Fence <= 0 || validateWorker(claim.Worker) != nil {
		if err != nil {
			return err
		}
		return fmt.Errorf("%w: invalid delivery claim", billing.ErrInvalid)
	}
	table, err := queueTable(message.Direction)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if message.Direction == integration.Outbound {
		if _, err := tx.ExecContext(ctx, `
			UPDATE billing_outbox SET state = 'unknown', worker = NULL, lease_deadline = NULL,
				last_error = CASE WHEN last_error = '' THEN 'lease expired' ELSE last_error END
			WHERE account_id = $1 AND message_id = $2 AND fingerprint = $3
			  AND state = 'processing' AND worker = $4 AND fence = $5
			  AND lease_deadline <= clock_timestamp()`, string(message.Account), message.ID, fingerprint, claim.Worker, claim.Fence); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE billing_outbox SET state = 'unknown', worker = NULL, lease_deadline = NULL, last_error = $1
			WHERE account_id = $2 AND message_id = $3 AND fingerprint = $4
			  AND state = 'processing' AND worker = $5 AND fence = $6
			  AND lease_deadline > clock_timestamp()`, reason, string(message.Account), message.ID, fingerprint, claim.Worker, claim.Fence)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			// An expired outbound attempt must remain unknown for reconciliation.
			if err := tx.Commit(); err != nil {
				return err
			}
			return billing.ErrConflict
		}
		return tx.Commit()
	}
	if availableAt.IsZero() {
		return fmt.Errorf("%w: invalid retry time", billing.ErrInvalid)
	}
	result, err := tx.ExecContext(ctx, fmt.Sprintf(`
		UPDATE %s
		SET state = CASE WHEN attempts >= $1 THEN 'dead' ELSE 'pending' END,
			available_at = $2, worker = NULL, lease_deadline = NULL, last_error = $3
		WHERE account_id = $4 AND message_id = $5 AND fingerprint = $6
		  AND state = 'processing' AND worker = $7 AND fence = $8
		  AND lease_deadline > clock_timestamp()`, table), maxAttempts, databaseTime(availableAt), reason,
		string(message.Account), message.ID, fingerprint, claim.Worker, claim.Fence)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return billing.ErrConflict
	}
	return tx.Commit()
}

func (s *Store) RetryInbox(ctx context.Context, claim integration.Claim, availableAt time.Time) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("%w: nil database", billing.ErrInvalid)
	}
	if availableAt.IsZero() || claim.Message.Direction != integration.Inbound || claim.Fence <= 0 {
		return fmt.Errorf("%w: invalid dead-letter retry", billing.ErrInvalid)
	}
	message, fingerprint, err := normalizedMessage(claim.Message)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE billing_inbox
		SET state = 'pending', available_at = $1, worker = NULL, lease_deadline = NULL, last_error = ''
		WHERE account_id = $2 AND message_id = $3 AND fingerprint = $4
		  AND state = 'dead' AND fence = $5`, databaseTime(availableAt), string(message.Account), message.ID, fingerprint, claim.Fence)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return billing.ErrConflict
	}
	return nil
}

func (s *Store) Delivery(ctx context.Context, message integration.Message) (integration.Delivery, error) {
	if s == nil || s.db == nil {
		return integration.Delivery{}, fmt.Errorf("%w: nil database", billing.ErrInvalid)
	}
	message, _, err := normalizedMessage(message)
	if err != nil {
		return integration.Delivery{}, err
	}
	table, err := queueTable(message.Direction)
	if err != nil {
		return integration.Delivery{}, err
	}
	query := fmt.Sprintf(`
		SELECT state, fence, attempts, available_at, lease_deadline, last_error
		FROM %s WHERE account_id = $1 AND message_id = $2`, table)
	var delivery integration.Delivery
	var deadline sql.NullTime
	if err := s.db.QueryRowContext(ctx, query, string(message.Account), message.ID).Scan(
		&delivery.State, &delivery.Fence, &delivery.Attempt, &delivery.AvailableAt, &deadline, &delivery.LastError,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return integration.Delivery{}, billing.ErrNotFound
		}
		return integration.Delivery{}, err
	}
	if deadline.Valid {
		delivery.Deadline = deadline.Time.UTC()
	}
	delivery.Message = message
	return delivery, nil
}

var _ integration.Repository = (*Store)(nil)
var _ integration.Session = (*session)(nil)
