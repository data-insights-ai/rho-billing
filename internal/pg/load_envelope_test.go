package pg

import (
	"fmt"
	"math"
	"slices"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/billingtest"
	"github.com/data-insights-ai/rho-billing/credit"
)

func TestPostgresHotAndManyAccountEnvelope(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	now := testTime()
	hot := billing.AccountID("load-hot")
	if err := store.CreateAccount(ctx, hot, "load-hot-subject"); err != nil {
		t.Fatal(err)
	}
	insertWorkLots(t, db, string(hot), now, billingtest.WorkloadHotLots, now.Add(24*time.Hour))
	readyWorkProjection(t, store, hot)
	engine := credit.New(store, func() time.Time { return now })
	for i := range billingtest.WorkloadHotHolds {
		if _, err := engine.Reserve(ctx, credit.ReserveInput{
			Account: hot, Operation: billing.OperationID(fmt.Sprintf("hot-hold-%d", i)),
			ReservationID: fmt.Sprintf("hot-hold-%d", i), Actor: "load-actor", Unit: "credits",
			Amount: 1, Deadline: now.Add(time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
	}
	wantHot := credit.Balance{Available: int64(billingtest.WorkloadHotLots - billingtest.WorkloadHotHolds), Held: int64(billingtest.WorkloadHotHolds)}
	got, err := engine.Balance(ctx, hot, "credits", "")
	if err != nil || got != wantHot {
		t.Fatalf("hot balance=%+v err=%v, want %+v", got, err, wantHot)
	}

	const samples = 80
	balanceSamples := make([]time.Duration, 0, samples)
	for range 10 {
		if _, err := engine.Balance(ctx, hot, "credits", ""); err != nil {
			t.Fatal(err)
		}
	}
	for range samples {
		start := time.Now()
		got, err := engine.Balance(ctx, hot, "credits", "")
		elapsed := time.Since(start)
		if err != nil || got != wantHot {
			t.Fatalf("hot balance sample=%+v err=%v", got, err)
		}
		balanceSamples = append(balanceSamples, elapsed)
	}
	reserveSamples := make([]time.Duration, 0, samples)
	for i := range samples {
		start := time.Now()
		id := fmt.Sprintf("hot-cycle-%d", i)
		if _, err := engine.Reserve(ctx, credit.ReserveInput{
			Account: hot, Operation: billing.OperationID(id), ReservationID: id,
			Actor: "load-actor", Unit: "credits", Amount: 1, Deadline: now.Add(time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := engine.Release(ctx, credit.ReleaseInput{Account: hot, Operation: billing.OperationID(id + "-rel"), ReservationID: id, Reason: "load-cycle"}); err != nil {
			t.Fatal(err)
		}
		reserveSamples = append(reserveSamples, time.Since(start))
	}

	many := 100
	lotsEach := 10
	for i := range many {
		account := billing.AccountID(fmt.Sprintf("load-many-%03d", i))
		if err := store.CreateAccount(ctx, account, string(account)); err != nil {
			t.Fatal(err)
		}
		insertWorkLots(t, db, string(account), now, lotsEach, now.Add(24*time.Hour))
		readyWorkProjection(t, store, account)
		got, err := engine.Balance(ctx, account, "credits", "")
		if err != nil || got.Available != int64(lotsEach) {
			t.Fatalf("account %s balance=%+v err=%v", account, got, err)
		}
	}

	balanceP95 := durationPercentile(balanceSamples, 0.95)
	balanceP99 := durationPercentile(balanceSamples, 0.99)
	reserveP95 := durationPercentile(reserveSamples, 0.95)
	reserveP99 := durationPercentile(reserveSamples, 0.99)
	t.Logf("hot lots=%d holds=%d balance p95=%s p99=%s reserve+release p95=%s p99=%s many-accounts=%d lots-each=%d",
		billingtest.WorkloadHotLots, billingtest.WorkloadHotHolds, balanceP95, balanceP99, reserveP95, reserveP99, many, lotsEach)
	if balanceP95 > billingtest.WorkloadBalanceP95 {
		t.Fatalf("hot balance p95=%s exceeds %s", balanceP95, billingtest.WorkloadBalanceP95)
	}
	if balanceP99 > billingtest.WorkloadBalanceP99 {
		t.Fatalf("hot balance p99=%s exceeds %s", balanceP99, billingtest.WorkloadBalanceP99)
	}
	if reserveP95 > billingtest.WorkloadReserveP95 {
		t.Fatalf("hot reserve+release p95=%s exceeds %s", reserveP95, billingtest.WorkloadReserveP95)
	}
	if reserveP99 > billingtest.WorkloadReserveP99 {
		t.Fatalf("hot reserve+release p99=%s exceeds %s", reserveP99, billingtest.WorkloadReserveP99)
	}
}

func durationPercentile(samples []time.Duration, p float64) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	ordered := slices.Clone(samples)
	slices.Sort(ordered)
	index := int(math.Ceil(p*float64(len(ordered)))) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(ordered) {
		index = len(ordered) - 1
	}
	return ordered[index]
}
