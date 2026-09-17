package pg

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/integration"
	"github.com/data-insights-ai/rho-billing/internal/identity"
)

const (
	outboxFinish  = "finish"
	outboxResolve = "resolve"
)

var errOutboxLeaseExpired = errors.New("postgres: outbox lease expired")

type lockedOutbox struct {
	fingerprint, state, worker string
	fence                      int64
	deadline, begun            sql.NullTime
	providerReference          sql.NullString
	lastObservation            string
	active                     bool
}

func validateOutboxClaim(claim integration.Claim, requireWorker bool) (integration.Message, string, error) {
	message, fingerprint, err := normalizedMessage(claim.Message)
	if err != nil {
		return integration.Message{}, "", err
	}
	if message.Direction != integration.Outbound || claim.Fence <= 0 || requireWorker && validateWorker(claim.Worker) != nil {
		return integration.Message{}, "", fmt.Errorf("%w: invalid outbox claim", billing.ErrInvalid)
	}
	return message, fingerprint, nil
}

func lockOutbox(ctx context.Context, tx *sql.Tx, message integration.Message) (lockedOutbox, error) {
	var row lockedOutbox
	err := tx.QueryRowContext(ctx, `
		SELECT fingerprint,state,COALESCE(worker,''),fence,lease_deadline,begun_at,provider_reference,
		       COALESCE(last_observation_id,''),
		       COALESCE(lease_deadline > clock_timestamp(),false)
		FROM billing_outbox
		WHERE account_id=$1 AND message_id=$2
		FOR UPDATE`, string(message.Account), message.ID).Scan(
		&row.fingerprint, &row.state, &row.worker, &row.fence, &row.deadline, &row.begun, &row.providerReference, &row.lastObservation, &row.active,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return lockedOutbox{}, billing.ErrNotFound
	}
	return row, err
}

func activeOutboxClaim(row lockedOutbox, claim integration.Claim, fingerprint string) bool {
	return row.fingerprint == fingerprint && row.state == "processing" && row.worker == claim.Worker && row.fence == claim.Fence && row.active
}

func compatibleOutboxReference(row lockedOutbox, result integration.OutboxResult) bool {
	return !row.providerReference.Valid || result.ProviderReference == "" || row.providerReference.String == result.ProviderReference
}

func (s *Store) BeginOutbox(ctx context.Context, claim integration.Claim, callback func(integration.Session) error) (bool, error) {
	if s == nil || s.db == nil || callback == nil {
		return false, fmt.Errorf("%w: nil database or callback", billing.ErrInvalid)
	}
	message, fingerprint, err := validateOutboxClaim(claim, true)
	if err != nil {
		return false, err
	}
	granted := false
	err = s.Atomic(ctx, message.Account, func(scope integration.Session) error {
		sess, ok := scope.(*session)
		if !ok || sess.tx == nil {
			return fmt.Errorf("%w: invalid postgres session", billing.ErrInvalid)
		}
		row, err := lockOutbox(ctx, sess.tx, message)
		if err != nil {
			return err
		}
		if row.fingerprint != fingerprint || row.fence != claim.Fence {
			return billing.ErrConflict
		}
		if row.begun.Valid {
			if activeOutboxClaim(row, claim, fingerprint) {
				return nil
			}
			return billing.ErrConflict
		}
		if !activeOutboxClaim(row, claim, fingerprint) {
			return billing.ErrConflict
		}
		result, err := sess.tx.ExecContext(ctx, `
			UPDATE billing_outbox SET begun_at=clock_timestamp()
			WHERE account_id=$1 AND message_id=$2 AND fingerprint=$3
			  AND state='processing' AND worker=$4 AND fence=$5
			  AND begun_at IS NULL AND lease_deadline > clock_timestamp()`,
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
		if err := callback(sess); err != nil {
			return err
		}
		var active bool
		if err := sess.tx.QueryRowContext(ctx, `
			SELECT lease_deadline > clock_timestamp()
			FROM billing_outbox
			WHERE account_id=$1 AND message_id=$2 AND fingerprint=$3
			  AND state='processing' AND worker=$4 AND fence=$5 AND begun_at IS NOT NULL`,
			string(message.Account), message.ID, fingerprint, claim.Worker, claim.Fence).Scan(&active); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return billing.ErrConflict
			}
			return err
		}
		if !active {
			return billing.ErrConflict
		}
		granted = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return granted, nil
}

func outboxResultFingerprint(mode, messageFingerprint string, fence int64, result integration.OutboxResult) string {
	return identity.Fingerprint(mode, messageFingerprint, fmt.Sprint(fence), result.Fingerprint())
}

func readOutboxResult(ctx context.Context, tx *sql.Tx, message integration.Message, observationID string) (string, bool, error) {
	var fingerprint string
	err := tx.QueryRowContext(ctx, `
		SELECT result_fingerprint FROM billing_outbox_results
		WHERE account_id=$1 AND message_id=$2 AND observation_id=$3`,
		string(message.Account), message.ID, observationID).Scan(&fingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return fingerprint, err == nil, err
}

func insertOutboxResult(ctx context.Context, tx *sql.Tx, message integration.Message, messageFingerprint, mode string, fence int64, result integration.OutboxResult) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO billing_outbox_results(
			account_id,message_id,observation_id,message_fingerprint,claim_fence,mode,state,
			provider_reference,evidence,expected_previous,result_fingerprint
		) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		string(message.Account), message.ID, result.ObservationID, messageFingerprint, fence, mode,
		string(result.State), nullString(result.ProviderReference), result.Evidence, result.ExpectedPrevious,
		outboxResultFingerprint(mode, messageFingerprint, fence, result))
	return err
}

