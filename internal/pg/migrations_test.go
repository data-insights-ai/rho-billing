package pg

import (
	"encoding/json"
	"errors"
	"io/fs"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	billing "github.com/data-insights-ai/rho-billing"
)

// A fresh install is one squashed baseline plus whatever has been added
// since. The baseline cannot be edited once it has been applied anywhere:
// the runner verifies its checksum and would refuse the whole database, so
// a change after a release is a new numbered file and never a line added
// to 001_release.sql. This test holds that shape.
func TestBaselineInstallStartsFromOneReleaseFile(t *testing.T) {
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 || entries[0].Name() != "001_release.sql" {
		t.Fatalf("the first migration must be 001_release.sql, got %v", entries)
	}
	numbered := regexp.MustCompile(`^(\d{3})_[a-z0-9_]+\.sql$`)
	previous := 0
	for _, e := range entries {
		match := numbered.FindStringSubmatch(e.Name())
		if match == nil {
			t.Fatalf("migration %q is not named NNN_name.sql", e.Name())
		}
		version := 0
		for _, r := range match[1] {
			version = version*10 + int(r-'0')
		}
		if version <= previous {
			t.Fatalf("migration %q does not follow version %d", e.Name(), previous)
		}
		previous = version
	}
}

func embeddedMigrationSection(t *testing.T, name string) string {
	t.Helper()
	data, err := fs.ReadFile(migrationFiles, "migrations/001_release.sql")
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	marker := "-- " + name + "\n"
	start := strings.Index(text, marker)
	if start < 0 {
		t.Fatalf("missing migration section %s", name)
	}
	rest := text[start+len(marker):]
	if loc := regexp.MustCompile(`\n-- \d{3}_`).FindStringIndex(rest); loc != nil {
		rest = rest[:loc[0]]
	}
	return rest
}

func releaseMigrationSnapshot(t *testing.T) fstest.MapFS {
	t.Helper()
	data, err := fs.ReadFile(migrationFiles, "migrations/001_release.sql")
	if err != nil {
		t.Fatal(err)
	}
	return fstest.MapFS{"migrations/001_release.sql": &fstest.MapFile{Data: data}}
}

func TestReleaseBaselineIsIdempotentAndRejectsUnsupportedHistory(t *testing.T) {
	store, db := testStoreWithMigrations(t, releaseMigrationSnapshot(t))
	ctx := t.Context()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var unsupported int64
	if err := db.QueryRowContext(ctx, `SELECT max(version)+1 FROM billing_schema_migrations`).Scan(&unsupported); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO billing_schema_migrations(version,checksum,applied_at,baseline_epoch) VALUES ($1,'unsupported-history',CURRENT_TIMESTAMP,'rho-billing-release-2026-09-16')`, unsupported); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("unsupported migration history error = %v, want conflict", err)
	}
	var version int
	if err := db.QueryRowContext(ctx, `SELECT max(version) FROM billing_schema_migrations`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if int64(version) != unsupported {
		t.Fatalf("unsupported history changed version to %d", version)
	}
}

func TestFailedReleaseMigrationRollsBackSchemaAndVersion(t *testing.T) {
	store, db := testStoreWithMigrations(t, releaseMigrationSnapshot(t))
	ctx := t.Context()
	broken := releaseMigrationSnapshot(t)
	broken["migrations/002_failure.sql"] = &fstest.MapFile{Data: []byte(`CREATE TABLE should_rollback(id bigint); SELECT * FROM table_that_does_not_exist;`)}
	if err := store.migrate(ctx, broken); err == nil {
		t.Fatal("invalid migration succeeded")
	}
	var exists bool
	if err := db.QueryRowContext(ctx, `SELECT to_regclass('should_rollback') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("failed migration left schema object behind")
	}
	var version int
	if err := db.QueryRowContext(ctx, `SELECT max(version) FROM billing_schema_migrations`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 1 {
		t.Fatalf("failed migration changed version to %d", version)
	}
}

func TestSettlementSummaryMigrationPreservesHistoricalOutcomeState(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	if err := store.CreateAccount(ctx, "acct-migration-summary", "migration-summary-subject"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO billing_settlement_operations(account_id,operation_id,fingerprint,error,result) VALUES($1,$2,$3,$4,jsonb_build_object('Batch',jsonb_build_object('Account',$1::text,'ID','historical-batch','OriginalBatchID','','Currency','USD','Total',7,'State','rejected','Revision',7,'CreatedAt','2026-09-02T00:00:00Z','UpdatedAt','2026-09-02T01:00:00Z','Lines',jsonb_build_array(jsonb_build_object('Kind','usage','UsageID','u-1','Amount',7))),'Attempt',jsonb_build_object('BatchID','historical-batch','ID','attempt-1','State','rejected','Revision',7)))`, "acct-migration-summary", "historical-operation", "operation-fingerprint", "rejected"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO billing_settlement_operations(account_id,operation_id,fingerprint,error,result) VALUES($1,$2,$3,$4,jsonb_build_object('Batch',jsonb_build_object('ID','zero-batch','State','rejected','Revision',3,'Lines',NULL),'Attempt',jsonb_build_object('BatchID','zero-batch','ID','attempt-zero','State','rejected','Revision',3)))`, "acct-migration-summary", "historical-zero", "operation-zero", "rejected"); err != nil {
		t.Fatal(err)
	}

	if _, err := db.ExecContext(ctx, embeddedMigrationSection(t, "006_settlement_summaries.sql")); err != nil {
		t.Fatal(err)
	}
	var result []byte
	if err := db.QueryRowContext(ctx, `SELECT result FROM billing_settlement_operations WHERE account_id=$1 AND operation_id=$2`, "acct-migration-summary", "historical-operation").Scan(&result); err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Batch map[string]any `json:"Batch"`
	}
	if err := json.Unmarshal(result, &decoded); err != nil {
		t.Fatal(err)
	}
	if _, ok := decoded.Batch["Lines"]; ok {
		t.Fatalf("historical lines were retained: %s", result)
	}
	if decoded.Batch["LineCount"] != float64(1) || decoded.Batch["State"] != "rejected" || decoded.Batch["Revision"] != float64(7) {
		t.Fatalf("historical summary changed: %s", result)
	}
	var zero []byte
	if err := db.QueryRowContext(ctx, `SELECT result FROM billing_settlement_operations WHERE account_id=$1 AND operation_id=$2`, "acct-migration-summary", "historical-zero").Scan(&zero); err != nil {
		t.Fatal(err)
	}
	var zeroDecoded struct {
		Batch map[string]any `json:"Batch"`
	}
	if err := json.Unmarshal(zero, &zeroDecoded); err != nil {
		t.Fatal(err)
	}
	if _, ok := zeroDecoded.Batch["Lines"]; ok || zeroDecoded.Batch["LineCount"] != float64(0) || zeroDecoded.Batch["Revision"] != float64(3) {
		t.Fatalf("null historical lines changed incorrectly: %s", zero)
	}
}
