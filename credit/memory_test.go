package credit

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
)

func TestMemoryConcurrentUnitScaleMismatchPersistsRejection(t *testing.T) {
	repo := NewMemoryRepository("acct-a", "acct-b")
	now := time.Date(2026, time.September, 15, 12, 0, 0, 0, time.UTC)
	inputs := []GrantInput{
		{Account: "acct-a", Operation: "unit-race-a", LotID: "unit-race-lot-a", Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 1, Source: "test", SourceRef: "unit-race-a", ValidFrom: now},
		{Account: "acct-b", Operation: "unit-race-b", LotID: "unit-race-lot-b", Unit: billing.Unit{Code: "credits", Scale: 2}, Amount: 1, Source: "test", SourceRef: "unit-race-b", ValidFrom: now},
	}
	start := make(chan struct{})
	type outcome struct {
		input GrantInput
		err   error
	}
	results := make(chan outcome, len(inputs))
	var wg sync.WaitGroup
	for _, input := range inputs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := New(repo, func() time.Time { return now }).Grant(context.Background(), input)
			results <- outcome{input: input, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	var loser outcome
	var winners int
	for result := range results {
		if result.err == nil {
			winners++
			continue
		}
		if !errors.Is(result.err, billing.ErrConflict) || !IsRejection(result.err) {
			t.Fatalf("concurrent scale result for %s: %v", result.input.Account, result.err)
		}
		loser = result
	}
	if winners != 1 || loser.input.Operation == "" {
		t.Fatalf("expected one winner and one persisted rejection, winners=%d loser=%+v", winners, loser)
	}
	if _, err := New(repo, func() time.Time { return now }).Grant(context.Background(), loser.input); !errors.Is(err, billing.ErrConflict) || !IsRejection(err) {
		t.Fatalf("replayed concurrent scale rejection: %v", err)
	}
}
