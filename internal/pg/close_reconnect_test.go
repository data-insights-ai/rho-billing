package pg

import (
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/usage"
)

func TestPostgresCloseResumesAfterStoreReconnect(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	account := billing.AccountID("close-reconnect")
	if err := store.CreateAccount(ctx, account, "close-reconnect-subject"); err != nil {
		t.Fatal(err)
	}
	period := settlementTestPeriod()
	for i, id := range []string{"close-reconnect-a", "close-reconnect-b", "close-reconnect-c"} {
		record := settlementTestRecord(t, store, string(account), id, period.Start.Add(timeHour(i+1)), period.Start.Add(timeHour(i+2)), int64(i+1))
		if _, err := recordUsage(ctx, store, record); err != nil {
			t.Fatal(err)
		}
	}
	in := closeTestInput(account, "close-reconnect-op", "close-reconnect-batch")
	svc := usage.NewSettlement(store.Settlements(), func() time.Time { return period.Cutoff })
	job, err := svc.StartClose(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	job, err = svc.AdvanceClose(ctx, account, job.BatchID, job.Revision, 1)
	if err != nil || job.Processed != 1 || job.State != usage.ClosePreparing {
		t.Fatalf("first close page=%+v err=%v", job, err)
	}

	recovered := usage.NewSettlement(New(secondQueueDB(t, db)).Settlements(), func() time.Time { return period.Cutoff })
	status, err := recovered.CloseJob(ctx, account, job.BatchID)
	if err != nil || status.Revision != job.Revision || status.Processed != 1 {
		t.Fatalf("recovered close job=%+v err=%v", status, err)
	}
	for i := 0; i < 8 && status.State == usage.ClosePreparing; i++ {
		status, err = recovered.AdvanceClose(ctx, account, status.BatchID, status.Revision, 1)
		if err != nil {
			t.Fatal(err)
		}
	}
	if status.State != usage.CloseReady {
		t.Fatalf("close after reconnect state=%+v", status)
	}
	status, err = recovered.PublishClose(ctx, account, status.BatchID, status.Revision)
	if err != nil || status.State != usage.ClosePublished {
		t.Fatalf("published after reconnect=%+v err=%v", status, err)
	}
	summary, err := recovered.BatchSummary(ctx, account, status.BatchID)
	if err != nil || summary.Total != 6 || summary.LineCount != 3 {
		t.Fatalf("summary after reconnect=%+v err=%v", summary, err)
	}
}

func timeHour(n int) time.Duration {
	return time.Duration(n) * time.Hour
}
