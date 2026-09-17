package pg

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
)

type journalBatchQueryCounter struct {
	sequenceUpdates atomic.Int64
	journalInserts  atomic.Int64
}

func (c *journalBatchQueryCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	sql := strings.ToUpper(data.SQL)
	if strings.Contains(sql, "UPDATE BILLING_ACCOUNTS") {
		c.sequenceUpdates.Add(1)
	}
	if strings.Contains(sql, "INSERT INTO BILLING_JOURNAL") {
		c.journalInserts.Add(1)
	}
	return ctx
}

func (*journalBatchQueryCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func openTracedJournalStore(t testing.TB, db *sql.DB) (*Store, *journalBatchQueryCounter) {
	t.Helper()
	ctx := t.Context()
	var schema string
	if err := db.QueryRowContext(ctx, "SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	config, err := pgx.ParseConfig(os.Getenv("BILLING_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	config.RuntimeParams["search_path"] = schema
	counter := new(journalBatchQueryCounter)
	config.Tracer = counter
	traced := stdlib.OpenDB(*config)
	t.Cleanup(func() { _ = traced.Close() })
	return New(traced), counter
}

func prepareJournalBatchReferences(t testing.TB, store *Store, account billing.AccountID) {
	t.Helper()
	now := testTime()
	if err := store.WithinAccount(t.Context(), account, func(tx credit.Tx) error {
		lot := testLot("journal-batch-lot", 1)
		lot.Initial, lot.Available = 2000, 2000
		if err := tx.PutLot(lot); err != nil {
			return err
		}
		return tx.PutReservation(credit.Reservation{
			ID: "journal-batch-reservation", Actor: "actor", Unit: "credits",
			CreatedAt: now, Deadline: now.Add(time.Hour), State: "held", Authorized: 2000,
		})
	}); err != nil {
		t.Fatal(err)
	}
}

func journalBatchEntries(count int, invalidIndex int) []credit.Entry {
	entries := make([]credit.Entry, count)
	when := testTime()
	for i := range entries {
		entries[i] = credit.Entry{
			OperationID:       billing.OperationID(fmt.Sprintf("journal-op-%04d", i)),
			Kind:              "grant",
			Reason:            fmt.Sprintf("batch-test-%d", i),
			RecordedAt:        when,
			EffectiveAt:       when.Add(time.Duration(i) * time.Second),
			Available:         int64(i%5) - 2,
			Held:              -int64(i%7) - 1,
			Consumed:          int64(i%11) + 3,
			Expired:           -int64(i%13) - 4,
			Revoked:           int64(i%17) + 5,
			PendingRevocation: -int64(i%19) - 6,
		}
		if i%3 == 0 {
			entries[i].LotID = "journal-batch-lot"
		}
		if i%5 == 0 {
			entries[i].ReservationID = "journal-batch-reservation"
		}
	}
	if invalidIndex >= 0 && invalidIndex < len(entries) {
		entries[invalidIndex].LotID = "missing-journal-batch-lot"
	}
	return entries
}

func journalHistory(t testing.TB, store *Store, ctx context.Context, account billing.AccountID, want int) []credit.Entry {
	t.Helper()
	all := make([]credit.Entry, 0, want)
	var after int64
	for len(all) < want {
		page, err := store.History(ctx, account, after, 1000)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		all = append(all, page...)
		after = page[len(page)-1].Sequence
	}
	return all
}

func TestPostgresAppendEntriesBatchesAndPreservesOrder(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	const account = billing.AccountID("journal-batch-account")
	if err := store.CreateAccount(ctx, account, "journal-batch-subject"); err != nil {
		t.Fatal(err)
	}
	prepareJournalBatchReferences(t, store, account)
	tracedStore, counter := openTracedJournalStore(t, db)
	entries := journalBatchEntries(1200, -1)

	if err := tracedStore.WithinAccount(ctx, account, func(tx credit.Tx) error {
		return tx.AppendEntries(entries)
	}); err != nil {
		t.Fatal(err)
	}
	if got := counter.sequenceUpdates.Load(); got != 3 {
		t.Fatalf("sequence UPDATE statements=%d, want 3", got)
	}
	if got := counter.journalInserts.Load(); got != 3 {
		t.Fatalf("journal INSERT statements=%d, want 3", got)
	}
	t.Logf("journal batch statements for 1200 entries: sequence updates=%d, journal inserts=%d", counter.sequenceUpdates.Load(), counter.journalInserts.Load())

	history := journalHistory(t, tracedStore, ctx, account, len(entries))
	if len(history) != len(entries) {
		t.Fatalf("history entries=%d, want %d", len(history), len(entries))
	}
	for i, got := range history {
		want := entries[i]
		want.Sequence = int64(i + 1)
		if got != want {
			t.Fatalf("history[%d]=%+v, want %+v", i, got, want)
		}
	}

	if err := tracedStore.WithinAccount(ctx, account, func(tx credit.Tx) error {
		if err := tx.AppendEntries(nil); err != nil {
			return err
		}
		return tx.Append(credit.Entry{OperationID: "journal-continuation", Kind: "grant", Reason: "single", RecordedAt: testTime(), EffectiveAt: testTime(), Available: 1})
	}); err != nil {
		t.Fatal(err)
	}
	if got := counter.sequenceUpdates.Load(); got != 4 {
		t.Fatalf("sequence UPDATE statements after single append=%d, want 4", got)
	}
	if got := counter.journalInserts.Load(); got != 4 {
		t.Fatalf("journal INSERT statements after single append=%d, want 4", got)
	}
	continuation, err := tracedStore.History(ctx, account, 1200, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(continuation) != 1 || continuation[0].Sequence != 1201 || continuation[0].OperationID != "journal-continuation" {
		t.Fatalf("continuation history=%+v", continuation)
	}
}

func TestPostgresAppendEntriesSequenceConflictRollsBackAndRetries(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	const account = billing.AccountID("journal-sequence-conflict")
	if err := store.CreateAccount(ctx, account, "journal-sequence-conflict-subject"); err != nil {
		t.Fatal(err)
	}
	prepareJournalBatchReferences(t, store, account)
	tracedStore, _ := openTracedJournalStore(t, db)
	conflict := journalBatchEntries(2, -1)
	conflict[0].Sequence = 99
	if err := tracedStore.WithinAccount(ctx, account, func(tx credit.Tx) error {
		return tx.AppendEntries(conflict)
	}); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("sequence conflict error=%v, want ErrConflict", err)
	}
	var sequence int64
	if err := db.QueryRowContext(ctx, "SELECT next_journal_sequence FROM billing_accounts WHERE account_id=$1", account).Scan(&sequence); err != nil {
		t.Fatal(err)
	}
	if sequence != 0 {
		t.Fatalf("sequence after conflict=%d, want 0", sequence)
	}
	if history, err := tracedStore.History(ctx, account, 0, 10); err != nil || len(history) != 0 {
		t.Fatalf("history after conflict=%+v, err=%v", history, err)
	}
	if err := tracedStore.WithinAccount(ctx, account, func(tx credit.Tx) error {
		return tx.AppendEntries(journalBatchEntries(2, -1))
	}); err != nil {
		t.Fatalf("retry after sequence conflict: %v", err)
	}
}

func TestPostgresAppendEntriesSecondPageFailureRollsBackAndRetries(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	const account = billing.AccountID("journal-second-page-failure")
	if err := store.CreateAccount(ctx, account, "journal-second-page-failure-subject"); err != nil {
		t.Fatal(err)
	}
	prepareJournalBatchReferences(t, store, account)
	tracedStore, counter := openTracedJournalStore(t, db)
	entries := journalBatchEntries(1200, 700)
	if err := tracedStore.WithinAccount(ctx, account, func(tx credit.Tx) error {
		return tx.AppendEntries(entries)
	}); err == nil {
		t.Fatal("second-page foreign-key failure unexpectedly succeeded")
	}
	if got := counter.sequenceUpdates.Load(); got != 2 {
		t.Fatalf("failed batch sequence UPDATE statements=%d, want 2", got)
	}
	if got := counter.journalInserts.Load(); got != 2 {
		t.Fatalf("failed batch journal INSERT statements=%d, want 2", got)
	}
	if history, err := tracedStore.History(ctx, account, 0, 10); err != nil || len(history) != 0 {
		t.Fatalf("history after failed page=%+v, err=%v", history, err)
	}
	var sequence int64
	if err := db.QueryRowContext(ctx, "SELECT next_journal_sequence FROM billing_accounts WHERE account_id=$1", account).Scan(&sequence); err != nil {
		t.Fatal(err)
	}
	if sequence != 0 {
		t.Fatalf("sequence after failed page=%d, want 0", sequence)
	}

	entries[700].LotID = "journal-batch-lot"
	if err := tracedStore.WithinAccount(ctx, account, func(tx credit.Tx) error {
		return tx.AppendEntries(entries)
	}); err != nil {
		t.Fatalf("retry after failed page: %v", err)
	}
	if got := counter.sequenceUpdates.Load(); got != 5 {
		t.Fatalf("sequence UPDATE statements after retry=%d, want 5", got)
	}
	if got := counter.journalInserts.Load(); got != 5 {
		t.Fatalf("journal INSERT statements after retry=%d, want 5", got)
	}
	if history := journalHistory(t, tracedStore, ctx, account, 1200); len(history) != 1200 {
		t.Fatalf("history after retry entries=%d", len(history))
	}
}

func TestPostgresAppendEntriesSequenceOverflowLeavesStateUnchanged(t *testing.T) {
	store, db := testStore(t)
	ctx := t.Context()
	const account = billing.AccountID("journal-sequence-overflow")
	if err := store.CreateAccount(ctx, account, "journal-sequence-overflow-subject"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE billing_accounts SET next_journal_sequence=$1 WHERE account_id=$2`, math.MaxInt64-1, account); err != nil {
		t.Fatal(err)
	}
	tracedStore, _ := openTracedJournalStore(t, db)
	entry := credit.Entry{OperationID: "journal-overflow-first", Kind: "grant", Reason: "overflow", RecordedAt: testTime(), EffectiveAt: testTime(), Available: 1}
	if err := tracedStore.WithinAccount(ctx, account, func(tx credit.Tx) error { return tx.AppendEntries([]credit.Entry{entry}) }); err != nil {
		t.Fatalf("append at MaxInt64: %v", err)
	}
	if err := tracedStore.WithinAccount(ctx, account, func(tx credit.Tx) error {
		return tx.AppendEntries([]credit.Entry{{OperationID: "journal-overflow-second", Kind: "grant", Reason: "overflow", RecordedAt: testTime(), EffectiveAt: testTime(), Available: 1}})
	}); !errors.Is(err, billing.ErrOverflow) {
		t.Fatalf("append past MaxInt64 error=%v, want ErrOverflow", err)
	}
	var sequence int64
	if err := db.QueryRowContext(ctx, "SELECT next_journal_sequence FROM billing_accounts WHERE account_id=$1", account).Scan(&sequence); err != nil {
		t.Fatal(err)
	}
	if sequence != math.MaxInt64 {
		t.Fatalf("sequence after overflow=%d, want MaxInt64", sequence)
	}
	history, err := tracedStore.History(ctx, account, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || history[0].Sequence != math.MaxInt64 || history[0].OperationID != entry.OperationID {
		t.Fatalf("history after overflow=%+v", history)
	}
}
