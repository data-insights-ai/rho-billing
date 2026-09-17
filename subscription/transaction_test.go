package subscription

import (
	"context"
	"errors"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
)

type failAfterObservation struct {
	Repository
	failure error
}

func (r failAfterObservation) WithinAccount(ctx context.Context, account billing.AccountID, fn func(Tx) error) error {
	return r.Repository.WithinAccount(ctx, account, func(tx Tx) error {
		if err := fn(tx); err != nil {
			return err
		}
		return r.failure
	})
}

func TestObservationTransactionFailureReturnsNoProvisionalSnapshot(t *testing.T) {
	repo := NewMemoryRepository()
	failure := errors.New("commit failed")
	in := transactionObservation()
	result, err := New(failAfterObservation{Repository: repo, failure: failure}).Observe(t.Context(), in)
	if !errors.Is(err, failure) || result.Account != "" || result.Revision != 0 {
		t.Fatalf("failed observation result=%+v error=%v", result, err)
	}
	if _, err := New(repo).Subscription(t.Context(), in.Snapshot.Account, in.Snapshot.Ref); !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("failed transaction published state: %v", err)
	}
	retried, err := New(repo).Observe(t.Context(), in)
	if err != nil || retried.Revision != 1 {
		t.Fatalf("retry=%+v error=%v", retried, err)
	}
}

func TestReferenceSubscriptionTransactionCancellationAndPanicRollback(t *testing.T) {
	for _, mode := range []string{"cancellation", "panic"} {
		t.Run(mode, func(t *testing.T) {
			repo := NewMemoryRepository()
			in := transactionObservation()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var recovered any
			var err error
			func() {
				defer func() { recovered = recover() }()
				err = repo.WithinAccount(ctx, in.Snapshot.Account, func(tx Tx) error {
					if err := tx.SaveSnapshot(ctx, in.Snapshot); err != nil {
						return err
					}
					if mode == "panic" {
						panic("abort callback")
					}
					cancel()
					return nil
				})
			}()
			if mode == "panic" && recovered != "abort callback" {
				t.Fatalf("panic=%v", recovered)
			}
			if mode == "cancellation" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation=%v", err)
			}
			if _, err := New(repo).Subscription(t.Context(), in.Snapshot.Account, in.Snapshot.Ref); !errors.Is(err, billing.ErrNotFound) {
				t.Fatalf("aborted transaction published state: %v", err)
			}
		})
	}
}

func transactionObservation() Observation {
	return Observation{
		Snapshot: Snapshot{Account: "account", Ref: billing.Reference{Scope: billing.Scope{Provider: "test", Merchant: "merchant", Environment: "sandbox"}, ID: "subscription"}, Status: "active"},
		EventID:  "event", OccurredAt: time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC),
	}
}
