package pg

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/billingtest"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/integration"
	"github.com/data-insights-ai/rho-billing/usage"
)

func TestPostgresMixedLoadAndReconnect(t *testing.T) {
	if os.Getenv("BILLING_LOAD_TEST") != "1" && os.Getenv("BILLING_MIXED_SECONDS") == "" {
		t.Skip("set BILLING_LOAD_TEST=1 or BILLING_MIXED_SECONDS for measured mixed load")
	}
	runLIB62Load(t, mixedDuration(), "mixed")
}

func TestPostgresLoadStorePoolMatchesWorkloadContract(t *testing.T) {
	_, db := testLoadStore(t)
	if db.Stats().MaxOpenConnections != billingtest.WorkloadDatabasePool {
		t.Fatalf("load store pool=%d, want %d", db.Stats().MaxOpenConnections, billingtest.WorkloadDatabasePool)
	}
}

func TestPostgresSoakHasNoDefaultDuration(t *testing.T) {
	if os.Getenv("BILLING_SOAK_SECONDS") != "" {
		t.Skip("explicit soak requested")
	}
	if soakDuration() != 0 {
		t.Fatal("soak must not have a default duration")
	}
}

func TestPostgresSoak(t *testing.T) {
	if os.Getenv("BILLING_SOAK_SECONDS") == "" {
		t.Skip("set BILLING_SOAK_SECONDS for an explicit soak; BILLING_LOAD_TEST runs mixed only")
	}
	d := soakDuration()
	if d <= 0 {
		t.Skip("BILLING_SOAK_SECONDS must be a positive integer")
	}
	runLIB62Load(t, d, "soak")
}

