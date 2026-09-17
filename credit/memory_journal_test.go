package credit

import (
	"errors"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
)

func TestMemoryJournalBatchValidationAndRollback(t *testing.T) {
	repo := NewMemoryRepository("journal")
	now := time.Date(2026, 9, 15, 12, 0, 0, 123456789, time.UTC)
	first := Entry{OperationID: "op", Kind: "grant", RecordedAt: now, EffectiveAt: now, Available: 2}
	for _, invalid := range []Entry{
		{OperationID: "", Kind: "grant"},
		{OperationID: "op", Kind: ""},
		{OperationID: "op", Kind: "grant", Sequence: 99},
	} {
		err := repo.WithinAccount(t.Context(), "journal", func(tx Tx) error {
			return tx.AppendEntries([]Entry{first, invalid})
		})
		if !errors.Is(err, billing.ErrInvalid) && !errors.Is(err, billing.ErrConflict) {
			t.Fatalf("invalid entry error=%v", err)
		}
		rows, err := repo.History(t.Context(), "journal", 0, 100)
		if err != nil || len(rows) != 0 {
			t.Fatalf("partial batch rows=%v err=%v", rows, err)
		}
	}
	if err := repo.WithinAccount(t.Context(), "journal", func(tx Tx) error {
		if err := tx.AppendEntries(nil); err != nil {
			return err
		}
		if err := tx.AppendEntries([]Entry{first, first}); err != nil {
			return err
		}
		next := first
		next.Sequence = 3
		return tx.Append(next)
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := repo.History(t.Context(), "journal", 0, 100)
	if err != nil || len(rows) != 3 {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
	for i, row := range rows {
		want := first
		want.Sequence = int64(i + 1)
		want.RecordedAt = billing.CanonicalTime(now)
		want.EffectiveAt = billing.CanonicalTime(now)
		if row != want {
			t.Fatalf("row %d=%+v want %+v", i, row, want)
		}
	}
	if first.Sequence != 0 || first.RecordedAt != now {
		t.Fatal("mutated caller input")
	}
}
