package purchase

import (
	"errors"
	"sync"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
)

func TestAutoTopUpConsentCooldownCapAndOneActiveAttempt(t *testing.T) {
	now := billing.CanonicalTime(time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC))
	clock := now
	svc := New(NewMemoryRepository(ReferenceAccount{Account: "acct"}), func() time.Time { return clock })
	policy := TopUpPolicy{Account: "acct", ID: "auto", ConsentRevision: 1, ConsentedAt: now, ConsentActor: "owner", Threshold: 5, Cooldown: time.Hour, PurchaseCap: 200, Currency: "USD", Amount: 100, Unit: billing.Unit{Code: "credits", Scale: 1}, Enabled: true}
	if _, err := svc.ConfigureTopUp(t.Context(), policy); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.EvaluateTopUp(t.Context(), "acct", "auto", 10); !errors.Is(err, billing.ErrState) {
		t.Fatalf("above threshold err=%v, want state", err)
	}
	first, err := svc.EvaluateTopUp(t.Context(), "acct", "auto", 5)
	if err != nil || first.State != TopUpPlanned || first.Amount != 100 {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	if _, err := svc.EvaluateTopUp(t.Context(), "acct", "auto", 1); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("second active err=%v, want conflict", err)
	}

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	race := New(NewMemoryRepository(ReferenceAccount{Account: "race"}), func() time.Time { return now })
	if _, err := race.ConfigureTopUp(t.Context(), TopUpPolicy{Account: "race", ID: "auto", ConsentRevision: 1, ConsentedAt: now, ConsentActor: "owner", Threshold: 5, Cooldown: time.Hour, PurchaseCap: 200, Currency: "USD", Amount: 100, Unit: billing.Unit{Code: "credits", Scale: 1}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		wg.Go(func() {
			<-start
			_, err := race.EvaluateTopUp(t.Context(), "race", "auto", 0)
			errs <- err
		})
	}
	close(start)
	wg.Wait()
	close(errs)
	var success, conflict int
	for err := range errs {
		if err == nil {
			success++
		} else if errors.Is(err, billing.ErrConflict) {
			conflict++
		} else {
			t.Fatalf("race err=%v", err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("concurrent evaluate success=%d conflict=%d", success, conflict)
	}
}

func TestUnknownTopUpBlocksPurchaseAndGrantIsOnceAfterPaid(t *testing.T) {
	s, now, scope, quote, intent := lifecycleFixture(t)
	policy := TopUpPolicy{Account: "acct", ID: "auto", ConsentRevision: 1, ConsentedAt: now, ConsentActor: "owner", Threshold: 5, Cooldown: time.Hour, PurchaseCap: 200, Currency: "USD", Amount: quote.Amount, Unit: billing.Unit{Code: "credits", Scale: 1}, Enabled: true}
	if _, err := s.ConfigureTopUp(t.Context(), policy); err != nil {
		t.Fatal(err)
	}
	attempt, err := s.EvaluateTopUp(t.Context(), "acct", "auto", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.GrantTopUp(t.Context(), "acct", attempt.ID); !errors.Is(err, billing.ErrState) {
		t.Fatalf("grant before paid err=%v", err)
	}
	dispatched, err := s.DispatchTopUp(t.Context(), "acct", attempt.ID, intent.ID)
	if err != nil || dispatched.State != TopUpDispatched {
		t.Fatalf("dispatch=%+v err=%v", dispatched, err)
	}
	unknown, err := s.ObserveTopUp(t.Context(), "acct", attempt.ID, TopUpUnknown)
	if err != nil || unknown.State != TopUpUnknown {
		t.Fatalf("unknown=%+v err=%v", unknown, err)
	}
	if _, err := s.EvaluateTopUp(t.Context(), "acct", "auto", 0); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("evaluate during unknown err=%v, want conflict", err)
	}
	if _, err := s.GrantTopUp(t.Context(), "acct", attempt.ID); !errors.Is(err, billing.ErrState) {
		t.Fatalf("grant while unknown err=%v", err)
	}
	if _, err := s.ObserveTopUp(t.Context(), "acct", attempt.ID, TopUpRejected); err != nil {
		t.Fatal(err)
	}
	next, err := s.EvaluateTopUp(t.Context(), "acct", "auto", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DispatchTopUp(t.Context(), "acct", next.ID, intent.ID); err != nil {
		t.Fatal(err)
	}
	fact := PaymentFact{Account: "acct", Scope: scope, EventID: "topup-paid", TransactionID: "topup-tx", IntentID: intent.ID, Status: FactPaid, Currency: quote.Currency, Gross: quote.Amount, Lines: []PaidLine{{LineID: "line", Gross: quote.Amount}}, OccurredAt: now.Add(time.Minute), CollectedAt: now.Add(time.Minute)}
	if _, err := s.ApplyPayment(t.Context(), fact); err != nil {
		t.Fatal(err)
	}
	granted, err := s.GrantTopUp(t.Context(), "acct", next.ID)
	if err != nil || granted.State != TopUpGranted {
		t.Fatalf("granted=%+v err=%v", granted, err)
	}
	replay, err := s.GrantTopUp(t.Context(), "acct", next.ID)
	if err != nil || replay.State != TopUpGranted || replay.UpdatedAt != granted.UpdatedAt {
		t.Fatalf("grant replay=%+v err=%v", replay, err)
	}
}