func updateOutboxResult(ctx context.Context, tx *sql.Tx, message integration.Message, fingerprint string, claim integration.Claim, result integration.OutboxResult, requireLease bool) (bool, error) {
	completedAt := "NULL"
	if result.State == integration.OutboxCompleted || result.State == integration.OutboxRejected {
		completedAt = "clock_timestamp()"
	}
	var query string
	var args []any
	if requireLease {
		query = fmt.Sprintf(`
			UPDATE billing_outbox
			SET state=$1,worker=NULL,lease_deadline=NULL,provider_reference=COALESCE($2,provider_reference),
				completed_at=%s,last_error=$3,last_observation_id=$4
			WHERE account_id=$5 AND message_id=$6 AND fingerprint=$7 AND fence=$9
			  AND state='processing' AND worker=$8 AND lease_deadline > clock_timestamp()
			  AND last_observation_id IS NULL`, completedAt)
		args = []any{string(result.State), nullString(result.ProviderReference), result.Evidence,
			result.ObservationID, string(message.Account), message.ID, fingerprint, claim.Worker, claim.Fence}
	} else {
		query = fmt.Sprintf(`
			UPDATE billing_outbox
			SET state=$1,worker=NULL,lease_deadline=NULL,provider_reference=COALESCE($2,provider_reference),
				completed_at=%s,last_error=$3,last_observation_id=$4
			WHERE account_id=$5 AND message_id=$6 AND fingerprint=$7 AND fence=$8
			  AND state='unknown' AND COALESCE(last_observation_id,'')=$9`, completedAt)
		args = []any{string(result.State), nullString(result.ProviderReference), result.Evidence,
			result.ObservationID, string(message.Account), message.ID, fingerprint, claim.Fence, result.ExpectedPrevious}
	}
	dbResult, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return false, err
	}
	count, err := dbResult.RowsAffected()
	return count == 1, err
}

func (s *Store) FinishOutbox(ctx context.Context, claim integration.Claim, result integration.OutboxResult, callback func(integration.Session) error) error {
	if s == nil || s.db == nil || callback == nil {
		return fmt.Errorf("%w: nil database or callback", billing.ErrInvalid)
	}
	if err := result.Validate(); err != nil {
		return err
	}
	message, fingerprint, err := validateOutboxClaim(claim, true)
	if err != nil {
		return err
	}
	expected := outboxResultFingerprint(outboxFinish, fingerprint, claim.Fence, result)
	err = s.Atomic(ctx, message.Account, func(scope integration.Session) error {
		sess, ok := scope.(*session)
		if !ok || sess.tx == nil {
			return fmt.Errorf("%w: invalid postgres session", billing.ErrInvalid)
		}
		row, err := lockOutbox(ctx, sess.tx, message)
		if err != nil {
			return err
		}
		stored, found, err := readOutboxResult(ctx, sess.tx, message, result.ObservationID)
		if err != nil {
			return err
		}
		if found {
			if stored == expected {
				return nil
			}
			return billing.ErrConflict
		}
		if !activeOutboxClaim(row, claim, fingerprint) || !row.begun.Valid {
			if row.fingerprint == fingerprint && row.state == "processing" && row.worker == claim.Worker && row.fence == claim.Fence && !row.active {
				return errOutboxLeaseExpired
			}
			return billing.ErrConflict
		}
		if result.ExpectedPrevious != "" || row.lastObservation != "" {
			return billing.ErrConflict
		}
		if !compatibleOutboxReference(row, result) {
			return billing.ErrConflict
		}
		if err := insertOutboxResult(ctx, sess.tx, message, fingerprint, outboxFinish, claim.Fence, result); err != nil {
			return err
		}
		if err := callback(sess); err != nil {
			return err
		}
		updated, err := updateOutboxResult(ctx, sess.tx, message, fingerprint, claim, result, true)
		if err != nil {
			return err
		}
		if !updated {
			return errOutboxLeaseExpired
		}
		return nil
	})
	if errors.Is(err, errOutboxLeaseExpired) {
		if expireErr := s.expireOutboxClaim(ctx, message, fingerprint, claim); expireErr != nil && !errors.Is(expireErr, billing.ErrConflict) {
			return expireErr
		}
		return billing.ErrConflict
	}
	return err
}

