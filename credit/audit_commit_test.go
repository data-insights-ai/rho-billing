package credit

import (
	"context"
	"errors"
	"testing"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
)

var errAuditCommit = errors.New("audit commit failed")

type auditCommitFailureRepository struct{ Repository }

func (r auditCommitFailureRepository) WithinAccount(ctx context.Context, account billing.AccountID, fn func(Tx) error) error {
	err := r.Repository.WithinAccount(ctx, account, fn)
	if err != nil {
		return err
	}
	return errAuditCommit
}

func TestVerifyLedgerReturnsZeroReportWhenCommitFails(t *testing.T) {
	base := NewMemoryRepository("audit-commit")
	engine := New(base, nil)
	if _, err := engine.Grant(t.Context(), GrantInput{Account: "audit-commit", Operation: "audit-grant", LotID: "audit-lot", Unit: billing.Unit{Code: "credits", Scale: 1}, Amount: 3, Source: "audit", SourceRef: "grant", ValidFrom: testClockTime()}); err != nil {
		t.Fatal(err)
	}
	wrapped := New(auditCommitFailureRepository{Repository: base}, nil)
	report, err := wrapped.VerifyLedger(t.Context(), "audit-commit")
	if !errors.Is(err, errAuditCommit) {
		t.Fatalf("error=%v", err)
	}
	if report != (AuditReport{}) {
		t.Fatalf("report=%+v, want zero report", report)
	}
}

func testClockTime() time.Time { return time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC) }
