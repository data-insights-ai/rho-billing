package pg

import (
	"errors"
	"fmt"
	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/billingtest"
	"github.com/data-insights-ai/rho-billing/usage"
	"testing"
	"time"
)

func TestRepeatedUnknownLookupNeverBecomesRejection(t *testing.T) {
	for _, backend := range []string{"memory", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := t.Context()
			now := testTime()
			period := usage.BillingPeriod{Start: now.Add(-time.Hour), End: now, Cutoff: now}
			config := usage.RuleConfig{Version: "uncertain-rate", Kind: usage.KindFixed, Target: usage.Target{Currency: "USD"}, Rounding: usage.RoundDown, FixedRate: "1"}
			rule, err := usage.NewRule(config)
			if err != nil {
				t.Fatal(err)
			}
			record, err := usage.Prepare(usage.Observation{Account: "acct-a", ID: "usage", Source: "source", OccurredAt: period.Start, Funding: usage.Postpaid, Input: usage.RateInput{ActionCount: 4}}, rule, now)
			if err != nil {
				t.Fatal(err)
			}
			var repo usage.SettlementRepository
			if backend == "memory" {
				repo = usage.NewMemorySettlementRepository(usage.ReferenceAccount{ID: "acct-a", Usage: []usage.Record{record}})
			} else {
				store, _ := testStore(t)
				createTestAccounts(t, store)
				if err := store.PublishRating(ctx, config); err != nil {
					t.Fatal(err)
				}
				if _, err := recordUsage(ctx, store, record); err != nil {
					t.Fatal(err)
				}
				repo = store.Settlements()
			}
			svc := usage.NewSettlement(repo, testTime)
			batch, err := finishSettlementClose(ctx, repo, testTime, usage.CloseInput{Account: "acct-a", Operation: "finalize", BatchID: "batch", Period: period, Currency: "USD", CreatedAt: now})
			if err != nil {
				t.Fatal(err)
			}
			attempt, err := svc.BeginSubmission(ctx, usage.BeginSubmissionInput{Account: "acct-a", Operation: "begin", BatchID: batch.ID, AttemptID: "attempt", Provider: "sim", IdempotencyKey: "charge-once", Capability: usage.SettlementCapability{SupportsLookup: true, SupportsIdempotency: true}, CreatedAt: now})
			if err != nil {
				t.Fatal(err)
			}
			sim := billingtest.NewSimulator(billingtest.Pending)
			response, err := sim.Submit(ctx, billingtest.SubmitRequest{BatchID: batch.ID, AttemptID: attempt.ID, IdempotencyKey: attempt.IdempotencyKey, Currency: batch.Currency, Amount: batch.Total})
			if err != nil || response.Status != usage.SubmissionUnknown {
				t.Fatalf("pending response %+v %v", response, err)
			}
			current, err := svc.CompleteSubmission(ctx, usage.CompleteSubmissionInput{Account: "acct-a", Operation: "initial-unknown", BatchID: batch.ID, AttemptID: attempt.ID, ExpectedRevision: attempt.Revision, Status: response.Status, Reason: response.Reason, CompletedAt: now})
			if err != nil {
				t.Fatal(err)
			}
			for i := range 2 {
				lookup, err := sim.Lookup(ctx, attempt.IdempotencyKey)
				if err != nil {
					t.Fatal(err)
				}
				current, err = svc.ReconcileUnknown(ctx, usage.ReconcileUnknownInput{Account: "acct-a", Operation: billing.OperationID(fmt.Sprintf("lookup-%d", i)), BatchID: batch.ID, AttemptID: attempt.ID, ExpectedRevision: current.Revision, Status: lookup.Status, Evidence: "provider still processing", ReconciledAt: now})
				if err != nil || current.State != usage.BatchUnknown {
					t.Fatalf("uncertainty became terminal %+v %v", current, err)
				}
			}
			if err := sim.Resolve(attempt.IdempotencyKey, usage.ProviderResult{Status: usage.SubmissionConfirmed, ProviderReference: "charge-later"}); err != nil {
				t.Fatal(err)
			}
			lookup, err := sim.Lookup(ctx, attempt.IdempotencyKey)
			if err != nil {
				t.Fatal(err)
			}
			confirmed, err := svc.ReconcileUnknown(ctx, usage.ReconcileUnknownInput{Account: "acct-a", Operation: "resolve", BatchID: batch.ID, AttemptID: attempt.ID, ExpectedRevision: current.Revision, Status: lookup.Status, ProviderReference: lookup.ProviderReference, Evidence: "authoritative completion", ReconciledAt: now})
			if err != nil || confirmed.State != usage.BatchConfirmed {
				t.Fatalf("resolution %+v %v", confirmed, err)
			}
			if _, err := svc.ReconcileUnknown(ctx, usage.ReconcileUnknownInput{Account: "acct-a", Operation: "old-late-result", BatchID: batch.ID, AttemptID: attempt.ID, ExpectedRevision: current.Revision, Status: usage.SubmissionRejected, Evidence: "outdated response", ReconciledAt: now}); !errors.Is(err, usage.ErrStaleRevision) {
				t.Fatalf("reordered callback overwrote confirmation %v", err)
			}
			// The settlement service never performs the provider call itself,
			// so counting the simulator's submits could not fail: the test made
			// the only call. What actually prevents a double charge is that no
			// second submission permission is granted for a batch that already
			// has one, whatever state it reached.
			if _, err := svc.BeginSubmission(ctx, usage.BeginSubmissionInput{Account: "acct-a", Operation: "begin-again", BatchID: batch.ID, AttemptID: "attempt-2", Provider: "sim", IdempotencyKey: "charge-twice", Capability: usage.SettlementCapability{SupportsLookup: true, SupportsIdempotency: true}, CreatedAt: now}); err == nil {
				t.Fatal("a second submission permission was granted for a settled batch")
			}
		})
	}
}