func (s *Store) expireOutboxClaim(ctx context.Context, message integration.Message, fingerprint string, claim integration.Claim) error {
	return s.Atomic(ctx, message.Account, func(scope integration.Session) error {
		sess, ok := scope.(*session)
		if !ok || sess.tx == nil {
			return fmt.Errorf("%w: invalid postgres session", billing.ErrInvalid)
		}
		result, err := sess.tx.ExecContext(ctx, `
			UPDATE billing_outbox SET state='unknown',worker=NULL,lease_deadline=NULL,
				last_error=CASE WHEN last_error='' THEN 'lease expired' ELSE last_error END
			WHERE account_id=$1 AND message_id=$2 AND fingerprint=$3 AND fence=$4
			  AND state='processing' AND worker=$5 AND lease_deadline <= clock_timestamp()`,
			string(message.Account), message.ID, fingerprint, claim.Fence, claim.Worker)
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
	})
}

func (s *Store) ResolveOutbox(ctx context.Context, claim integration.Claim, result integration.OutboxResult, callback func(integration.Session) error) error {
	if s == nil || s.db == nil || callback == nil {
		return fmt.Errorf("%w: nil database or callback", billing.ErrInvalid)
	}
	if err := result.Validate(); err != nil {
		return err
	}
	message, fingerprint, err := validateOutboxClaim(claim, false)
	if err != nil {
		return err
	}
	expected := outboxResultFingerprint(outboxResolve, fingerprint, claim.Fence, result)
	return s.Atomic(ctx, message.Account, func(scope integration.Session) error {
		sess, ok := scope.(*session)
		if !ok || sess.tx == nil {
			return fmt.Errorf("%w: invalid postgres session", billing.ErrInvalid)
		}
		row, err := lockOutbox(ctx, sess.tx, message)
		if err != nil {
			return err
		}
		stored, found, err := readOutboxResult(ctx, sess.tx, message, result.ObservationID)
		if err != nil {
			return err
		}
		if found {
			if stored == expected {
				return nil
			}
			return billing.ErrConflict
		}
		if row.fingerprint != fingerprint || row.state != "unknown" || row.fence != claim.Fence {
			return billing.ErrConflict
		}
		if row.lastObservation != result.ExpectedPrevious {
			return billing.ErrConflict
		}
		if !compatibleOutboxReference(row, result) {
			return billing.ErrConflict
		}
		if err := insertOutboxResult(ctx, sess.tx, message, fingerprint, outboxResolve, claim.Fence, result); err != nil {
			return err
		}
		if err := callback(sess); err != nil {
			return err
		}
		updated, err := updateOutboxResult(ctx, sess.tx, message, fingerprint, claim, result, false)
		if err != nil {
			return err
		}
		if !updated {
			return billing.ErrConflict
		}
		return nil
	})
}

// ReleaseOutbox clears an unused send permission and requeues the message. The
// preconditions are deliberately narrow: the caller must still hold a live
// claim, the permission must have been granted, and no observation may have
// been recorded for it. Anything else means the outcome is not known to be
// unsent, and the caller must record unknown instead.
func (s *Store) ReleaseOutbox(ctx context.Context, claim integration.Claim, reason string, availableAt time.Time) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("%w: nil database", billing.ErrInvalid)
	}
	if reason == "" || len(reason) > 4096 || availableAt.IsZero() {
		return fmt.Errorf("%w: invalid outbox release", billing.ErrInvalid)
	}
	message, fingerprint, err := validateOutboxClaim(claim, true)
	if err != nil {
		return err
	}
	return s.Atomic(ctx, message.Account, func(scope integration.Session) error {
		sess, ok := scope.(*session)
		if !ok || sess.tx == nil {
			return fmt.Errorf("%w: invalid postgres session", billing.ErrInvalid)
		}
		row, err := lockOutbox(ctx, sess.tx, message)
		if err != nil {
			return err
		}
		if !activeOutboxClaim(row, claim, fingerprint) || !row.begun.Valid || row.lastObservation != "" {
			return billing.ErrConflict
		}
		var observations int
		if err := sess.tx.QueryRowContext(ctx, `SELECT count(*) FROM billing_outbox_results WHERE account_id=$1 AND message_id=$2`, string(message.Account), message.ID).Scan(&observations); err != nil {
			return err
		}
		if observations != 0 {
			return billing.ErrConflict
		}
		result, err := sess.tx.ExecContext(ctx, `
			UPDATE billing_outbox SET state='pending', begun_at=NULL, worker=NULL,
				lease_deadline=NULL, available_at=$6, last_error=$7
			WHERE account_id=$1 AND message_id=$2 AND fingerprint=$3
			  AND state='processing' AND worker=$4 AND fence=$5
			  AND begun_at IS NOT NULL AND lease_deadline > clock_timestamp()`,
			string(message.Account), message.ID, fingerprint, claim.Worker, claim.Fence, databaseTime(availableAt), reason)
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
	})
}
