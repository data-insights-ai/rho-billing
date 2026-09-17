package pg

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/integration"
)

func claimedOutbox(t *testing.T, account billing.AccountID, id string, lease time.Duration) (*Store, integration.Message, integration.Claim) {
	t.Helper()
	store, _ := queueStore(t)
	if err := store.CreateAccount(t.Context(), account, string(account)); err != nil {
		t.Fatal(err)
	}
	message := queueMessage(account, id, integration.Outbound, "frozen-command")
	if err := store.Atomic(t.Context(), account, func(session integration.Session) error {
		return session.Enqueue(t.Context(), message)
	}); err != nil {
		t.Fatal(err)
	}
	claim, ok, err := store.Claim(t.Context(), integration.Outbound, "outbox-worker", time.Now().UTC(), lease)
	if err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	return store, message, claim
}

func TestBeginOutboxGrantsOnePermitAcrossPools(t *testing.T) {
	store, message, claim := claimedOutbox(t, "outbox-begin-race", "begin-race", time.Minute)
	second := New(secondQueueDB(t, store.db))
	var callbacks atomic.Int64
	type beginResult struct {
		permit bool
		err    error
	}
	results := make(chan beginResult, 2)
	var wg sync.WaitGroup
	for _, repository := range []*Store{store, second} {
		wg.Go(func() {
			permit, err := repository.BeginOutbox(t.Context(), claim, func(integration.Session) error {
				callbacks.Add(1)
				return nil
			})
			results <- beginResult{permit, err}
		})
	}
	wg.Wait()
	close(results)
	permits := 0
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.permit {
			permits++
		}
	}
	if permits != 1 || callbacks.Load() != 1 {
		t.Fatalf("permits=%d callbacks=%d", permits, callbacks.Load())
	}
	permit, err := store.BeginOutbox(t.Context(), claim, func(integration.Session) error {
		callbacks.Add(1)
		return nil
	})
	if err != nil || permit || callbacks.Load() != 1 {
		t.Fatalf("repeat permit=%v callbacks=%d err=%v", permit, callbacks.Load(), err)
	}
	delivery, err := store.Delivery(t.Context(), message)
	if err != nil || delivery.State != "processing" {
		t.Fatalf("delivery=%+v err=%v", delivery, err)
	}
}

func TestBeginOutboxRollbackAndCommitFailureReturnNoPermit(t *testing.T) {
	t.Run("callback", func(t *testing.T) {
		store, _, claim := claimedOutbox(t, "outbox-begin-callback", "begin-callback", time.Minute)
		sentinel := errors.New("callback failed")
		permit, err := store.BeginOutbox(t.Context(), claim, func(integration.Session) error { return sentinel })
		if permit || !errors.Is(err, sentinel) {
			t.Fatalf("permit=%v err=%v", permit, err)
		}
		permit, err = store.BeginOutbox(t.Context(), claim, func(integration.Session) error { return nil })
		if err != nil || !permit {
			t.Fatalf("retry permit=%v err=%v", permit, err)
		}
	})

	t.Run("commit", func(t *testing.T) {
		store, _, claim := claimedOutbox(t, "outbox-begin-commit", "begin-commit", time.Minute)
		if _, err := store.db.ExecContext(t.Context(), `
			CREATE FUNCTION fail_outbox_begin_commit() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN RAISE EXCEPTION 'outbox begin commit failure'; END $$;
			CREATE CONSTRAINT TRIGGER fail_outbox_begin_commit
			AFTER UPDATE OF begun_at ON billing_outbox DEFERRABLE INITIALLY DEFERRED
			FOR EACH ROW WHEN (OLD.begun_at IS NULL AND NEW.begun_at IS NOT NULL)
			EXECUTE FUNCTION fail_outbox_begin_commit()`); err != nil {
			t.Fatal(err)
		}
		permit, err := store.BeginOutbox(t.Context(), claim, func(integration.Session) error { return nil })
		if err == nil || permit {
			t.Fatalf("permit=%v err=%v", permit, err)
		}
		if _, err := store.db.ExecContext(t.Context(), `DROP TRIGGER fail_outbox_begin_commit ON billing_outbox`); err != nil {
			t.Fatal(err)
		}
		permit, err = store.BeginOutbox(t.Context(), claim, func(integration.Session) error { return nil })
		if err != nil || !permit {
			t.Fatalf("retry permit=%v err=%v", permit, err)
		}
	})
}

