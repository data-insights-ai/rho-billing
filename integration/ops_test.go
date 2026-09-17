package integration

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"

	"github.com/data-insights-ai/rho-billing/usage"
)

func TestWorkerHonorsCancelFenceRetryAndBackpressure(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	inbox := newMemoryInbox()
	svc := NewOps(NewMemoryOpsRepository("acct"), func() time.Time { return now }).WithInbox(inbox)
	msg := Message{Account: "acct", ID: "evt-1", Scope: billing.Scope{Provider: "p", Merchant: "m", Environment: "test"}, Kind: "event", Direction: Inbound, OccurredAt: now, Payload: []byte("body")}
	if err := inbox.Receive(t.Context(), msg); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	got, err := svc.ProcessDue(canceled, ProcessDueInput{Worker: "w1", Limit: 10, Lease: time.Minute, MaxAttempts: 3, Handler: func(Session) error { return nil }})
	if !errors.Is(err, context.Canceled) || !got.Canceled || inbox.Processed("evt-1") {
		t.Fatalf("cancel result=%+v err=%v processed=%v", got, err, inbox.Processed("evt-1"))
	}

	if err := inbox.Receive(t.Context(), msg); err != nil {
		t.Fatal(err)
	}
	claim, ok, err := inbox.Claim(t.Context(), Inbound, "w1", now, time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim=%+v ok=%v err=%v", claim, ok, err)
	}
	wrong := claim
	wrong.Fence = claim.Fence + 1
	if err := inbox.ProcessInbox(t.Context(), wrong, func(Session) error { return nil }); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("stale fence err=%v", err)
	}
	if Classify(billing.ErrConflict) != RetryNever {
		t.Fatal("fence conflict must not retry as a fresh send")
	}

	inbox = newMemoryInbox()
	svc = NewOps(NewMemoryOpsRepository("acct"), func() time.Time { return now }).WithInbox(inbox)
	if err := inbox.Receive(t.Context(), msg); err != nil {
		t.Fatal(err)
	}
	if Classify(ErrBackpressure) != RetryLater {
		t.Fatal("backpressure must be retry-later")
	}
	result, err := svc.ProcessDue(t.Context(), ProcessDueInput{Worker: "w1", Limit: 1, Lease: time.Minute, MaxAttempts: 3, RetryAfter: time.Minute, Backpressure: time.Minute, Handler: func(Session) error {
		return ErrBackpressure
	}})
	if err != nil || result.Failed != 1 || result.Processed != 0 || result.RetryClass != RetryLater {
		t.Fatalf("backpressure result=%+v err=%v", result, err)
	}
	if inbox.Processed("evt-1") {
		t.Fatal("backpressure marked the claim processed")
	}
	okResult, err := svc.ProcessDue(t.Context(), ProcessDueInput{Worker: "w1", Limit: 1, Lease: time.Minute, MaxAttempts: 3, Handler: func(Session) error { return nil }})
	if err != nil || okResult.Processed != 1 || !inbox.Processed("evt-1") {
		t.Fatalf("retry after backpressure=%+v err=%v", okResult, err)
	}
}

func TestReconcileDryRunAndIdempotentRepair(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	credits := credit.New(credit.NewMemoryRepository("acct"), func() time.Time { return now })
	if _, err := credits.Grant(t.Context(), credit.GrantInput{Account: "acct", Operation: "g1", LotID: "lot", Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 4, Source: "purchase", SourceRef: "pay", ValidFrom: now}); err != nil {
		t.Fatal(err)
	}
	svc := NewOps(NewMemoryOpsRepository("acct"), func() time.Time { return now }).WithCredits(credits)
	dry, err := svc.Reconcile(t.Context(), ReconcileInput{Account: "acct", DryRun: true})
	if err != nil || !dry.DryRun || len(dry.Drifts) != 0 || dry.Audit.Lots != 1 {
		t.Fatalf("dry-run=%+v err=%v", dry, err)
	}
	repair, err := svc.RecordRepair(t.Context(), ReconcileInput{Account: "acct", RepairID: "repair-1", ExpectedRevision: 1, Actor: "ops", Reason: "verified", Evidence: "journal-match"})
	if err != nil || repair.Revision != 1 {
		t.Fatalf("repair=%+v err=%v", repair, err)
	}
	replay, err := svc.RecordRepair(t.Context(), ReconcileInput{Account: "acct", RepairID: "repair-1", ExpectedRevision: 1, Actor: "ops", Reason: "verified", Evidence: "journal-match"})
	if err != nil || replay.CreatedAt != repair.CreatedAt {
		t.Fatalf("repair replay=%+v err=%v", replay, err)
	}
	if _, err := svc.RecordRepair(t.Context(), ReconcileInput{Account: "acct", RepairID: "repair-1", ExpectedRevision: 1, Actor: "ops", Reason: "changed", Evidence: "journal-match"}); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("changed repair err=%v", err)
	}
	if _, err := svc.RecordRepair(t.Context(), ReconcileInput{Account: "acct", RepairID: "repair-2", ExpectedRevision: 0, Actor: "ops", Reason: "verified", Evidence: "journal-match"}); !errors.Is(err, billing.ErrInvalid) {
		t.Fatalf("missing revision err=%v", err)
	}
}

