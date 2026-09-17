package billingtest

import (
	"context"
	"errors"
	"testing"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/usage"
)

func TestAcceptedThenTimeoutIsRecoverableByAuthoritativeLookup(t *testing.T) {
	simulator := NewSimulator(AcceptThenTimeout)
	request := SubmitRequest{BatchID: "batch", AttemptID: "attempt", IdempotencyKey: "idem", Currency: "USD", Amount: 42}
	response, err := simulator.Submit(context.Background(), request)
	if !errors.Is(err, ErrAcceptedThenTimeout) || response.Status != usage.SubmissionUnknown || response.ProviderReference != "" {
		t.Fatalf("accepted timeout response = %+v / %v", response, err)
	}
	lookup, err := simulator.Lookup(context.Background(), request.IdempotencyKey)
	if err != nil || lookup.Status != usage.SubmissionConfirmed || lookup.ProviderReference == "" {
		t.Fatalf("authoritative lookup = %+v / %v", lookup, err)
	}
	retry, err := simulator.Submit(context.Background(), request)
	if err != nil || retry.ProviderReference != lookup.ProviderReference {
		t.Fatalf("idempotent retry = %+v / %v", retry, err)
	}
	if simulator.SubmitCount() != 2 || simulator.LookupCount() != 1 {
		t.Fatalf("simulator call counts: submits=%d lookups=%d", simulator.SubmitCount(), simulator.LookupCount())
	}
}

func TestRejectedAndMissingLookupAreAuthoritative(t *testing.T) {
	simulator := NewSimulator(Reject)
	response, err := simulator.Submit(context.Background(), SubmitRequest{BatchID: "batch", AttemptID: "attempt", IdempotencyKey: "idem", Currency: "EUR", Amount: 7})
	if err != nil || response.Status != usage.SubmissionRejected || response.Reason == "" {
		t.Fatalf("rejection response = %+v / %v", response, err)
	}
	missing, err := simulator.Lookup(context.Background(), "missing")
	if err != nil || missing.Status != usage.SubmissionRejected || missing.Reason == "" {
		t.Fatalf("missing lookup = %+v / %v", missing, err)
	}
	_, err = simulator.Submit(context.Background(), SubmitRequest{BatchID: "batch", AttemptID: "attempt", IdempotencyKey: "idem", Currency: "EUR", Amount: 8})
	if !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("changed idempotency payload error = %v", err)
	}
}