func TestFinishOutboxAtomicEffectsReplayAndClaimFences(t *testing.T) {
	store, message, claim := claimedOutbox(t, "outbox-finish", "finish", time.Minute)
	if permit, err := store.BeginOutbox(t.Context(), claim, func(integration.Session) error { return nil }); err != nil || !permit {
		t.Fatalf("begin permit=%v err=%v", permit, err)
	}
	if _, err := store.db.ExecContext(t.Context(), `CREATE TABLE outbox_test_effects(id text PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	result := integration.OutboxResult{ObservationID: "finish-observation", State: integration.OutboxCompleted, ProviderReference: "provider-result", Evidence: "provider accepted"}
	callbacks := 0
	callback := func(scope integration.Session) error {
		callbacks++
		_, err := scope.(*session).tx.ExecContext(t.Context(), `INSERT INTO outbox_test_effects(id) VALUES('finished')`)
		return err
	}
	if err := store.FinishOutbox(t.Context(), claim, result, callback); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishOutbox(t.Context(), claim, result, callback); err != nil || callbacks != 1 {
		t.Fatalf("replay callbacks=%d err=%v", callbacks, err)
	}
	changed := result
	changed.Evidence = "changed"
	if err := store.FinishOutbox(t.Context(), claim, changed, func(integration.Session) error { return nil }); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("changed replay err=%v", err)
	}
	future := claim
	future.Fence++
	if err := store.FinishOutbox(t.Context(), future, result, func(integration.Session) error { return nil }); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("future fence err=%v", err)
	}
	foreign := claim
	foreign.Message.Payload = []byte("changed-command")
	if err := store.FinishOutbox(t.Context(), foreign, result, func(integration.Session) error { return nil }); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("changed message err=%v", err)
	}
	wrongScope := claim
	wrongScope.Message.Scope.Merchant = "other-merchant"
	if err := store.FinishOutbox(t.Context(), wrongScope, result, func(integration.Session) error { return nil }); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("changed provider scope err=%v", err)
	}
	var effects, observations int
	if err := store.db.QueryRowContext(t.Context(), `SELECT (SELECT count(*) FROM outbox_test_effects),(SELECT count(*) FROM billing_outbox_results WHERE account_id=$1 AND message_id=$2)`, message.Account, message.ID).Scan(&effects, &observations); err != nil {
		t.Fatal(err)
	}
	if effects != 1 || observations != 1 {
		t.Fatalf("effects=%d observations=%d", effects, observations)
	}
}

func TestFinishOutboxCallbackFailureAndExpiryRollback(t *testing.T) {
	t.Run("callback", func(t *testing.T) {
		store, message, claim := claimedOutbox(t, "outbox-finish-callback", "finish-callback", time.Minute)
		if permit, err := store.BeginOutbox(t.Context(), claim, func(integration.Session) error { return nil }); err != nil || !permit {
			t.Fatal(err)
		}
		sentinel := errors.New("domain effect failed")
		result := integration.OutboxResult{ObservationID: "failed-finish", State: integration.OutboxCompleted, ProviderReference: "provider-result", Evidence: "provider accepted"}
		if err := store.FinishOutbox(t.Context(), claim, result, func(integration.Session) error { return sentinel }); !errors.Is(err, sentinel) {
			t.Fatalf("finish err=%v", err)
		}
		var observations int
		if err := store.db.QueryRowContext(t.Context(), `SELECT count(*) FROM billing_outbox_results WHERE account_id=$1 AND message_id=$2`, message.Account, message.ID).Scan(&observations); err != nil || observations != 0 {
			t.Fatalf("observations=%d err=%v", observations, err)
		}
	})

	t.Run("commit", func(t *testing.T) {
		store, message, claim := claimedOutbox(t, "outbox-finish-commit", "finish-commit", time.Minute)
		if permit, err := store.BeginOutbox(t.Context(), claim, func(integration.Session) error { return nil }); err != nil || !permit {
			t.Fatal(err)
		}
		if _, err := store.db.ExecContext(t.Context(), `
			CREATE TABLE outbox_commit_effects(id text PRIMARY KEY);
			CREATE FUNCTION fail_outbox_finish_commit() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN RAISE EXCEPTION 'outbox finish commit failure'; END $$;
			CREATE CONSTRAINT TRIGGER fail_outbox_finish_commit
			AFTER INSERT ON billing_outbox_results DEFERRABLE INITIALLY DEFERRED
			FOR EACH ROW EXECUTE FUNCTION fail_outbox_finish_commit()`); err != nil {
			t.Fatal(err)
		}
		result := integration.OutboxResult{ObservationID: "commit-finish", State: integration.OutboxCompleted, ProviderReference: "provider-result", Evidence: "provider accepted"}
		err := store.FinishOutbox(t.Context(), claim, result, func(scope integration.Session) error {
			_, err := scope.(*session).tx.ExecContext(t.Context(), `INSERT INTO outbox_commit_effects(id) VALUES('effect')`)
			return err
		})
		if err == nil {
			t.Fatal("deferred commit failure succeeded")
		}
		var effects, observations int
		if err := store.db.QueryRowContext(t.Context(), `SELECT (SELECT count(*) FROM outbox_commit_effects),(SELECT count(*) FROM billing_outbox_results WHERE account_id=$1 AND message_id=$2)`, message.Account, message.ID).Scan(&effects, &observations); err != nil {
			t.Fatal(err)
		}
		if effects != 0 || observations != 0 {
			t.Fatalf("effects=%d observations=%d", effects, observations)
		}
	})

	t.Run("expired", func(t *testing.T) {
		store, message, claim := claimedOutbox(t, "outbox-finish-expired", "finish-expired", time.Minute)
		if permit, err := store.BeginOutbox(t.Context(), claim, func(integration.Session) error { return nil }); err != nil || !permit {
			t.Fatal(err)
		}
		if _, err := store.db.ExecContext(t.Context(), `UPDATE billing_outbox SET lease_deadline=clock_timestamp()-interval '1 second' WHERE account_id=$1 AND message_id=$2`, message.Account, message.ID); err != nil {
			t.Fatal(err)
		}
		callbacks := 0
		result := integration.OutboxResult{ObservationID: "expired-finish", State: integration.OutboxCompleted, ProviderReference: "provider-result", Evidence: "late response"}
		if err := store.FinishOutbox(t.Context(), claim, result, func(integration.Session) error { callbacks++; return nil }); !errors.Is(err, billing.ErrConflict) {
			t.Fatalf("finish err=%v", err)
		}
		delivery, err := store.Delivery(t.Context(), message)
		if err != nil || delivery.State != "unknown" || callbacks != 0 {
			t.Fatalf("delivery=%+v callbacks=%d err=%v", delivery, callbacks, err)
		}
	})
}

func TestResolveOutboxRetainsUnresolvedObservationJournal(t *testing.T) {
	store, message, claim := claimedOutbox(t, "outbox-resolve", "resolve", time.Minute)
	if permit, err := store.BeginOutbox(t.Context(), claim, func(integration.Session) error { return nil }); err != nil || !permit {
		t.Fatal(err)
	}
	initial := integration.OutboxResult{ObservationID: "finish-unknown", State: integration.OutboxUnknown, ProviderReference: "known-provider-id", Evidence: "response lost"}
	if err := store.FinishOutbox(t.Context(), claim, initial, func(integration.Session) error { return nil }); err != nil {
		t.Fatal(err)
	}
	callbacks := 0
	different := integration.OutboxResult{ObservationID: "lookup-wrong-reference", State: integration.OutboxUnknown, ProviderReference: "different-provider-id", Evidence: "conflicting candidate", ExpectedPrevious: initial.ObservationID}
	if err := store.ResolveOutbox(t.Context(), claim, different, func(integration.Session) error { callbacks++; return nil }); !errors.Is(err, billing.ErrConflict) || callbacks != 0 {
		t.Fatalf("changed provider reference callbacks=%d err=%v", callbacks, err)
	}
	first := integration.OutboxResult{ObservationID: "lookup-one", State: integration.OutboxUnknown, Evidence: "zero candidates", ExpectedPrevious: initial.ObservationID}
	if err := store.ResolveOutbox(t.Context(), claim, first, func(integration.Session) error { callbacks++; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := store.ResolveOutbox(t.Context(), claim, first, func(integration.Session) error { callbacks++; return nil }); err != nil || callbacks != 1 {
		t.Fatalf("replay callbacks=%d err=%v", callbacks, err)
	}
	recovered, err := store.Outbox(t.Context(), message.Account, message.ProviderScope(), message.ID)
	if err != nil || recovered.ProviderReference != initial.ProviderReference || recovered.LastResult == nil || recovered.LastResult.ProviderReference != "" || recovered.LastResult.ObservationID != first.ObservationID {
		t.Fatalf("known reference was not retained: delivery=%+v err=%v", recovered, err)
	}
	changed := first
	changed.Evidence = "one candidate"
	if err := store.ResolveOutbox(t.Context(), claim, changed, func(integration.Session) error { return nil }); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("changed observation err=%v", err)
	}
	second := integration.OutboxResult{ObservationID: "lookup-two", State: integration.OutboxUnknown, Evidence: "scan incomplete", ExpectedPrevious: first.ObservationID}
	if err := store.ResolveOutbox(t.Context(), claim, second, func(integration.Session) error { callbacks++; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := store.ResolveOutbox(t.Context(), claim, first, func(integration.Session) error { callbacks++; return nil }); err != nil {
		t.Fatal(err)
	}
	var pointer string
	if err := store.db.QueryRowContext(t.Context(), `SELECT last_observation_id FROM billing_outbox WHERE account_id=$1 AND message_id=$2`, message.Account, message.ID).Scan(&pointer); err != nil || pointer != second.ObservationID {
		t.Fatalf("replay changed observation pointer=%q err=%v", pointer, err)
	}
	accepted := integration.OutboxResult{ObservationID: "lookup-final", State: integration.OutboxCompleted, ProviderReference: "known-provider-id", Evidence: "exact candidate", ExpectedPrevious: second.ObservationID}
	if err := store.ResolveOutbox(t.Context(), claim, accepted, func(integration.Session) error { callbacks++; return nil }); err != nil {
		t.Fatal(err)
	}
	var observations int
	if err := store.db.QueryRowContext(t.Context(), `SELECT count(*) FROM billing_outbox_results WHERE account_id=$1 AND message_id=$2`, message.Account, message.ID).Scan(&observations); err != nil {
		t.Fatal(err)
	}
	delivery, err := store.Delivery(t.Context(), message)
	if err != nil || observations != 4 || callbacks != 3 || delivery.State != "completed" {
		t.Fatalf("observations=%d callbacks=%d delivery=%+v err=%v", observations, callbacks, delivery, err)
	}
}

func TestResolveOutboxConcurrentSuccessorCASRollsBackLoser(t *testing.T) {
	store, message, claim := claimedOutbox(t, "outbox-resolve-race", "resolve-race", time.Minute)
	if permit, err := store.BeginOutbox(t.Context(), claim, func(integration.Session) error { return nil }); err != nil || !permit {
		t.Fatalf("begin permit=%v err=%v", permit, err)
	}
	initial := integration.OutboxResult{ObservationID: "race-initial", State: integration.OutboxUnknown, Evidence: "response lost"}
	if err := store.FinishOutbox(t.Context(), claim, initial, func(integration.Session) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(t.Context(), `CREATE TABLE outbox_race_effects(id text PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	second := New(secondQueueDB(t, store.db))
	type resolveResult struct {
		observation string
		err         error
	}
	results := make(chan resolveResult, 2)
	var wg sync.WaitGroup
	for _, attempt := range []struct {
		repository  *Store
		observation string
	}{{store, "race-page-a"}, {second, "race-page-b"}} {
		wg.Go(func() {
			result := integration.OutboxResult{ObservationID: attempt.observation, State: integration.OutboxUnknown, Evidence: "page checkpoint", ExpectedPrevious: initial.ObservationID}
			err := attempt.repository.ResolveOutbox(t.Context(), claim, result, func(scope integration.Session) error {
				_, err := scope.(*session).tx.ExecContext(t.Context(), `INSERT INTO outbox_race_effects(id) VALUES($1)`, attempt.observation)
				return err
			})
			results <- resolveResult{attempt.observation, err}
		})
	}
	wg.Wait()
	close(results)
	winner, successes, conflicts := "", 0, 0
	for result := range results {
		switch {
		case result.err == nil:
			winner, successes = result.observation, successes+1
		case errors.Is(result.err, billing.ErrConflict):
			conflicts++
		default:
			t.Fatal(result.err)
		}
	}
	var pointer string
	var effects, observations int
	if err := store.db.QueryRowContext(t.Context(), `SELECT last_observation_id,(SELECT count(*) FROM outbox_race_effects),(SELECT count(*) FROM billing_outbox_results WHERE account_id=$1 AND message_id=$2) FROM billing_outbox WHERE account_id=$1 AND message_id=$2`, message.Account, message.ID).Scan(&pointer, &effects, &observations); err != nil {
		t.Fatal(err)
	}
	if successes != 1 || conflicts != 1 || pointer != winner || effects != 1 || observations != 2 {
		t.Fatalf("successes=%d conflicts=%d winner=%q pointer=%q effects=%d observations=%d", successes, conflicts, winner, pointer, effects, observations)
	}
}

func TestReleaseOutboxRequeuesAnUnusedPermitAndRefusesAfterAnObservation(t *testing.T) {
	store, message, claim := claimedOutbox(t, "outbox-release", "release-unsent", time.Minute)
	ctx := t.Context()
	if permit, err := store.BeginOutbox(ctx, claim, func(integration.Session) error { return nil }); err != nil || !permit {
		t.Fatalf("permit=%v err=%v", permit, err)
	}
	// The provider refused before the mutation could take effect.
	if err := store.ReleaseOutbox(ctx, claim, "provider_rate_limited", time.Now().UTC().Add(-time.Second)); err != nil {
		t.Fatalf("release err=%v", err)
	}
	delivery, err := store.Outbox(ctx, message.Account, message.Scope, message.ID)
	if err != nil || delivery.State != "pending" {
		t.Fatalf("delivery=%+v err=%v, want pending", delivery, err)
	}
	// The message is claimable again and the permission is grantable again,
	// which is the whole point: nothing was sent.
	next, ok, err := store.Claim(ctx, integration.Outbound, "outbox-worker-2", time.Now().UTC(), time.Minute)
	if err != nil || !ok || next.Message.ID != message.ID {
		t.Fatalf("reclaim ok=%v err=%v", ok, err)
	}
	if permit, err := store.BeginOutbox(ctx, next, func(integration.Session) error { return nil }); err != nil || !permit {
		t.Fatalf("second permit=%v err=%v", permit, err)
	}
	observation := integration.OutboxResult{ObservationID: "release-observed", State: integration.OutboxUnknown, Evidence: "provider_outcome_uncertain"}
	if err := store.FinishOutbox(ctx, next, observation, func(integration.Session) error { return nil }); err != nil {
		t.Fatalf("finish err=%v", err)
	}
	// Once an outcome is recorded the send is no longer known to be unsent.
	if err := store.ReleaseOutbox(ctx, next, "provider_rate_limited", time.Now().UTC()); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("release after observation err=%v, want conflict", err)
	}
}