func TestRestartableBackfillHasNoFinancialSideEffects(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	credits := credit.New(credit.NewMemoryRepository("acct"), func() time.Time { return now })
	if _, err := credits.Grant(t.Context(), credit.GrantInput{Account: "acct", Operation: "g1", LotID: "lot", Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 4, Source: "purchase", SourceRef: "pay", ValidFrom: now}); err != nil {
		t.Fatal(err)
	}
	svc := NewOps(NewMemoryOpsRepository("acct"), func() time.Time { return now }).WithCredits(credits)
	first, err := svc.AdvanceBackfill(t.Context(), BackfillInput{Account: "acct", ID: "bf-1", Limit: 100})
	if err != nil || first.Processed != 100 || first.State != BackfillRunning {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	second, err := svc.AdvanceBackfill(t.Context(), BackfillInput{Account: "acct", ID: "bf-1", Limit: 50})
	if err != nil || second.Processed != 150 || second.Cursor != 150 {
		t.Fatalf("resume=%+v err=%v", second, err)
	}
	done, err := svc.FinishBackfill(t.Context(), "acct", "bf-1")
	if err != nil || done.State != BackfillDone {
		t.Fatalf("done=%+v err=%v", done, err)
	}
	again, err := svc.AdvanceBackfill(t.Context(), BackfillInput{Account: "acct", ID: "bf-1", Limit: 10})
	if err != nil || again.State != BackfillDone || again.Processed != 150 {
		t.Fatalf("terminal resume mutated job=%+v err=%v", again, err)
	}
	bal, err := credits.Balance(t.Context(), "acct", "credits", "")
	if err != nil || bal.Available != 4 {
		t.Fatalf("backfill mutated credits %+v err=%v", bal, err)
	}
}

func TestArchiveRestoreRetainsTombstonesAndReplay(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	rule, err := usage.NewRule(usage.RuleConfig{Version: "ops-rule", Kind: usage.KindWeighted, Target: usage.Target{Currency: "USD"}, Rounding: usage.RoundDown, Weights: map[string]string{"tokens": "1"}})
	if err != nil {
		t.Fatal(err)
	}
	usageSvc := usage.New(usage.NewMemoryRepository(usage.MemoryConfig{Accounts: []billing.AccountID{"acct"}, Rules: func(_ context.Context, version string) (*usage.Rule, error) {
		if version != rule.Version() {
			return nil, billing.ErrNotFound
		}
		return rule, nil
	}}), func() time.Time { return now })
	credits := credit.New(credit.NewMemoryRepository("acct"), func() time.Time { return now })
	if _, err := credits.Grant(t.Context(), credit.GrantInput{Account: "acct", Operation: "g1", LotID: "lot", Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 4, Source: "purchase", SourceRef: "pay", ValidFrom: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := usageSvc.RateAndRecord(t.Context(), usage.Observation{Account: "acct", ID: "u1", Source: "meter", OccurredAt: now, Funding: usage.Postpaid, Input: usage.RateInput{Metrics: []usage.MetricQuantity{{Name: "tokens", Quantity: 2}}}}, rule.Version()); err != nil {
		t.Fatal(err)
	}
	svc := NewOps(NewMemoryOpsRepository("acct"), func() time.Time { return now }).WithUsage(usageSvc).WithCredits(credits)
	period := billing.Period{Start: now.Add(-time.Hour), End: now.Add(time.Hour)}
	snap, err := svc.Archive(t.Context(), "acct", period)
	if err != nil || len(snap.Tombstones) != 1 || snap.Tombstones[0].ID != "u1" {
		t.Fatalf("archive=%+v err=%v", snap, err)
	}
	if err := svc.Restore(t.Context(), snap); err != nil {
		t.Fatal(err)
	}
	if err := svc.Restore(t.Context(), snap); err != nil {
		t.Fatal(err)
	}
	// Usage rows route by their own account, so a snapshot carrying another
	// tenant's rows must be refused rather than written into that tenant.
	foreign := snap
	foreign.Usage = slices.Clone(snap.Usage)
	if len(foreign.Usage) == 0 {
		t.Fatal("archive captured no usage to scope-check")
	}
	foreign.Usage[0].Observation.Account = "other-acct"
	if err := svc.Restore(t.Context(), foreign); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("foreign usage restore err=%v, want conflict", err)
	}
	replay, err := usageSvc.RateAndRecord(t.Context(), usage.Observation{Account: "acct", ID: "u1", Source: "meter", OccurredAt: now, Funding: usage.Postpaid, Input: usage.RateInput{Metrics: []usage.MetricQuantity{{Name: "tokens", Quantity: 2}}}}, rule.Version())
	if err != nil || replay.Observation.ID != "u1" {
		t.Fatalf("usage replay=%+v err=%v", replay, err)
	}
	changed := usage.Observation{Account: "acct", ID: "u1", Source: "meter", OccurredAt: now, Funding: usage.Postpaid, Input: usage.RateInput{Metrics: []usage.MetricQuantity{{Name: "tokens", Quantity: 9}}}}
	if _, err := usageSvc.RateAndRecord(t.Context(), changed, rule.Version()); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("redelivered changed usage err=%v", err)
	}
	grant, err := credits.Grant(t.Context(), credit.GrantInput{Account: "acct", Operation: "g1", LotID: "lot", Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 4, Source: "purchase", SourceRef: "pay", ValidFrom: now})
	if err != nil {
		t.Fatal(err)
	}
	bal, err := credits.Balance(t.Context(), "acct", "credits", "")
	if err != nil || bal.Available != 4 || grant.Balance.Available != 4 {
		t.Fatalf("redelivered grant doubled credits %+v %+v", grant, bal)
	}
}

func TestHooksRedactSecretsAndReportLagUnknownDriftRecovery(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	if Redact("Bearer super-secret-token") != "[redacted]" || Redact("api-key=abc") != "[redacted]" || Redact("queue") != "queue" {
		t.Fatalf("redact=%q %q %q", Redact("Bearer super-secret-token"), Redact("api-key=abc"), Redact("queue"))
	}
	var events []Event
	inbox := newMemoryInbox()
	credits := credit.New(credit.NewMemoryRepository("acct"), func() time.Time { return now })
	if _, err := credits.Grant(t.Context(), credit.GrantInput{Account: "acct", Operation: "g1", LotID: "lot", Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 1, Source: "purchase", SourceRef: "pay", ValidFrom: now}); err != nil {
		t.Fatal(err)
	}
	svc := NewOps(NewMemoryOpsRepository("acct"), func() time.Time { return now }).WithInbox(inbox).WithCredits(credits).WithObserver(func(ev Event) { events = append(events, ev) })
	if err := inbox.Receive(t.Context(), Message{Account: "acct", ID: "evt", Scope: billing.Scope{Provider: "p", Merchant: "m", Environment: "test"}, Kind: "event", Direction: Inbound, OccurredAt: now, Payload: []byte("password=hidden")}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ProcessDue(t.Context(), ProcessDueInput{Worker: "w1", Limit: 1, Lease: time.Minute, MaxAttempts: 1, Handler: func(Session) error { return nil }}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Reconcile(t.Context(), ReconcileInput{Account: "acct", DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RecordRepair(t.Context(), ReconcileInput{Account: "acct", RepairID: "r1", ExpectedRevision: 1, Actor: "ops", Reason: "ok", Evidence: "audit"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AdvanceBackfill(t.Context(), BackfillInput{Account: "acct", ID: "bf", Limit: 1}); err != nil {
		t.Fatal(err)
	}
	kinds := map[EventKind]int{}
	for _, ev := range events {
		kinds[ev.Kind]++
		for _, v := range []string{ev.Worker, ev.Reason, ev.RepairID, ev.Actor, ev.JobID} {
			if v != Redact(v) {
				t.Fatalf("unredacted event field %q", v)
			}
		}
	}
	if kinds[EventQueueLag] == 0 || kinds[EventRecovery] == 0 || kinds[EventBackfill] == 0 {
		t.Fatalf("events=%v", kinds)
	}
}
