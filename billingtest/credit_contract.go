package billingtest

import (
	"context"
	"errors"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
)

// RunCreditContract exercises the shared credit behavior against a repository
// implementation. It deliberately uses only the public credit API so memory
// and Postgres cannot drift behind backend-specific assertions.
func RunCreditContract(t testing.TB, repo credit.Repository, account, other billing.AccountID) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	engine := credit.New(repo, func() time.Time { return now })
	unit := billing.Unit{Code: "credits", Scale: 1}
	grant := func(op billing.OperationID, lot, source string, amount int64, scope string, expiry time.Time) credit.Result {
		t.Helper()
		result, err := engine.Grant(ctx, credit.GrantInput{Account: account, Operation: op, LotID: lot, Unit: unit, Amount: amount, Scope: scope, Source: "contract", SourceRef: source, ValidFrom: now, ExpiresAt: expiry})
		if err != nil {
			t.Fatalf("grant %s: %v", lot, err)
		}
		return result
	}

	grant("grant-early", "lot-early", "source-early", 3, "project", now.Add(time.Hour))
	grant("grant-late", "lot-late", "source-late", 5, "project", now.Add(2*time.Hour))
	grant("grant-global", "lot-global", "source-global", 2, "", time.Time{})
	grant("grant-revoke", "lot-revoke", "source-revoke", 4, "revoke-scope", time.Time{})

	fefo, err := engine.Reserve(ctx, credit.ReserveInput{Account: account, Operation: "reserve-fefo", ReservationID: "reservation-fefo", Actor: "actor", Unit: unit.Code, Scope: "project", Amount: 4, Deadline: now.Add(time.Hour)})
	if err != nil || fefo.ReservationID != "reservation-fefo" {
		t.Fatalf("FEFO reservation: %+v, %v", fefo, err)
	}
	reservation, err := engine.Reservation(ctx, account, "reservation-fefo")
	if err != nil || len(reservation.Allocations) != 2 || reservation.Allocations[0] != (credit.Allocation{LotID: "lot-early", Amount: 3}) || reservation.Allocations[1] != (credit.Allocation{LotID: "lot-late", Amount: 1}) {
		t.Fatalf("FEFO allocations: %+v, %v", reservation, err)
	}
	if _, err := engine.Release(ctx, credit.ReleaseInput{Account: account, Operation: "release-fefo", ReservationID: "reservation-fefo", Reason: "contract"}); err != nil {
		t.Fatal(err)
	}
	project, err := engine.Balance(ctx, account, unit.Code, "project")
	if err != nil || project.Available != 10 {
		t.Fatalf("scope balance: %+v, %v", project, err)
	}
	otherScope, err := engine.Balance(ctx, account, unit.Code, "other")
	if err != nil || otherScope.Available != 2 {
		t.Fatalf("scope filtering: %+v, %v", otherScope, err)
	}

	if _, err := engine.Reserve(ctx, credit.ReserveInput{Account: account, Operation: "reserve-deadline", ReservationID: "reservation-deadline", Actor: "actor", Unit: unit.Code, Scope: "project", Amount: 2, Deadline: now.Add(30 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(30 * time.Minute)
	if _, err := engine.Balance(ctx, account, unit.Code, "project"); err != nil {
		t.Fatal(err)
	}
	deadlineReservation, err := engine.Reservation(ctx, account, "reservation-deadline")
	if err != nil || deadlineReservation.State != "timed_out" {
		t.Fatalf("exact deadline reservation: %+v, %v", deadlineReservation, err)
	}
	now = time.Date(2026, 9, 15, 13, 0, 0, 0, time.UTC)
	expiredBalance, err := engine.Balance(ctx, account, unit.Code, "project")
	if err != nil || expiredBalance.Expired != 3 {
		t.Fatalf("expiry boundary: %+v, %v", expiredBalance, err)
	}

	if _, err := engine.Reserve(ctx, credit.ReserveInput{Account: account, Operation: "reserve-extend", ReservationID: "reservation-extend", Actor: "actor", Unit: unit.Code, Scope: "project", Amount: 1, Deadline: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Extend(ctx, credit.ExtendInput{Account: account, Operation: "extend", ReservationID: "reservation-extend", Additional: 1}); err != nil {
		t.Fatal(err)
	}
	extended, err := engine.Reservation(ctx, account, "reservation-extend")
	if err != nil || extended.Authorized != 2 {
		t.Fatalf("extended reservation: %+v, %v", extended, err)
	}
	if _, err := engine.Release(ctx, credit.ReleaseInput{Account: account, Operation: "release-extend", ReservationID: "reservation-extend", Reason: "contract"}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Reserve(ctx, credit.ReserveInput{Account: account, Operation: "reuse-terminal", ReservationID: "reservation-extend", Actor: "actor", Unit: unit.Code, Scope: "project", Amount: 1, Deadline: now.Add(time.Hour)}); !errors.Is(err, billing.ErrConflict) || !credit.IsRejection(err) {
		t.Fatalf("terminal reservation ID reuse: %v", err)
	}
	if _, err := engine.Reserve(ctx, credit.ReserveInput{Account: account, Operation: "reserve-duplicate-usage-a", ReservationID: "reservation-duplicate-usage-a", Actor: "actor", Unit: unit.Code, Scope: "project", Amount: 1, Deadline: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Settle(ctx, credit.SettleInput{Account: account, Operation: "settle-duplicate-usage-a", ReservationID: "reservation-duplicate-usage-a", Actual: 1, Evidence: credit.Evidence{UsageID: "duplicate-usage", RatingVersion: "contract", Metrics: []credit.Metric{{Name: "units", Quantity: 1}}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Reserve(ctx, credit.ReserveInput{Account: account, Operation: "reserve-duplicate-usage-b", ReservationID: "reservation-duplicate-usage-b", Actor: "actor", Unit: unit.Code, Scope: "project", Amount: 1, Deadline: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Settle(ctx, credit.SettleInput{Account: account, Operation: "settle-duplicate-usage-b", ReservationID: "reservation-duplicate-usage-b", Actual: 1, Evidence: credit.Evidence{UsageID: "duplicate-usage", RatingVersion: "contract", Metrics: []credit.Metric{{Name: "units", Quantity: 1}}}}); !errors.Is(err, billing.ErrConflict) || !credit.IsRejection(err) {
		t.Fatalf("duplicate usage identity: %v", err)
	}

	if _, err := engine.Revoke(ctx, credit.RevokeInput{Account: account, Operation: "revoke-global", LotID: "lot-global", Reason: "contract"}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Reserve(ctx, credit.ReserveInput{Account: account, Operation: "reserve-revoke", ReservationID: "reservation-revoke", Actor: "actor", Unit: unit.Code, Scope: "revoke-scope", Amount: 2, Deadline: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Revoke(ctx, credit.RevokeInput{Account: account, Operation: "revoke", LotID: "lot-revoke", Reason: "contract"}); err != nil {
		t.Fatal(err)
	}
	lot, err := readContractLot(ctx, repo, account, "lot-revoke")
	if err != nil || lot.Available != 0 || lot.Held != 2 || lot.Revoked != 2 {
		t.Fatalf("full revoke with hold: %+v, %v", lot, err)
	}
	if _, err := engine.Release(ctx, credit.ReleaseInput{Account: account, Operation: "release-revoked", ReservationID: "reservation-revoke", Reason: "contract"}); err != nil {
		t.Fatal(err)
	}
	lot, err = readContractLot(ctx, repo, account, "lot-revoke")
	if err != nil || lot.Held != 0 || lot.Revoked != 4 {
		t.Fatalf("released revoked hold: %+v, %v", lot, err)
	}

	period := billing.Period{Start: now.Add(-time.Minute), End: now.Add(time.Hour)}
	if _, err := engine.SetLimit(ctx, credit.LimitInput{Account: account, Operation: "set-limit", Limit: credit.Limit{Actor: "limited", Unit: unit.Code, Period: period, Amount: 3}}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Reserve(ctx, credit.ReserveInput{Account: account, Operation: "reserve-limit-a", ReservationID: "reservation-limit-a", Actor: "limited", Unit: unit.Code, Scope: "project", Amount: 2, Deadline: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Settle(ctx, credit.SettleInput{Account: account, Operation: "settle-limit-a", ReservationID: "reservation-limit-a", Actual: 1, Evidence: credit.Evidence{UsageID: "limit-usage", RatingVersion: "contract", Metrics: []credit.Metric{{Name: "units", Quantity: 1}}}}); err != nil {
		t.Fatal(err)
	}
	limitAttempt := credit.ReserveInput{Account: account, Operation: "reserve-limit-b", ReservationID: "reservation-limit-b", Actor: "limited", Unit: unit.Code, Scope: "project", Amount: 3, Deadline: now.Add(time.Hour)}
	if _, err := engine.Reserve(ctx, limitAttempt); !errors.Is(err, billing.ErrLimit) || !credit.IsRejection(err) {
		t.Fatalf("limit rejection: %v", err)
	}
	if _, err := engine.Reserve(ctx, limitAttempt); !errors.Is(err, billing.ErrLimit) || !credit.IsRejection(err) {
		t.Fatalf("replayed limit rejection: %v", err)
	}

	historyBefore, err := repo.History(ctx, account, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	badGrant := credit.GrantInput{Account: account, Operation: "rejected-grant", LotID: "rejected-lot", Unit: unit, Amount: 0, Source: "contract", SourceRef: "rejected-source", ValidFrom: now}
	if _, err := engine.Grant(ctx, badGrant); !errors.Is(err, billing.ErrInvalid) || !credit.IsRejection(err) {
		t.Fatalf("recorded rejected grant: %v", err)
	}
	if _, err := engine.Grant(ctx, badGrant); !errors.Is(err, billing.ErrInvalid) || !credit.IsRejection(err) {
		t.Fatalf("replayed rejected grant: %v", err)
	}
	historyAfter, err := repo.History(ctx, account, 0, 1000)
	if err != nil || len(historyAfter) != len(historyBefore) {
		t.Fatalf("rejected grant wrote forbidden journal entry: before=%d after=%d err=%v", len(historyBefore), len(historyAfter), err)
	}

	rollbackErr := errors.New("contract rollback")
	if err := repo.WithinAccount(ctx, account, func(tx credit.Tx) error {
		if err := tx.PutLot(credit.Lot{ID: "rollback-lot", Unit: unit, Source: "contract", SourceRef: "rollback", ValidFrom: now, Initial: 1, Available: 1}); err != nil {
			return err
		}
		return rollbackErr
	}); !errors.Is(err, rollbackErr) {
		t.Fatalf("rollback callback: %v", err)
	}
	if _, err := engine.Grant(ctx, credit.GrantInput{Account: account, Operation: "grant-rollback", LotID: "rollback-lot", Unit: unit, Amount: 1, Source: "contract", SourceRef: "rollback", ValidFrom: now}); err != nil {
		t.Fatalf("rollback retained forbidden lot: %v", err)
	}

	if _, err := engine.Grant(ctx, credit.GrantInput{Account: other, Operation: "grant-other-scale", LotID: "other-scale-lot", Unit: billing.Unit{Code: unit.Code, Scale: 2}, Amount: 1, Source: "contract", SourceRef: "other-scale", ValidFrom: now}); !errors.Is(err, billing.ErrConflict) || !credit.IsRejection(err) {
		t.Fatalf("global unit scale mismatch: %v", err)
	}
	entries, err := repo.History(ctx, account, 0, 1000)
	if err != nil || len(entries) == 0 {
		t.Fatalf("journal history: %d, %v", len(entries), err)
	}
	for i, entry := range entries {
		if entry.Sequence != int64(i+1) {
			t.Fatalf("journal sequence %d at index %d", entry.Sequence, i)
		}
	}
	audit, err := engine.VerifyLedger(ctx, account)
	if err != nil || audit.Lots == 0 || audit.Entries != int64(len(entries)) {
		t.Fatalf("ledger audit: %+v, %v", audit, err)
	}
}

func readContractLot(ctx context.Context, repo credit.Repository, account billing.AccountID, id string) (credit.Lot, error) {
	var out credit.Lot
	err := repo.WithinAccount(ctx, account, func(tx credit.Tx) error {
		lots, err := tx.Lots()
		if err != nil {
			return err
		}
		for _, lot := range lots {
			if lot.ID == id {
				out = lot
				return nil
			}
		}
		return billing.ErrNotFound
	})
	return out, err
}
