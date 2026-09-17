package pg

import (
	"context"
	"errors"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/integration"
	"github.com/data-insights-ai/rho-billing/usage"
)

func TestPostgresWorkerCancelAndProcessDue(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	store, _ := testStoreAt(t, now)
	ctx := t.Context()
	account := billing.AccountID("lib55-acct")
	if err := store.CreateAccount(ctx, account, "lib55-subject"); err != nil {
		t.Fatal(err)
	}
	msg := integration.Message{Account: account, ID: "evt-1", Scope: billing.Scope{Provider: "provider", Merchant: "merchant", Environment: "test"}, Kind: "event", Direction: integration.Inbound, OccurredAt: now, Payload: []byte("body")}
	if err := store.Receive(ctx, msg); err != nil {
		t.Fatal(err)
	}
	svc := integration.NewOps(store.Ops(), func() time.Time { return now }).WithInbox(store)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	got, err := svc.ProcessDue(canceled, integration.ProcessDueInput{Worker: "w1", Limit: 10, Lease: time.Minute, MaxAttempts: 3, Handler: func(integration.Session) error { return nil }})
	if !errors.Is(err, context.Canceled) || !got.Canceled {
		t.Fatalf("cancel result=%+v err=%v", got, err)
	}
	result, err := svc.ProcessDue(ctx, integration.ProcessDueInput{Worker: "w1", Limit: 10, Lease: time.Minute, MaxAttempts: 3, Handler: func(integration.Session) error { return nil }})
	if err != nil || result.Processed != 1 {
		t.Fatalf("process=%+v err=%v", result, err)
	}
	empty, err := svc.ProcessDue(ctx, integration.ProcessDueInput{Worker: "w1", Limit: 10, Lease: time.Minute, MaxAttempts: 3, Handler: func(integration.Session) error { return nil }})
	if err != nil || empty.Claimed != 0 {
		t.Fatalf("empty=%+v err=%v", empty, err)
	}
}

func TestPostgresRepairBackfillAndMigrationDowngrade(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("lib56-acct")
	if err := store.CreateAccount(ctx, account, "lib56-subject"); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	credits := credit.New(store, func() time.Time { return now })
	if _, err := credits.Grant(ctx, credit.GrantInput{Account: account, Operation: "g1", LotID: "lot", Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 5, Source: "purchase", SourceRef: "pay", ValidFrom: now}); err != nil {
		t.Fatal(err)
	}
	svc := integration.NewOps(store.Ops(), func() time.Time { return now }).WithCredits(credits)
	dry, err := svc.Reconcile(ctx, integration.ReconcileInput{Account: account, DryRun: true})
	if err != nil || len(dry.Drifts) != 0 {
		t.Fatalf("dry-run=%+v err=%v", dry, err)
	}
	if _, err := svc.RecordRepair(ctx, integration.ReconcileInput{Account: account, RepairID: "repair-1", ExpectedRevision: 1, Actor: "ops", Reason: "verified", Evidence: "ledger"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RecordRepair(ctx, integration.ReconcileInput{Account: account, RepairID: "repair-1", ExpectedRevision: 1, Actor: "ops", Reason: "verified", Evidence: "ledger"}); err != nil {
		t.Fatal(err)
	}
	job, err := svc.AdvanceBackfill(ctx, integration.BackfillInput{Account: account, ID: "bf-1", Limit: 25})
	if err != nil || job.Processed != 25 || job.State != integration.BackfillRunning {
		t.Fatalf("backfill=%+v err=%v", job, err)
	}
	bal, err := credits.Balance(ctx, account, "credits", "")
	if err != nil || bal.Available != 5 {
		t.Fatalf("backfill mutated balance %+v err=%v", bal, err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := credits.Balance(ctx, account, "credits", ""); err != nil {
		t.Fatal(err)
	}
	var unsupported int64
	if err := db.QueryRowContext(ctx, `SELECT max(version)+1 FROM billing_schema_migrations`).Scan(&unsupported); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO billing_schema_migrations(version,checksum,applied_at,baseline_epoch) VALUES ($1,'future',CURRENT_TIMESTAMP,'rho-billing-release-2026-09-16')`, unsupported); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("newer schema migrate err=%v, want conflict", err)
	}
}

func TestPostgresArchiveRestoreAndRedaction(t *testing.T) {
	store, _ := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("lib58-acct")
	if err := store.CreateAccount(ctx, account, "lib58-subject"); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	config := usage.RuleConfig{Version: "ops-usage", Kind: usage.KindWeighted, Target: usage.Target{Currency: "USD"}, Rounding: usage.RoundDown, Weights: map[string]string{"tokens": "1"}}
	if err := store.PublishRating(ctx, config); err != nil {
		t.Fatal(err)
	}
	usageSvc := usage.New(store.UsageRepository(), func() time.Time { return now })
	if _, err := usageSvc.RateAndRecord(ctx, usage.Observation{Account: account, ID: "u1", Source: "meter", OccurredAt: now, Funding: usage.Postpaid, Input: usage.RateInput{Metrics: []usage.MetricQuantity{{Name: "tokens", Quantity: 2}}}}, config.Version); err != nil {
		t.Fatal(err)
	}
	credits := credit.New(store, func() time.Time { return now })
	if _, err := credits.Grant(ctx, credit.GrantInput{Account: account, Operation: "g1", LotID: "lot", Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 3, Source: "purchase", SourceRef: "pay", ValidFrom: now}); err != nil {
		t.Fatal(err)
	}
	if integration.Redact("Authorization: Bearer secret") != "[redacted]" {
		t.Fatal("postgres path must use the same redaction helper")
	}
	svc := integration.NewOps(store.Ops(), func() time.Time { return now }).WithUsage(usageSvc).WithCredits(credits)
	period := billing.Period{Start: now.Add(-time.Hour), End: now.Add(time.Hour)}
	snap, err := svc.Archive(ctx, account, period)
	if err != nil || len(snap.Tombstones) == 0 {
		t.Fatalf("archive=%+v err=%v", snap, err)
	}
	if err := svc.Restore(ctx, snap); err != nil {
		t.Fatal(err)
	}
	replay, err := usageSvc.RateAndRecord(ctx, usage.Observation{Account: account, ID: "u1", Source: "meter", OccurredAt: now, Funding: usage.Postpaid, Input: usage.RateInput{Metrics: []usage.MetricQuantity{{Name: "tokens", Quantity: 2}}}}, config.Version)
	if err != nil || replay.Observation.ID != "u1" {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	if _, err := usageSvc.RateAndRecord(ctx, usage.Observation{Account: account, ID: "u1", Source: "meter", OccurredAt: now, Funding: usage.Postpaid, Input: usage.RateInput{Metrics: []usage.MetricQuantity{{Name: "tokens", Quantity: 9}}}}, config.Version); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("changed redelivery err=%v", err)
	}
	if _, err := credits.Grant(ctx, credit.GrantInput{Account: account, Operation: "g1", LotID: "lot", Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 3, Source: "purchase", SourceRef: "pay", ValidFrom: now}); err != nil {
		t.Fatal(err)
	}
	bal, err := credits.Balance(ctx, account, "credits", "")
	if err != nil || bal.Available != 3 {
		t.Fatalf("grant replay doubled %+v err=%v", bal, err)
	}
}
