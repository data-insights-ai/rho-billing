package purchase

import (
	"errors"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
)

func TestCommandRecoveryTransitions(t *testing.T) {
	t.Run("planned unknown repeated unknown rejected", func(t *testing.T) {
		service, now, _, _, intent := lifecycleFixture(t)
		unknown := recordRecoveryCommand(t, service, intent, "recover-missing", CommandUnknown, "observation-missing", "")
		unknown = recordRecoveryCommand(t, service, unknown, "recover-unresolved", CommandUnknown, "observation-unresolved", "")
		rejected := recordRecoveryCommand(t, service, unknown, "recover-rejected", CommandRejected, "observation-rejected", "")
		if rejected.Command != CommandRejected || rejected.Revision != intent.Revision+3 || !rejected.UpdatedAt.Equal(now) {
			t.Fatalf("rejected=%+v", rejected)
		}
	})

	for _, test := range []struct {
		name       string
		dispatched bool
		state      CommandState
	}{
		{name: "planned reconciled", state: CommandReconciled},
		{name: "dispatched reconciled", dispatched: true, state: CommandReconciled},
		{name: "planned rejected", state: CommandRejected},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, _, _, _, intent := lifecycleFixture(t)
			if test.dispatched {
				var err error
				intent, err = service.RecordCommand(t.Context(), CommandInput{Account: intent.Account, IntentID: intent.ID, Operation: "dispatch", ExpectedRevision: intent.Revision, State: CommandDispatched, ProviderReference: "provider-command", OccurredAt: intent.CreatedAt})
				if err != nil {
					t.Fatal(err)
				}
			}
			providerReference := ""
			if test.state == CommandReconciled {
				providerReference = "provider-command"
			}
			got := recordRecoveryCommand(t, service, intent, "recovery", test.state, "observation-authoritative", providerReference)
			if got.Command != test.state {
				t.Fatalf("command=%q, want %q", got.Command, test.state)
			}
		})
	}
}

func TestCommandRecoveryRequiresEvidenceAndForbidsRetryOrRewind(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T, *Service, Intent) Intent
		to    CommandState
	}{
		{name: "planned unknown", setup: unchangedRecoveryIntent, to: CommandUnknown},
		{name: "planned rejected", setup: unchangedRecoveryIntent, to: CommandRejected},
		{name: "planned reconciled", setup: unchangedRecoveryIntent, to: CommandReconciled},
		{name: "dispatched reconciled", setup: dispatchedRecoveryIntent, to: CommandReconciled},
		{name: "unknown unknown", setup: unknownRecoveryIntent, to: CommandUnknown},
		{name: "unknown rejected", setup: unknownRecoveryIntent, to: CommandRejected},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, _, _, _, base := lifecycleFixture(t)
			intent := test.setup(t, service, base)
			providerReference := ""
			if test.to == CommandReconciled {
				providerReference = "provider-command"
			}
			out, err := service.RecordCommand(t.Context(), CommandInput{Account: intent.Account, IntentID: intent.ID, Operation: "missing-evidence", ExpectedRevision: intent.Revision, State: test.to, ProviderReference: providerReference, OccurredAt: intent.CreatedAt})
			if !errors.Is(err, billing.ErrInvalid) || out != (Intent{}) {
				t.Fatalf("out=%+v err=%v", out, err)
			}
		})
	}

	service, _, _, _, base := lifecycleFixture(t)
	unknown := unknownRecoveryIntent(t, service, base)
	if out, err := service.RecordCommand(t.Context(), CommandInput{Account: unknown.Account, IntentID: unknown.ID, Operation: "retry-dispatch", ExpectedRevision: unknown.Revision, State: CommandDispatched, ProviderReference: "provider-command", OccurredAt: unknown.CreatedAt}); !errors.Is(err, billing.ErrState) || out != (Intent{}) {
		t.Fatalf("unknown retry out=%+v err=%v", out, err)
	}
	rejected := recordRecoveryCommand(t, service, unknown, "terminal-rejected", CommandRejected, "observation-rejected", "")
	if out, err := service.RecordCommand(t.Context(), CommandInput{Account: rejected.Account, IntentID: rejected.ID, Operation: "terminal-rewind", ExpectedRevision: rejected.Revision, State: CommandUnknown, EvidenceReference: "observation-late", OccurredAt: rejected.CreatedAt}); !errors.Is(err, billing.ErrState) || out != (Intent{}) {
		t.Fatalf("terminal rewind out=%+v err=%v", out, err)
	}
}

func TestCommandRecoveryReplayConflictAndPaymentRace(t *testing.T) {
	service, now, scope, quote, intent := lifecycleFixture(t)
	fact := PaymentFact{Account: intent.Account, Scope: scope, EventID: "action-required", TransactionID: "provider-transaction", IntentID: intent.ID, Status: FactActionRequired, Currency: quote.Currency, OccurredAt: now.Add(time.Minute)}
	result, err := service.ApplyPayment(t.Context(), fact)
	if err != nil || !result.Applied {
		t.Fatalf("payment result=%+v err=%v", result, err)
	}
	current, err := service.Intent(t.Context(), intent.Account, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	in := CommandInput{Account: current.Account, IntentID: current.ID, Operation: "recovery-after-payment", ExpectedRevision: current.Revision, State: CommandReconciled, ProviderReference: "provider-command", EvidenceReference: "observation-paid-race", OccurredAt: now.Add(2 * time.Minute)}
	first, err := service.RecordCommand(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := service.RecordCommand(t.Context(), in)
	if err != nil || replay != first {
		t.Fatalf("replay=%+v err=%v want=%+v", replay, err, first)
	}
	if first.Payment != PaymentActionRequired || first.Fulfillment != FulfillmentPending || !first.LastPaymentAt.Equal(fact.OccurredAt) || first.LastPaymentEventID != fact.EventID {
		t.Fatalf("recovery changed payment fields: %+v", first)
	}
	changed := in
	changed.EvidenceReference = "observation-conflict"
	if out, err := service.RecordCommand(t.Context(), changed); !errors.Is(err, billing.ErrConflict) || out != (Intent{}) {
		t.Fatalf("changed replay out=%+v err=%v", out, err)
	}
}

func recordRecoveryCommand(t *testing.T, service *Service, intent Intent, operation string, state CommandState, evidence, providerReference string) Intent {
	t.Helper()
	out, err := service.RecordCommand(t.Context(), CommandInput{Account: intent.Account, IntentID: intent.ID, Operation: operation, ExpectedRevision: intent.Revision, State: state, ProviderReference: providerReference, EvidenceReference: evidence, OccurredAt: intent.CreatedAt})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func unchangedRecoveryIntent(_ *testing.T, _ *Service, intent Intent) Intent { return intent }

func dispatchedRecoveryIntent(t *testing.T, service *Service, intent Intent) Intent {
	t.Helper()
	out, err := service.RecordCommand(t.Context(), CommandInput{Account: intent.Account, IntentID: intent.ID, Operation: "dispatch", ExpectedRevision: intent.Revision, State: CommandDispatched, ProviderReference: "provider-command", OccurredAt: intent.CreatedAt})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func unknownRecoveryIntent(t *testing.T, service *Service, intent Intent) Intent {
	t.Helper()
	return recordRecoveryCommand(t, service, intent, "initial-unknown", CommandUnknown, "observation-initial", "")
}
