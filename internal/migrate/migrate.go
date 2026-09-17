// Package migrate runs ordered, checksummed SQL migrations in one transaction.
package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/internal/identity"
)

const advisoryLockKey int64 = 0x72686f62696c6c

// baselineEpoch identifies the coherent pre-release schema that this runner
// understands. It is deliberately separate from the migration number: a
// database created by an earlier unreleased draft may have the same migration
// rows but a different table contract.
const baselineEpoch = "rho-billing-release-2026-09-16"

// Run applies the migrations below source atomically. The source must contain
// a migrations directory whose files use the ordered NNN_name.sql convention.
// Existing versions are verified by checksum; a database newer than the source
// is rejected.
func Run(ctx context.Context, db *sql.DB, source fs.FS) error {
	if db == nil || source == nil {
		return billing.ErrInvalid
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, advisoryLockKey); err != nil {
		return err
	}
	exists, err := migrationLedgerExists(ctx, tx)
	if err != nil {
		return err
	}
	if exists {
		compatible, err := migrationLedgerCompatible(ctx, tx)
		if err != nil {
			return err
		}
		if !compatible {
			return fmt.Errorf("%w: migration ledger belongs to an unsupported schema baseline", billing.ErrConflict)
		}
	} else {
		draft, err := draftSchemaObjectsExist(ctx, tx)
		if err != nil {
			return err
		}
		if draft {
			return fmt.Errorf("%w: existing billing schema has no supported migration baseline", billing.ErrConflict)
		}
		if _, err = tx.ExecContext(ctx, `CREATE TABLE billing_schema_migrations(version bigint PRIMARY KEY,checksum text NOT NULL,applied_at timestamptz NOT NULL,baseline_epoch text NOT NULL)`); err != nil {
			return err
		}
	}
	files, err := fs.ReadDir(source, "migrations")
	if err != nil {
		return err
	}
	var current int64
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM billing_schema_migrations`).Scan(&current); err != nil {
		return err
	}
	var last int64
	for _, file := range files {
		if file.IsDir() || !strings.HasSuffix(file.Name(), ".sql") {
			continue
		}
		prefix, _, ok := strings.Cut(file.Name(), "_")
		if !ok {
			return fmt.Errorf("invalid migration filename %s", file.Name())
		}
		version, err := strconv.ParseInt(prefix, 10, 64)
		if err != nil || version != last+1 {
			return fmt.Errorf("non-contiguous migration %s", file.Name())
		}
		last = version
		data, err := fs.ReadFile(source, "migrations/"+file.Name())
		if err != nil {
			return err
		}
		checksum := identity.Fingerprint(string(data))
		if version <= current {
			var old, epoch string
			if err = tx.QueryRowContext(ctx, `SELECT checksum,baseline_epoch FROM billing_schema_migrations WHERE version=$1`, version).Scan(&old, &epoch); err != nil {
				return err
			}
			if epoch != baselineEpoch {
				return fmt.Errorf("%w: migration %d belongs to unsupported schema baseline", billing.ErrConflict, version)
			}
			if old != checksum {
				return fmt.Errorf("%w: migration %d checksum changed", billing.ErrConflict, version)
			}
			continue
		}
		if _, err = tx.ExecContext(ctx, string(data)); err != nil {
			return fmt.Errorf("migration %d: %w", version, err)
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO billing_schema_migrations(version,checksum,applied_at,baseline_epoch) VALUES($1,$2,CURRENT_TIMESTAMP,$3)`, version, checksum, baselineEpoch); err != nil {
			return err
		}
	}
	if current > last {
		return fmt.Errorf("%w: database schema is newer than this library", billing.ErrConflict)
	}
	return tx.Commit()
}

func migrationLedgerExists(ctx context.Context, tx *sql.Tx) (bool, error) {
	var exists bool
	err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM information_schema.tables
			WHERE table_schema = current_schema()
			  AND table_name = 'billing_schema_migrations'
		)`).Scan(&exists)
	return exists, err
}

func migrationLedgerCompatible(ctx context.Context, tx *sql.Tx) (bool, error) {
	var marker bool
	err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM information_schema.columns
			WHERE table_schema = current_schema()
			  AND table_name = 'billing_schema_migrations'
			  AND column_name = 'baseline_epoch'
		)`).Scan(&marker)
	if err != nil || !marker {
		return marker, err
	}
	var epoch string
	err = tx.QueryRowContext(ctx, `
		SELECT baseline_epoch
		FROM billing_schema_migrations
		ORDER BY version
		LIMIT 1`).Scan(&epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return epoch == baselineEpoch, nil
}

func draftSchemaObjectsExist(ctx context.Context, tx *sql.Tx) (bool, error) {
	var exists bool
	err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM information_schema.tables
			WHERE table_schema = current_schema()
			  AND table_name LIKE 'billing_%'
		)`).Scan(&exists)
	return exists, err
}
