package pg

import (
	"database/sql"
	"errors"
	"io/fs"
	"testing"
	"testing/fstest"

	billing "github.com/data-insights-ai/rho-billing"
)

// emptyReleaseStore gives each safety test a fresh, isolated schema without
// production tables. Only this test-owned schema's empty ledger is removed.
func emptyReleaseStore(t *testing.T) (*Store, *sql.DB) {
	t.Helper()
	store, db := testStoreWithMigrations(t, fstest.MapFS{"migrations": &fstest.MapFile{Mode: fs.ModeDir}})
	if _, err := db.ExecContext(t.Context(), `DROP TABLE billing_schema_migrations`); err != nil {
		t.Fatal(err)
	}
	return store, db
}

func TestReleaseBaselineRefusesDraftHistoryWithoutChangingData(t *testing.T) {
	for _, ledger := range []bool{true, false} {
		name := "without-ledger"
		if ledger {
			name = "seventeen-draft-versions"
		}
		t.Run(name, func(t *testing.T) {
			store, db := emptyReleaseStore(t)
			ctx := t.Context()
			if _, err := db.ExecContext(ctx, `CREATE TABLE billing_draft_evidence(id bigint PRIMARY KEY, payload text NOT NULL); INSERT INTO billing_draft_evidence VALUES(7,'retained financial evidence')`); err != nil {
				t.Fatal(err)
			}
			if ledger {
				if _, err := db.ExecContext(ctx, `CREATE TABLE billing_schema_migrations(version bigint PRIMARY KEY, checksum text NOT NULL, applied_at timestamptz NOT NULL); INSERT INTO billing_schema_migrations SELECT n,'draft-'||n,CURRENT_TIMESTAMP FROM generate_series(1,17) n`); err != nil {
					t.Fatal(err)
				}
			}
			before := releaseTableInventory(t, db)
			if err := store.Migrate(ctx); !errors.Is(err, billing.ErrConflict) {
				t.Fatalf("draft migration error=%v, want conflict", err)
			}
			if after := releaseTableInventory(t, db); after != before {
				t.Fatalf("draft schema changed: before=%s after=%s", before, after)
			}
			var payload string
			if err := db.QueryRowContext(ctx, `SELECT payload FROM billing_draft_evidence WHERE id=7`).Scan(&payload); err != nil || payload != "retained financial evidence" {
				t.Fatalf("draft data changed: %q error=%v", payload, err)
			}
			if ledger {
				var valid bool
				if err := db.QueryRowContext(ctx, `SELECT count(*)=17 AND bool_and(checksum='draft-'||version) FROM billing_schema_migrations`).Scan(&valid); err != nil || !valid {
					t.Fatalf("draft ledger changed: valid=%v error=%v", valid, err)
				}
			}
		})
	}
}

func releaseTableInventory(t *testing.T, db *sql.DB) string {
	t.Helper()
	var result string
	if err := db.QueryRowContext(t.Context(), `SELECT COALESCE(string_agg(table_name||'.'||column_name||':'||data_type||':'||is_nullable,',' ORDER BY table_name,ordinal_position),'') FROM information_schema.columns WHERE table_schema=current_schema()`).Scan(&result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestReleaseBaselineFailureRollsBackFreshInstallation(t *testing.T) {
	store, db := emptyReleaseStore(t)
	data, err := migrationFiles.ReadFile("migrations/001_release.sql")
	if err != nil {
		t.Fatal(err)
	}
	broken := fstest.MapFS{"migrations/001_release.sql": &fstest.MapFile{Data: append(data, []byte("\nSELECT * FROM deliberate_missing_release_table;")...)}}
	if err := store.migrate(t.Context(), broken); err == nil {
		t.Fatal("failed baseline succeeded")
	}
	if inventory := releaseTableInventory(t, db); inventory != "" {
		t.Fatalf("failed baseline left tables or migration ledger: %s", inventory)
	}
	if err := store.Migrate(t.Context()); err != nil {
		t.Fatalf("fresh retry after rollback: %v", err)
	}
}

func TestReleaseBaselineRejectsChecksumAndEpochChangesBeforeLaterDDL(t *testing.T) {
	for name, mutation := range map[string]string{
		"checksum": `UPDATE billing_schema_migrations SET checksum='changed' WHERE version=1`,
		"epoch":    `UPDATE billing_schema_migrations SET baseline_epoch='unsupported' WHERE version=1`,
	} {
		t.Run(name, func(t *testing.T) {
			store, db := testStore(t)
			ctx := t.Context()
			if _, err := db.ExecContext(ctx, mutation); err != nil {
				t.Fatal(err)
			}
			data, err := migrationFiles.ReadFile("migrations/001_release.sql")
			if err != nil {
				t.Fatal(err)
			}
			source := fstest.MapFS{
				"migrations/001_release.sql":      &fstest.MapFile{Data: data},
				"migrations/002_must_not_run.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE must_not_run(id bigint)`)},
			}
			before := releaseTableInventory(t, db)
			if err := store.migrate(ctx, source); !errors.Is(err, billing.ErrConflict) {
				t.Fatalf("mutated ledger error=%v, want conflict", err)
			}
			if after := releaseTableInventory(t, db); after != before {
				t.Fatal("refused migration changed the schema")
			}
		})
	}
}

func TestReleaseBaselineHasRequiredProjectionsWithoutDraftObjects(t *testing.T) {
	_, db := testStore(t)
	for table, column := range map[string]string{
		"billing_allowance_schedule_history": "state_effective_at",
		"billing_allowance_schedules":        "assignment_start",
	} {
		var nullable string
		if err := db.QueryRowContext(t.Context(), `SELECT is_nullable FROM information_schema.columns WHERE table_schema=current_schema() AND table_name=$1 AND column_name=$2`, table, column).Scan(&nullable); err != nil || nullable != "NO" {
			t.Fatalf("required %s.%s nullable=%q error=%v", table, column, nullable, err)
		}
	}
	for _, table := range []string{"billing_allowance_lineage", "billing_allowance_successors", "billing_credit_account_balances", "billing_credit_scope_balances"} {
		var exists bool
		if err := db.QueryRowContext(t.Context(), `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&exists); err != nil || !exists {
			t.Fatalf("missing baseline projection %s: exists=%v error=%v", table, exists, err)
		}
	}
	for _, object := range []string{"billing_settlement_close_commands", "billing_allowance_lineage_work", "billing_allowance_history_unprepared_idx", "billing_allowance_lineage_unprepared_idx", "billing_allowance_successor_unprepared_idx"} {
		var exists bool
		if err := db.QueryRowContext(t.Context(), `SELECT to_regclass($1) IS NOT NULL`, object).Scan(&exists); err != nil || exists {
			t.Fatalf("obsolete draft object %s: exists=%v error=%v", object, exists, err)
		}
	}
}
