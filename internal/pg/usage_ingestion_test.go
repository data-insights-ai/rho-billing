package pg

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/data-insights-ai/rho-billing/integration"
	"github.com/data-insights-ai/rho-billing/usage"
)

func TestUsageIngestionUpgradeWatermarkReplayAndRollback(t *testing.T) {
	store, db := testStoreWithMigrations(t, releaseMigrationSnapshot(t))
	ctx := t.Context()
	if err := store.CreateAccount(ctx, "ingestion", "ingestion-tenant"); err != nil {
		t.Fatal(err)
	}
	config := usage.RuleConfig{Version: "ingestion-rule", Kind: usage.KindFixed, Target: usage.Target{Currency: "EUR"}, Rounding: usage.RoundDown, FixedRate: "1"}
	if err := store.PublishRating(ctx, config); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)
	service := usage.New(store.UsageRepository(), func() time.Time { return now })
	observation := usage.Observation{Account: "ingestion", ID: "before-upgrade", Source: "meter", OccurredAt: now, Funding: usage.Postpaid, Input: usage.RateInput{ActionCount: 1}}
	first, err := service.RateAndRecord(ctx, observation, config.Version)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var watermark int64
	if err := db.QueryRowContext(ctx, `SELECT max(ingestion_sequence) FROM billing_usage WHERE account_id='ingestion'`).Scan(&watermark); err != nil || watermark <= 0 {
		t.Fatalf("upgraded watermark=%d error=%v", watermark, err)
	}
	// An exact replay cannot create a new ingestion position or replace its
	// receipt evidence, even when the service clock moves forward.
	now = now.Add(time.Hour)
	replayed, err := service.RateAndRecord(ctx, observation, config.Version)
	if err != nil || replayed.ReceivedAt != first.ReceivedAt {
		t.Fatalf("upgraded replay=%+v error=%v", replayed, err)
	}
	var afterReplay int64
	if err := db.QueryRowContext(ctx, `SELECT max(ingestion_sequence) FROM billing_usage WHERE account_id='ingestion'`).Scan(&afterReplay); err != nil || afterReplay != watermark {
		t.Fatalf("replay advanced watermark from %d to %d: %v", watermark, afterReplay, err)
	}
	// Both operational timestamps can be backdated; storage sequence cannot.
	now = now.Add(-48 * time.Hour)
	observation.OccurredAt = now
	observation.ID = "late-backdated"
	if _, err := service.RateAndRecord(ctx, observation, config.Version); err != nil {
		t.Fatal(err)
	}
	var lateSequence int64
	if err := db.QueryRowContext(ctx, `SELECT ingestion_sequence FROM billing_usage WHERE account_id='ingestion' AND usage_id='late-backdated'`).Scan(&lateSequence); err != nil || lateSequence <= watermark {
		t.Fatalf("backdated ingestion sequence=%d watermark=%d error=%v", lateSequence, watermark, err)
	}
	observation.ID = "rolled-back"
	sentinel := errors.New("abort ingestion")
	if err := store.Atomic(ctx, observation.Account, func(session integration.Session) error {
		if _, err := usage.New(session.Usage(), func() time.Time { return now }).RateAndRecord(ctx, observation, config.Version); err != nil {
			return err
		}
		return sentinel
	}); !errors.Is(err, sentinel) {
		t.Fatalf("rollback error=%v", err)
	}
	observation.ID = "after-rollback"
	if _, err := service.RateAndRecord(ctx, observation, config.Version); err != nil {
		t.Fatal(err)
	}
	var newer, frozen int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FILTER(WHERE ingestion_sequence>$1),count(*) FILTER(WHERE ingestion_sequence<=$1) FROM billing_usage WHERE account_id='ingestion'`, watermark).Scan(&newer, &frozen); err != nil || newer != 2 || frozen != 1 {
		t.Fatalf("watermark membership newer=%d frozen=%d error=%v", newer, frozen, err)
	}
}

// The close competes with a host transaction that has already inserted usage.
// The account lock must fence the watermark after that transaction commits.
func TestUsageIngestionWatermarkWaitsForAccountTransaction(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	if err := store.CreateAccount(ctx, "watermark-lock", "watermark-lock-tenant"); err != nil {
		t.Fatal(err)
	}
	config := usage.RuleConfig{Version: "watermark-lock-rule", Kind: usage.KindFixed, Target: usage.Target{Currency: "EUR"}, Rounding: usage.RoundDown, FixedRate: "1"}
	if err := store.PublishRating(ctx, config); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)
	secondDB := secondQueueDB(t, db)
	second := New(secondDB)
	var closePID int
	if err := secondDB.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&closePID); err != nil {
		t.Fatal(err)
	}
	monitor := secondQueueDB(t, db)
	inserted := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	writeResult := make(chan error, 1)
	go func() {
		writeResult <- store.Atomic(ctx, "watermark-lock", func(session integration.Session) error {
			_, err := usage.New(session.Usage(), func() time.Time { return now }).RateAndRecord(ctx, usage.Observation{Account: "watermark-lock", ID: "in-flight", Source: "meter", OccurredAt: now.Add(-time.Hour), Funding: usage.Postpaid, Input: usage.RateInput{ActionCount: 1}}, config.Version)
			if err != nil {
				return err
			}
			close(inserted)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	select {
	case <-inserted:
	case err := <-writeResult:
		t.Fatalf("usage transaction failed before lock barrier: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	closeResult := make(chan struct {
		job usage.CloseJob
		err error
	}, 1)
	go func() {
		job, err := usage.NewSettlement(second.Settlements(), func() time.Time { return now }).StartClose(ctx, usage.CloseInput{Account: "watermark-lock", Operation: "close", BatchID: "batch", Currency: "EUR", Period: usage.BillingPeriod{Start: now.Add(-2 * time.Hour), End: now, Cutoff: now}})
		closeResult <- struct {
			job usage.CloseJob
			err error
		}{job, err}
	}()
	// Observe an actual PostgreSQL lock wait instead of assuming goroutine
	// scheduling or relying on a sleep to establish transaction ordering.
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		var blocked bool
		if err := monitor.QueryRowContext(ctx, `SELECT cardinality(pg_blocking_pids($1)) > 0`, closePID).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case result := <-closeResult:
			t.Fatalf("close passed uncommitted account usage: job=%+v err=%v", result.job, result.err)
		case <-deadline.C:
			t.Fatal("close never entered the account lock wait")
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-tick.C:
		}
	}
	unblock()
	if err := <-writeResult; err != nil {
		t.Fatal(err)
	}
	closed := <-closeResult
	if closed.err != nil {
		t.Fatal(closed.err)
	}
	var sequence int64
	if err := db.QueryRowContext(ctx, `SELECT ingestion_sequence FROM billing_usage WHERE account_id='watermark-lock' AND usage_id='in-flight'`).Scan(&sequence); err != nil {
		t.Fatal(err)
	}
	if sequence == 0 || closed.job.Watermark < sequence {
		t.Fatalf("watermark=%d omits previously committed sequence=%d", closed.job.Watermark, sequence)
	}
}