func mixedDuration() time.Duration {
	if raw := os.Getenv("BILLING_MIXED_SECONDS"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return billingtest.WorkloadMixedDuration
}

func soakDuration() time.Duration {
	n, err := strconv.Atoi(os.Getenv("BILLING_SOAK_SECONDS"))
	if err != nil || n <= 0 {
		return 0
	}
	return time.Duration(n) * time.Second
}

func runLIB62Load(t *testing.T, duration time.Duration, label string) {
	t.Helper()
	store, db := testLoadStore(t)
	if db.Stats().MaxOpenConnections != billingtest.WorkloadDatabasePool {
		t.Fatalf("load database pool=%d, want %d", db.Stats().MaxOpenConnections, billingtest.WorkloadDatabasePool)
	}
	t.Logf("load database pool=%d in-flight=%d", db.Stats().MaxOpenConnections, billingtest.WorkloadInFlight)
	ctx := t.Context()
	now := testTime()
	account := billing.AccountID("lib62-" + label)
	if err := store.CreateAccount(ctx, account, string(account)); err != nil {
		t.Fatal(err)
	}
	insertWorkLots(t, db, string(account), now, billingtest.WorkloadHotLots, now.Add(24*time.Hour))
	readyWorkProjection(t, store, account)
	engine := credit.New(store, func() time.Time { return now })
	var liveEngine atomic.Pointer[credit.Engine]
	var liveUsage atomic.Pointer[usage.Service]
	liveEngine.Store(engine)
	for i := range billingtest.WorkloadHotHolds {
		if _, err := engine.Reserve(ctx, credit.ReserveInput{
			Account: account, Operation: billing.OperationID(fmt.Sprintf("seed-hold-%d", i)),
			ReservationID: fmt.Sprintf("seed-hold-%d", i), Actor: "load-actor", Unit: "credits",
			Amount: 1, Deadline: now.Add(time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
	}
	config := usage.RuleConfig{Version: "lib62-" + label, Kind: usage.KindWeighted, Target: usage.Target{Currency: "USD"}, Rounding: usage.RoundDown, Weights: map[string]string{"tokens": "1"}}
	if err := store.PublishRating(ctx, config); err != nil {
		t.Fatal(err)
	}
	usageSvc := usage.New(store.UsageRepository(), func() time.Time { return now })
	liveUsage.Store(usageSvc)
	var offered, completed, rejected, technical, dropped atomic.Int64
	var mu sync.Mutex
	var balanceSamples, reserveSamples []time.Duration
	inflight := make(chan struct{}, billingtest.WorkloadInFlight)
	deadline := time.Now().Add(duration)
	warmup := time.Minute
	if duration < 2*time.Minute {
		warmup = duration / 4
	}
	warmupUntil := time.Now().Add(warmup)
	var seq atomic.Int64
	ticks := make(chan struct{}, billingtest.WorkloadInFlight)
	var pacer sync.WaitGroup
	pacer.Go(func() {
		ticker := time.NewTicker(time.Second / time.Duration(billingtest.WorkloadOfferedOps))
		defer ticker.Stop()
		stop := time.NewTimer(time.Until(deadline))
		defer stop.Stop()
		for {
			select {
			case <-stop.C:
				close(ticks)
				return
			case <-ticker.C:
				select {
				case ticks <- struct{}{}:
				default:
					dropped.Add(1)
				}
			}
		}
	})
	var wg sync.WaitGroup
	worker := func(id int) {
		for range ticks {
			offered.Add(1)
			select {
			case inflight <- struct{}{}:
			default:
				dropped.Add(1)
				continue
			}
			n := seq.Add(1)
			kind := n % 20
			start := time.Now()
			engine := liveEngine.Load()
			usageSvc := liveUsage.Load()
			var err error
			switch {
			case kind < 8:
				_, err = engine.Balance(ctx, account, "credits", "")
				if err == nil && time.Now().After(warmupUntil) {
					mu.Lock()
					balanceSamples = append(balanceSamples, time.Since(start))
					mu.Unlock()
				}
			case kind < 13:
				rid := fmt.Sprintf("w%d-%d", id, n)
				_, err = engine.Reserve(ctx, credit.ReserveInput{Account: account, Operation: billing.OperationID(rid), ReservationID: rid, Actor: "load-actor", Unit: "credits", Amount: 1, Deadline: now.Add(time.Hour)})
				if err == nil && time.Now().After(warmupUntil) {
					mu.Lock()
					reserveSamples = append(reserveSamples, time.Since(start))
					mu.Unlock()
				}
			case kind < 18:
				rid := fmt.Sprintf("rel-%d-%d", id, n)
				if _, rerr := engine.Reserve(ctx, credit.ReserveInput{Account: account, Operation: billing.OperationID(rid), ReservationID: rid, Actor: "load-actor", Unit: "credits", Amount: 1, Deadline: now.Add(time.Hour)}); rerr != nil {
					err = rerr
				} else {
					_, err = engine.Release(ctx, credit.ReleaseInput{Account: account, Operation: billing.OperationID(rid + "-out"), ReservationID: rid, Reason: "load"})
				}
			case kind == 18:
				_, err = engine.Grant(ctx, credit.GrantInput{Account: account, Operation: billing.OperationID(fmt.Sprintf("g-%d-%d", id, n)), LotID: fmt.Sprintf("g-%d-%d", id, n), Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 1, Source: "load", SourceRef: fmt.Sprintf("g-%d-%d", id, n), ValidFrom: now})
			default:
				_, err = usageSvc.RateAndRecord(ctx, usage.Observation{Account: account, ID: fmt.Sprintf("u-%d-%d", id, n), Source: "load", OccurredAt: now, Funding: usage.Postpaid, Input: usage.RateInput{Metrics: []usage.MetricQuantity{{Name: "tokens", Quantity: 1}}}}, config.Version)
			}
			<-inflight
			if err == nil {
				completed.Add(1)
			} else if errors.Is(err, billing.ErrInsufficient) || errors.Is(err, billing.ErrLimit) || errors.Is(err, billing.ErrConflict) || errors.Is(err, billing.ErrInvalid) {
				rejected.Add(1)
			} else if loadStoreClosed(err) {
				dropped.Add(1)
			} else {
				technical.Add(1)
			}
		}
	}
	for i := range 8 {
		wg.Go(func() { worker(i) })
	}
	time.Sleep(min(warmup/2, 2*time.Second))
	recoveredStore, recoveredEngine := injectLIB62Failures(t, store, db, account, now, engine, billingtest.WorkloadDatabasePool)
	liveEngine.Store(recoveredEngine)
	liveUsage.Store(usage.New(recoveredStore.UsageRepository(), func() time.Time { return now }))
	pacer.Wait()
	wg.Wait()
	balanceP50 := durationPercentile(balanceSamples, 0.50)
	balanceP95 := durationPercentile(balanceSamples, 0.95)
	balanceP99 := durationPercentile(balanceSamples, 0.99)
	reserveP95 := durationPercentile(reserveSamples, 0.95)
	reserveP99 := durationPercentile(reserveSamples, 0.99)
	t.Logf("%s duration=%s pool=%d in-flight=%d offered=%d completed=%d rejected=%d dropped=%d technical=%d balance p50=%s p95=%s p99=%s reserve p95=%s p99=%s samples_balance=%d samples_reserve=%d",
		label, duration, billingtest.WorkloadDatabasePool, billingtest.WorkloadInFlight, offered.Load(), completed.Load(), rejected.Load(), dropped.Load(), technical.Load(), balanceP50, balanceP95, balanceP99, reserveP95, reserveP99, len(balanceSamples), len(reserveSamples))
	if technical.Load() > 0 {
		t.Fatalf("%s technical errors=%d", label, technical.Load())
	}
	if len(balanceSamples) > 0 && balanceP95 > billingtest.WorkloadBalanceP95 {
		t.Fatalf("%s balance p95=%s exceeds %s", label, balanceP95, billingtest.WorkloadBalanceP95)
	}
	if len(balanceSamples) > 0 && balanceP99 > billingtest.WorkloadBalanceP99 {
		t.Fatalf("%s balance p99=%s exceeds %s", label, balanceP99, billingtest.WorkloadBalanceP99)
	}
	if len(reserveSamples) > 0 && reserveP95 > billingtest.WorkloadReserveP95 {
		t.Fatalf("%s reserve p95=%s exceeds %s", label, reserveP95, billingtest.WorkloadReserveP95)
	}
	if len(reserveSamples) > 0 && reserveP99 > billingtest.WorkloadReserveP99 {
		t.Fatalf("%s reserve p99=%s exceeds %s", label, reserveP99, billingtest.WorkloadReserveP99)
	}
	bal, err := recoveredEngine.Balance(ctx, account, "credits", "")
	if err != nil || bal.Available < 0 || bal.Held < 0 {
		t.Fatalf("%s final balance=%+v err=%v", label, bal, err)
	}
}

func injectLIB62Failures(t *testing.T, store *Store, db *sql.DB, account billing.AccountID, now time.Time, engine *credit.Engine, pool int) (*Store, *credit.Engine) {
	t.Helper()
	ctx := t.Context()
	msg := queueMessage(account, "lib62-worker-kill", integration.Outbound, "kill")
	enqueueOutboundForTest(t, store, msg)
	wall := time.Now().UTC().Add(time.Second)
	claim, ok, err := store.Claim(ctx, integration.Outbound, "dead-worker", wall, time.Minute)
	if err != nil || !ok || claim.Message.ID != msg.ID {
		t.Fatalf("worker-kill claim=%+v ok=%v err=%v", claim, ok, err)
	}
	if _, err := db.ExecContext(ctx, `
		UPDATE billing_outbox
		SET lease_deadline = $1, available_at = $2
		WHERE account_id = $3 AND message_id = $4`,
		wall.Add(-time.Minute), wall.Add(-time.Hour), string(account), msg.ID); err != nil {
		t.Fatal(err)
	}
	var schema string
	if err := db.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Balance(ctx, account, "credits", ""); err == nil {
		t.Fatal("load workers' store still served Balance after Close")
	}
	reopened := searchPathDBPool(t, schema, pool)
	if reopened.Stats().MaxOpenConnections != pool {
		t.Fatalf("recovered database pool=%d, want %d", reopened.Stats().MaxOpenConnections, pool)
	}
	t.Cleanup(func() {
		_, _ = reopened.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+schema+` CASCADE`)
	})
	recovered := New(reopened)
	recoveredEngine := credit.New(recovered.Credits(), func() time.Time { return now })
	if _, err := recoveredEngine.Balance(ctx, account, "credits", ""); err != nil {
		t.Fatalf("connection-loss recovery balance err=%v", err)
	}
	if _, _, err := recovered.Claim(ctx, integration.Outbound, "successor-worker", wall, time.Minute); err != nil {
		t.Fatalf("lease-expiry recovery claim err=%v", err)
	}
	var state string
	if err := reopened.QueryRowContext(ctx, `SELECT state FROM billing_outbox WHERE account_id=$1 AND message_id=$2`, account, msg.ID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "unknown" {
		t.Fatalf("worker-death lease expiry state=%s, want unknown", state)
	}
	t.Logf("lib62 injection: closed load store schema=%s recovered pool=%d; recovered Balance and Claim; outbox state=%s", schema, reopened.Stats().MaxOpenConnections, state)
	return recovered, recoveredEngine
}

func loadStoreClosed(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, sql.ErrConnDone) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "database is closed") || strings.Contains(msg, "closed pool") || strings.Contains(msg, "conn closed")
}
