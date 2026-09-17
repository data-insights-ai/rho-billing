package usage

// Settlement coordinates provider submission without making provider calls itself.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/internal/checked"
	"github.com/data-insights-ai/rho-billing/internal/identity"
)

var (
	ErrReconciliationRequired = errors.New("usage: authoritative reconciliation required")
	ErrCapability             = errors.New("usage: provider capability is insufficient")
	ErrStaleRevision          = errors.New("usage: stale submission revision")
)

type BatchState string

const (
	BatchPreparing  BatchState = "preparing"
	BatchCanceling  BatchState = "canceling"
	BatchReady      BatchState = "ready"
	BatchSubmitting BatchState = "submitting"
	BatchConfirmed  BatchState = "confirmed"
	BatchRejected   BatchState = "rejected"
	BatchUnknown    BatchState = "unknown"
)

type AttemptState string

const (
	AttemptSubmitting AttemptState = "submitting"
	AttemptConfirmed  AttemptState = "confirmed"
	AttemptRejected   AttemptState = "rejected"
	AttemptUnknown    AttemptState = "unknown"
)

type SubmissionStatus string

const (
	SubmissionConfirmed SubmissionStatus = "confirmed"
	SubmissionRejected  SubmissionStatus = "rejected"
	SubmissionUnknown   SubmissionStatus = "unknown"
)

// ProviderResult: settlement never performs the provider call itself.
type ProviderResult struct {
	Status            SubmissionStatus
	ProviderReference string
	Reason            string
}

type BillingPeriod struct {
	Start  time.Time
	End    time.Time
	Cutoff time.Time
	Scope  BillingScope
}

// Equal compares the instants and scope a billing period covers. Never use ==
// on a BillingPeriod: its time.Time fields compare their monotonic reading and
// location pointer too, so a period read back from a database is unequal to the
// one that was written.
func (p BillingPeriod) Equal(other BillingPeriod) bool {
	return p.Start.Equal(other.Start) && p.End.Equal(other.End) && p.Cutoff.Equal(other.Cutoff) && p.Scope == other.Scope
}

func (p BillingPeriod) Valid() bool {
	return !p.Start.IsZero() && p.End.After(p.Start) && !p.Cutoff.Before(p.Start) && p.Scope.Valid()
}

func (p BillingPeriod) UTC() BillingPeriod {
	p.Start = billing.CanonicalTime(p.Start)
	p.End = billing.CanonicalTime(p.End)
	p.Cutoff = billing.CanonicalTime(p.Cutoff)
	return p
}

type ChargeKind string

const (
	ChargeUsage      ChargeKind = "usage"
	ChargeAdjustment ChargeKind = "adjustment"
)

type ChargeLine struct {
	Kind            ChargeKind
	UsageID         string
	AdjustmentID    string
	OriginalUsageID string
	Source          string
	Actor           string
	Project         string
	Scope           BillingScope
	OccurredAt      time.Time
	Interval        billing.Period
	Amount          int64
	ExactAmount     string
	RuleVersion     string
	Currency        string
	Reason          string
}

type Batch struct {
	Account         billing.AccountID
	ID              string
	OriginalBatchID string
	Period          BillingPeriod
	Currency        string
	Lines           []ChargeLine
	Total           int64
	State           BatchState
	Revision        int64
	CreatedAt       time.Time
	UpdatedAt       time.Time
	Fingerprint     string
}

type SettlementCapability struct {
	SupportsIdempotency bool
	SupportsLookup      bool
}

type Attempt struct {
	BatchID           string
	ID                string
	Provider          string
	IdempotencyKey    string
	ProviderReference string
	State             AttemptState
	Revision          int64
	Capability        SettlementCapability
	Reason            string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

type Outcome struct {
	Fingerprint string
	Error       string
	Batch       BatchSummary
	Attempt     Attempt
}

type BatchRecord struct {
	Summary     BatchSummary
	Fingerprint string
}

type Adjustment struct {
	ID              string
	OriginalUsageID string
	Amount          int64
	Currency        string
	Reason          string
}

type CorrectionInput struct {
	Account         billing.AccountID
	Operation       billing.OperationID
	BatchID         string
	OriginalBatchID string
	Period          BillingPeriod
	Currency        string
	UsageIDs        []string
	Adjustments     []Adjustment
	CreatedAt       time.Time
}

type BeginSubmissionInput struct {
	Account        billing.AccountID
	Operation      billing.OperationID
	BatchID        string
	AttemptID      string
	Provider       string
	IdempotencyKey string
	Capability     SettlementCapability
	CreatedAt      time.Time
}

type CompleteSubmissionInput struct {
	Account           billing.AccountID
	Operation         billing.OperationID
	BatchID           string
	AttemptID         string
	ExpectedRevision  int64
	Status            SubmissionStatus
	ProviderReference string
	Reason            string
	CompletedAt       time.Time
}

type ReconcileUnknownInput struct {
	Account           billing.AccountID
	Operation         billing.OperationID
	BatchID           string
	AttemptID         string
	ExpectedRevision  int64
	Status            SubmissionStatus
	ProviderReference string
	Evidence          string
	ReconciledAt      time.Time
}

type SettlementTx interface {
	BatchHeader(string) (BatchRecord, bool, error)
	UsageByIDs([]string) ([]Record, error)
	OriginalUsageIDs(string, []string) ([]string, error)
	UsageClaims([]UsageClaimRequest) (map[string]string, error)
	ReserveUsageClaims(string, []UsageClaimRequest) error
	InsertCorrectionBatch(BatchRecord, []ChargeLine) error
	UpdateBatchState(BatchSummary, int64) error
	Attempt(string) (Attempt, bool, error)
	PutAttempt(Attempt) error
	FindAttempt(string, string) (Attempt, bool, error)
	Outcome(billing.OperationID) (Outcome, bool, error)
	PutOutcome(billing.OperationID, Outcome) error
}

type UsageClaimRequest struct{ UsageID, SourceFingerprint string }

type SettlementRepository interface {
	WithinAccount(context.Context, billing.AccountID, func(SettlementTx) error) error
	CloseRepository
	Attempt(context.Context, billing.AccountID, string) (Attempt, error)
}

type SettlementService struct {
	repo SettlementRepository
	now  func() time.Time
}

// Recorded terminal failures return *SettlementRejection; use errors.AsType to
// distinguish them from storage failures and commit. Other errors must abort.
func NewSettlement(repo SettlementRepository, now func() time.Time) *SettlementService {
	if repo == nil {
		panic("usage: nil settlement repository")
	}
	if now == nil {
		now = time.Now
	}
	return &SettlementService{repo: repo, now: now}
}

func prepareBatch(in CloseInput, records []Record) (Batch, error) {
	period, err := validateBatchInput(in.Account, in.BatchID, in.Period, in.Currency, in.CreatedAt)
	if err != nil {
		return Batch{}, err
	}
	lines, total, err := prepareUsageLines(in.Account, in.Currency, period, records, false)
	if err != nil {
		return Batch{}, err
	}
	return newBatch(in.Account, in.BatchID, "", period, in.Currency, lines, total, in.CreatedAt), nil
}

func prepareCorrection(in CorrectionInput, original Batch, originalIDs []string, records []Record) (Batch, error) {
	if original.ID == "" || original.Account != in.Account || original.Currency != in.Currency || original.State == "" || original.Period.Scope != in.Period.Scope {
		return Batch{}, billing.ErrInvalid
	}
	period, err := validateBatchInput(in.Account, in.BatchID, in.Period, in.Currency, in.CreatedAt)
	if err != nil {
		return Batch{}, err
	}
	if in.OriginalBatchID != original.ID || in.BatchID == original.ID {
		return Batch{}, billing.ErrConflict
	}
	originalUsage := make(map[string]struct{}, len(originalIDs))
	for _, id := range originalIDs {
		originalUsage[id] = struct{}{}
	}
	lines, total, err := prepareUsageLines(in.Account, in.Currency, period, records, true)
	if err != nil {
		return Batch{}, err
	}
	seenAdjustments := make(map[string]struct{}, len(in.Adjustments))
	for _, adjustment := range in.Adjustments {
		if !billing.ValidID(adjustment.ID) || !billing.ValidID(adjustment.OriginalUsageID) || adjustment.Amount == 0 || adjustment.Currency != in.Currency || strings.TrimSpace(adjustment.Reason) == "" {
			return Batch{}, billing.ErrInvalid
		}
		if _, exists := originalUsage[adjustment.OriginalUsageID]; !exists {
			return Batch{}, billing.ErrConflict
		}
		if _, exists := seenAdjustments[adjustment.ID]; exists {
			return Batch{}, billing.ErrConflict
		}
		seenAdjustments[adjustment.ID] = struct{}{}
		lines = append(lines, ChargeLine{Kind: ChargeAdjustment, AdjustmentID: adjustment.ID, OriginalUsageID: adjustment.OriginalUsageID, Scope: period.Scope, Amount: adjustment.Amount, Currency: adjustment.Currency, Reason: adjustment.Reason})
		total, err = checked.Add(total, adjustment.Amount)
		if err != nil {
			return Batch{}, err
		}
	}
	if len(lines) == 0 {
		return Batch{}, billing.ErrInvalid
	}
	slices.SortFunc(lines, func(a, b ChargeLine) int {
		return strings.Compare(lineKey(a), lineKey(b))
	})
	return newBatch(in.Account, in.BatchID, original.ID, period, in.Currency, lines, total, in.CreatedAt), nil
}

func applyBeginSubmission(batch Batch, in BeginSubmissionInput) (Batch, Attempt, error) {
	if !billing.ValidID(in.BatchID) || in.BatchID != batch.ID || !billing.ValidID(in.AttemptID) || in.Provider == "" || in.IdempotencyKey == "" || in.CreatedAt.IsZero() {
		return Batch{}, Attempt{}, billing.ErrInvalid
	}
	if !in.Capability.SupportsIdempotency && !in.Capability.SupportsLookup {
		return Batch{}, Attempt{}, ErrCapability
	}
	if batch.State != BatchReady || batch.Revision != 0 {
		return Batch{}, Attempt{}, billing.ErrState
	}
	when := billing.CanonicalTime(in.CreatedAt)
	batch.State = BatchSubmitting
	batch.Revision = 1
	batch.UpdatedAt = when
	attempt := Attempt{BatchID: batch.ID, ID: in.AttemptID, Provider: in.Provider, IdempotencyKey: in.IdempotencyKey, State: AttemptSubmitting, Revision: batch.Revision, Capability: in.Capability, CreatedAt: when, UpdatedAt: when}
	return batch, attempt, nil
}

func applyCompleteSubmission(batch Batch, attempt Attempt, in CompleteSubmissionInput) (Batch, Attempt, error) {
	if batch.State != BatchSubmitting || attempt.State != AttemptSubmitting || attempt.BatchID != batch.ID || batch.ID != in.BatchID || attempt.ID != in.AttemptID || in.ExpectedRevision != batch.Revision || batch.Revision != attempt.Revision || in.CompletedAt.IsZero() {
		return Batch{}, Attempt{}, ErrStaleRevision
	}
	if in.Status != SubmissionConfirmed && in.Status != SubmissionRejected && in.Status != SubmissionUnknown {
		return Batch{}, Attempt{}, billing.ErrInvalid
	}
	when := billing.CanonicalTime(in.CompletedAt)
	revision, err := checked.Add(batch.Revision, 1)
	if err != nil {
		return Batch{}, Attempt{}, err
	}
	batch.Revision = revision
	batch.UpdatedAt = when
	attempt.Revision = batch.Revision
	attempt.UpdatedAt = when
	switch in.Status {
	case SubmissionConfirmed:
		if in.ProviderReference == "" {
			return Batch{}, Attempt{}, billing.ErrInvalid
		}
		batch.State, attempt.State, attempt.ProviderReference = BatchConfirmed, AttemptConfirmed, in.ProviderReference
	case SubmissionRejected:
		if strings.TrimSpace(in.Reason) == "" {
			return Batch{}, Attempt{}, billing.ErrInvalid
		}
		batch.State, attempt.State, attempt.Reason = BatchRejected, AttemptRejected, in.Reason
	case SubmissionUnknown:
		if strings.TrimSpace(in.Reason) == "" {
			return Batch{}, Attempt{}, billing.ErrInvalid
		}
		batch.State, attempt.State, attempt.Reason = BatchUnknown, AttemptUnknown, in.Reason
	}
	return batch, attempt, nil
}

func applyReconcileUnknown(batch Batch, attempt Attempt, in ReconcileUnknownInput) (Batch, Attempt, error) {
	if batch.State != BatchUnknown || attempt.State != AttemptUnknown || attempt.BatchID != batch.ID || batch.ID != in.BatchID || attempt.ID != in.AttemptID || in.ExpectedRevision != batch.Revision || batch.Revision != attempt.Revision {
		return Batch{}, Attempt{}, ErrStaleRevision
	}
	if !attempt.Capability.SupportsLookup || strings.TrimSpace(in.Evidence) == "" || in.ReconciledAt.IsZero() {
		return Batch{}, Attempt{}, ErrReconciliationRequired
	}
	if in.Status != SubmissionConfirmed && in.Status != SubmissionRejected && in.Status != SubmissionUnknown {
		return Batch{}, Attempt{}, billing.ErrInvalid
	}
	if in.Status == SubmissionConfirmed && in.ProviderReference == "" || in.Status != SubmissionConfirmed && in.ProviderReference != "" {
		return Batch{}, Attempt{}, billing.ErrInvalid
	}
	when := billing.CanonicalTime(in.ReconciledAt)
	revision, err := checked.Add(batch.Revision, 1)
	if err != nil {
		return Batch{}, Attempt{}, err
	}
	batch.Revision = revision
	batch.UpdatedAt = when
	attempt.Revision = batch.Revision
	attempt.UpdatedAt = when
	switch in.Status {
	case SubmissionConfirmed:
		batch.State, attempt.State, attempt.ProviderReference = BatchConfirmed, AttemptConfirmed, in.ProviderReference
	case SubmissionRejected:
		batch.State, attempt.State, attempt.Reason = BatchRejected, AttemptRejected, in.Evidence
	case SubmissionUnknown:
		batch.State, attempt.State, attempt.Reason = BatchUnknown, AttemptUnknown, in.Evidence
	}
	return batch, attempt, nil
}

func (s *SettlementService) Correct(ctx context.Context, in CorrectionInput) (BatchSummary, error) {
	if !validOperation(in.Account, in.Operation) {
		return BatchSummary{}, billing.ErrInvalid
	}
	if in.CreatedAt.IsZero() {
		in.CreatedAt = billing.CanonicalTime(s.now())
	} else {
		in.CreatedAt = billing.CanonicalTime(in.CreatedAt)
	}
	in.Period = in.Period.UTC()
	if len(in.UsageIDs) > 1000 || len(in.Adjustments) > 1000-len(in.UsageIDs) {
		return BatchSummary{}, billing.ErrInvalid
	}
	fingerprint := correctionFingerprint(in)
	var out Outcome
	var records []Record
	err := s.repo.WithinAccount(ctx, in.Account, func(tx SettlementTx) error {
		old, ok, err := tx.Outcome(in.Operation)
		if err != nil {
			return err
		}
		if ok {
			if old.Fingerprint != fingerprint {
				return billing.ErrConflict
			}
			out = old
			return nil
		}
		header, exists, err := tx.BatchHeader(in.OriginalBatchID)
		if err != nil {
			return err
		}
		var batch Batch
		var actionErr error
		if !exists {
			actionErr = billing.ErrNotFound
		} else {
			adjustmentIDs := make([]string, 0, len(in.Adjustments))
			for _, adjustment := range in.Adjustments {
				adjustmentIDs = append(adjustmentIDs, adjustment.OriginalUsageID)
			}
			originalIDs, readErr := tx.OriginalUsageIDs(in.OriginalBatchID, adjustmentIDs)
			if readErr != nil {
				return readErr
			}
			records, readErr = tx.UsageByIDs(in.UsageIDs)
			if readErr != nil {
				return readErr
			}
			original := Batch{Account: header.Summary.Account, ID: header.Summary.ID, OriginalBatchID: header.Summary.OriginalBatchID, Period: header.Summary.Period, Currency: header.Summary.Currency, Total: header.Summary.Total, State: header.Summary.State, Revision: header.Summary.Revision, CreatedAt: header.Summary.CreatedAt, UpdatedAt: header.Summary.UpdatedAt, Fingerprint: header.Fingerprint}
			batch, actionErr = prepareCorrection(in, original, originalIDs, records)
			if actionErr == nil {
				if _, exists, err = tx.BatchHeader(batch.ID); err != nil {
					return err
				} else if exists {
					actionErr = billing.ErrConflict
				}
			}
		}
		if actionErr == nil {
			claims := make([]UsageClaimRequest, 0, len(batch.Lines))
			for _, line := range batch.Lines {
				if line.Kind == ChargeUsage {
					claims = append(claims, UsageClaimRequest{UsageID: line.UsageID, SourceFingerprint: recordsFingerprint(records, line.UsageID)})
				}
			}
			existing, claimErr := tx.UsageClaims(claims)
			if claimErr != nil {
				return claimErr
			}
			for _, claim := range claims {
				if owner := existing[claim.UsageID]; owner != "" && owner != batch.ID {
					return billing.ErrConflict
				}
			}
			if claimErr := tx.ReserveUsageClaims(batch.ID, claims); claimErr != nil {
				return claimErr
			}
		}
		if actionErr == nil {
			if err = tx.InsertCorrectionBatch(BatchRecord{Summary: batchSummary(batch), Fingerprint: batch.Fingerprint}, batch.Lines); err != nil {
				return err
			}
		}
		return storeOutcome(tx, in.Operation, fingerprint, batchSummary(batch), Attempt{}, actionErr, &out)
	})
	if err != nil {
		return BatchSummary{}, err
	}
	return out.Batch, outcomeError(out.Error)
}

func (s *SettlementService) BeginSubmission(ctx context.Context, in BeginSubmissionInput) (Attempt, error) {
	if !validOperation(in.Account, in.Operation) {
		return Attempt{}, billing.ErrInvalid
	}
	if in.CreatedAt.IsZero() {
		in.CreatedAt = billing.CanonicalTime(s.now())
	} else {
		in.CreatedAt = billing.CanonicalTime(in.CreatedAt)
	}
	fingerprint := beginFingerprint(in)
	var out Outcome
	err := s.repo.WithinAccount(ctx, in.Account, func(tx SettlementTx) error {
		old, ok, err := tx.Outcome(in.Operation)
		if err != nil {
			return err
		}
		if ok {
			if old.Fingerprint != fingerprint {
				return billing.ErrConflict
			}
			out = old
			return nil
		}
		header, exists, err := tx.BatchHeader(in.BatchID)
		if err != nil {
			return err
		}
		batch := batchFromSummary(header.Summary)
		var attempt Attempt
		var actionErr error
		if !exists {
			actionErr = billing.ErrNotFound
		} else {
			if _, attemptExists, findErr := tx.Attempt(in.AttemptID); findErr != nil {
				return findErr
			} else if attemptExists {
				actionErr = billing.ErrConflict
			} else if _, keyExists, findErr := tx.FindAttempt(in.Provider, in.IdempotencyKey); findErr != nil {
				return findErr
			} else if keyExists {
				actionErr = billing.ErrConflict
			} else {
				batch, attempt, actionErr = applyBeginSubmission(batch, in)
			}
		}
		if actionErr == nil {
			if err = tx.UpdateBatchState(transitionedSummary(header.Summary, batch), batch.Revision-1); err != nil {
				return err
			}
			if err = tx.PutAttempt(attempt); err != nil {
				return err
			}
		}
		return storeOutcome(tx, in.Operation, fingerprint, BatchSummary{}, attempt, actionErr, &out)
	})
	if err != nil {
		return Attempt{}, err
	}
	return out.Attempt, outcomeError(out.Error)
}

func (s *SettlementService) CompleteSubmission(ctx context.Context, in CompleteSubmissionInput) (BatchSummary, error) {
	if !validOperation(in.Account, in.Operation) {
		return BatchSummary{}, billing.ErrInvalid
	}
	if in.CompletedAt.IsZero() {
		in.CompletedAt = billing.CanonicalTime(s.now())
	} else {
		in.CompletedAt = billing.CanonicalTime(in.CompletedAt)
	}
	fingerprint := completeFingerprint(in)
	var out Outcome
	err := s.repo.WithinAccount(ctx, in.Account, func(tx SettlementTx) error {
		old, ok, err := tx.Outcome(in.Operation)
		if err != nil {
			return err
		}
		if ok {
			if old.Fingerprint != fingerprint {
				return billing.ErrConflict
			}
			out = old
			return nil
		}
		header, exists, err := tx.BatchHeader(in.BatchID)
		if err != nil {
			return err
		}
		batch := batchFromSummary(header.Summary)
		attempt, attemptExists, err := tx.Attempt(in.AttemptID)
		if err != nil {
			return err
		}
		var actionErr error
		if !exists || !attemptExists {
			actionErr = billing.ErrNotFound
		} else {
			batch, attempt, actionErr = applyCompleteSubmission(batch, attempt, in)
			if actionErr == nil {
				if err = tx.UpdateBatchState(transitionedSummary(header.Summary, batch), batch.Revision-1); err != nil {
					return err
				}
				if err = tx.PutAttempt(attempt); err != nil {
					return err
				}
			}
		}
		return storeOutcome(tx, in.Operation, fingerprint, transitionedSummary(header.Summary, batch), attempt, actionErr, &out)
	})
	if err != nil {
		return BatchSummary{}, err
	}
	return out.Batch, outcomeError(out.Error)
}

func (s *SettlementService) ReconcileUnknown(ctx context.Context, in ReconcileUnknownInput) (BatchSummary, error) {
	if !validOperation(in.Account, in.Operation) {
		return BatchSummary{}, billing.ErrInvalid
	}
	if in.ReconciledAt.IsZero() {
		in.ReconciledAt = billing.CanonicalTime(s.now())
	} else {
		in.ReconciledAt = billing.CanonicalTime(in.ReconciledAt)
	}
	fingerprint := reconcileFingerprint(in)
	var out Outcome
	err := s.repo.WithinAccount(ctx, in.Account, func(tx SettlementTx) error {
		old, ok, err := tx.Outcome(in.Operation)
		if err != nil {
			return err
		}
		if ok {
			if old.Fingerprint != fingerprint {
				return billing.ErrConflict
			}
			out = old
			return nil
		}
		header, exists, err := tx.BatchHeader(in.BatchID)
		if err != nil {
			return err
		}
		batch := batchFromSummary(header.Summary)
		attempt, attemptExists, err := tx.Attempt(in.AttemptID)
		if err != nil {
			return err
		}
		var actionErr error
		if !exists || !attemptExists {
			actionErr = billing.ErrNotFound
		} else {
			batch, attempt, actionErr = applyReconcileUnknown(batch, attempt, in)
			if actionErr == nil {
				if err = tx.UpdateBatchState(transitionedSummary(header.Summary, batch), batch.Revision-1); err != nil {
					return err
				}
				if err = tx.PutAttempt(attempt); err != nil {
					return err
				}
			}
		}
		return storeOutcome(tx, in.Operation, fingerprint, transitionedSummary(header.Summary, batch), attempt, actionErr, &out)
	})
	if err != nil {
		return BatchSummary{}, err
	}
	return out.Batch, outcomeError(out.Error)
}

func (s *SettlementService) Attempt(ctx context.Context, account billing.AccountID, id string) (Attempt, error) {
	return s.repo.Attempt(ctx, account, id)
}

func validateBatchInput(account billing.AccountID, id string, period BillingPeriod, currency string, createdAt time.Time) (BillingPeriod, error) {
	if !billing.ValidID(string(account)) || !billing.ValidID(id) || !period.Valid() || period.Start.Location() == nil || !validCurrency(currency) || createdAt.IsZero() || billing.CanonicalTime(createdAt).Before(billing.CanonicalTime(period.Cutoff)) {
		return BillingPeriod{}, billing.ErrInvalid
	}
	return period.UTC(), nil
}

func prepareUsageLines(account billing.AccountID, currency string, period BillingPeriod, records []Record, late bool) ([]ChargeLine, int64, error) {
	lines := make([]ChargeLine, 0, len(records))
	seen := make(map[string]struct{}, len(records))
	aggregates := make(map[string]struct{})
	var total int64
	for _, record := range records {
		o := record.Observation
		if o.Account != account || !billing.ValidID(o.ID) || !billing.ValidID(o.Source) || o.Funding != Postpaid || o.Input.Waived || record.Fingerprint == "" || record.Fingerprint != Identity(record) || record.ReceivedAt.IsZero() || o.OccurredAt.IsZero() {
			return nil, 0, billing.ErrInvalid
		}
		if late && !record.ReceivedAt.After(period.Cutoff) {
			return nil, 0, billing.ErrInvalid
		}
		if !late && record.ReceivedAt.After(period.Cutoff) {
			return nil, 0, billing.ErrInvalid
		}
		if _, exists := seen[o.ID]; exists {
			return nil, 0, billing.ErrConflict
		}
		seen[o.ID] = struct{}{}
		if !period.Cutoff.After(o.OccurredAt) || o.OccurredAt.Before(period.Start) || !o.OccurredAt.Before(period.End) {
			return nil, 0, billing.ErrInvalid
		}
		if !validPeriodContainment(o.Interval, period) {
			return nil, 0, billing.ErrInvalid
		}
		if record.Rating.Money == nil || record.Rating.Credits != nil || record.Rating.Money.Currency != currency || record.Rating.Target.Currency != currency || record.Rating.Money.MinorUnits != record.Rating.RoundedAmount || record.Rating.RoundedAmount < 0 || record.Rating.ExactAmount == "" || record.Rating.RuleVersion == "" || record.Rating.Evidence.Waived {
			return nil, 0, billing.ErrInvalid
		}
		if o.Scope != period.Scope {
			return nil, 0, billing.ErrInvalid
		}
		if err := ValidatePostpaidAggregate(record, period.Start, period.End); err != nil {
			return nil, 0, err
		}
		if aggregateKey := PostpaidAggregateIdentity(record, period.Start, period.End); aggregateKey != "" {
			if _, exists := aggregates[aggregateKey]; exists {
				return nil, 0, billing.ErrConflict
			}
			aggregates[aggregateKey] = struct{}{}
		}
		line := ChargeLine{Kind: ChargeUsage, UsageID: o.ID, Source: o.Source, Actor: o.Actor, Project: o.Project, Scope: o.Scope, OccurredAt: billing.CanonicalTime(o.OccurredAt), Interval: billing.Period{Start: billing.CanonicalTime(o.Interval.Start), End: billing.CanonicalTime(o.Interval.End)}, Amount: record.Rating.Money.MinorUnits, ExactAmount: record.Rating.ExactAmount, RuleVersion: record.Rating.RuleVersion, Currency: currency}
		lines = append(lines, line)
		var err error
		total, err = checked.Add(total, line.Amount)
		if err != nil {
			return nil, 0, err
		}
	}
	slices.SortFunc(lines, func(a, b ChargeLine) int { return strings.Compare(lineKey(a), lineKey(b)) })
	return lines, total, nil
}

func validPeriodContainment(interval billing.Period, period BillingPeriod) bool {
	if interval.Start.IsZero() && interval.End.IsZero() {
		return true
	}
	boundary := period.End
	if period.Cutoff.Before(boundary) {
		boundary = period.Cutoff
	}
	return interval.Valid() && !interval.Start.Before(period.Start) && !interval.End.After(boundary)
}

func newBatch(account billing.AccountID, id, original string, period BillingPeriod, currency string, lines []ChargeLine, total int64, createdAt time.Time) Batch {
	createdAt = billing.CanonicalTime(createdAt)
	batch := Batch{Account: account, ID: id, OriginalBatchID: original, Period: period.UTC(), Currency: currency, Lines: slices.Clone(lines), Total: total, State: BatchReady, CreatedAt: createdAt, UpdatedAt: createdAt}
	batch.Fingerprint = batchFingerprint(batch)
	return batch
}

func validCurrency(currency string) bool {
	if len(currency) != 3 || strings.ToUpper(currency) != currency {
		return false
	}
	for _, char := range currency {
		if char < 'A' || char > 'Z' {
			return false
		}
	}
	return true
}

func validOperation(account billing.AccountID, op billing.OperationID) bool {
	return billing.ValidID(string(account)) && billing.ValidID(string(op))
}

func storeOutcome(tx SettlementTx, op billing.OperationID, fingerprint string, summary BatchSummary, attempt Attempt, actionErr error, out *Outcome) error {
	if actionErr != nil && !expected(actionErr) {
		return actionErr
	}
	result := Outcome{Fingerprint: fingerprint, Error: errorCode(actionErr), Batch: summary, Attempt: attempt}
	if err := tx.PutOutcome(op, result); err != nil {
		return err
	}
	*out = result
	return nil
}

func batchSummary(batch Batch) BatchSummary {
	return BatchSummary{Account: batch.Account, ID: batch.ID, OriginalBatchID: batch.OriginalBatchID, Period: batch.Period, Currency: batch.Currency, Total: batch.Total, State: batch.State, Revision: batch.Revision, LineCount: int64(len(batch.Lines)), CreatedAt: batch.CreatedAt, UpdatedAt: batch.UpdatedAt}
}

func batchFromSummary(s BatchSummary) Batch {
	return Batch{Account: s.Account, ID: s.ID, OriginalBatchID: s.OriginalBatchID, Period: s.Period, Currency: s.Currency, Total: s.Total, State: s.State, Revision: s.Revision, CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt}
}

func transitionedSummary(base BatchSummary, updated Batch) BatchSummary {
	base.State, base.Revision, base.UpdatedAt = updated.State, updated.Revision, updated.UpdatedAt
	return base
}

func recordsFingerprint(records []Record, id string) string {
	for _, record := range records {
		if record.Observation.ID == id {
			return record.Fingerprint
		}
	}
	return ""
}

func expected(err error) bool {
	return errors.Is(err, billing.ErrInvalid) || errors.Is(err, billing.ErrConflict) || errors.Is(err, billing.ErrNotFound) || errors.Is(err, billing.ErrState) || errors.Is(err, billing.ErrOverflow) || errors.Is(err, ErrReconciliationRequired) || errors.Is(err, ErrCapability) || errors.Is(err, ErrStaleRevision)
}

func errorCode(err error) string {
	if err == nil {
		return ""
	}
	for _, item := range []struct {
		err  error
		code string
	}{{billing.ErrInvalid, "invalid"}, {billing.ErrConflict, "conflict"}, {billing.ErrNotFound, "not_found"}, {billing.ErrState, "state"}, {billing.ErrOverflow, "overflow"}, {ErrReconciliationRequired, "reconcile"}, {ErrCapability, "capability"}, {ErrStaleRevision, "stale"}} {
		if errors.Is(err, item.err) {
			return item.code
		}
	}
	return ""
}

func outcomeError(code string) error {
	switch code {
	case "":
		return nil
	case "invalid":
		return &SettlementRejection{Cause: billing.ErrInvalid}
	case "conflict":
		return &SettlementRejection{Cause: billing.ErrConflict}
	case "not_found":
		return &SettlementRejection{Cause: billing.ErrNotFound}
	case "state":
		return &SettlementRejection{Cause: billing.ErrState}
	case "overflow":
		return &SettlementRejection{Cause: billing.ErrOverflow}
	case "reconcile":
		return &SettlementRejection{Cause: ErrReconciliationRequired}
	case "capability":
		return &SettlementRejection{Cause: ErrCapability}
	case "stale":
		return &SettlementRejection{Cause: ErrStaleRevision}
	default:
		return fmt.Errorf("usage: corrupt operation outcome %q", code)
	}
}

func cloneBatch(batch Batch) Batch {
	batch.Lines = slices.Clone(batch.Lines)
	return batch
}

func lineKey(line ChargeLine) string {
	if line.Kind == ChargeAdjustment {
		return "adjustment:" + line.AdjustmentID
	}
	return "usage:" + line.UsageID
}

func scopeFingerprint(scope BillingScope) string {
	return identity.Fingerprint(scope.Subscription.Scope.Provider, scope.Subscription.Scope.Merchant, scope.Subscription.Scope.Environment, scope.Subscription.ID, scope.ItemID)
}

func batchFingerprint(batch Batch) string {
	fields := []string{string(batch.Account), batch.ID, batch.OriginalBatchID, identity.Instant(batch.Period.Start), identity.Instant(batch.Period.End), identity.Instant(batch.Period.Cutoff), scopeFingerprint(batch.Period.Scope), batch.Currency}
	for _, line := range batch.Lines {
		fields = append(fields, lineKey(line), line.OriginalUsageID, line.Source, line.Actor, line.Project, scopeFingerprint(line.Scope), identity.Instant(line.OccurredAt), identity.Instant(line.Interval.Start), identity.Instant(line.Interval.End), fmt.Sprint(line.Amount), line.ExactAmount, line.RuleVersion, line.Currency, line.Reason)
	}
	return identity.Fingerprint(fields...)
}

func correctionFingerprint(in CorrectionInput) string {
	fields := []string{"correct", string(in.Account), string(in.Operation), in.BatchID, in.OriginalBatchID, identity.Instant(in.Period.Start), identity.Instant(in.Period.End), identity.Instant(in.Period.Cutoff), scopeFingerprint(in.Period.Scope), in.Currency}
	identities := slices.Clone(in.UsageIDs)
	slices.Sort(identities)
	fields = append(fields, "usage-ids", fmt.Sprint(len(identities)))
	fields = append(fields, identities...)
	fields = append(fields, "adjustments", fmt.Sprint(len(in.Adjustments)))
	for _, adjustment := range in.Adjustments {
		fields = append(fields, adjustment.ID, adjustment.OriginalUsageID, fmt.Sprint(adjustment.Amount), adjustment.Currency, adjustment.Reason)
	}
	return identity.Fingerprint(fields...)
}

func beginFingerprint(in BeginSubmissionInput) string {
	return identity.Fingerprint("begin", string(in.Account), string(in.Operation), in.BatchID, in.AttemptID, in.Provider, in.IdempotencyKey, fmt.Sprint(in.Capability.SupportsIdempotency), fmt.Sprint(in.Capability.SupportsLookup))
}

func completeFingerprint(in CompleteSubmissionInput) string {
	return identity.Fingerprint("complete", string(in.Account), string(in.Operation), in.BatchID, in.AttemptID, fmt.Sprint(in.ExpectedRevision), string(in.Status), in.ProviderReference, in.Reason)
}

func reconcileFingerprint(in ReconcileUnknownInput) string {
	return identity.Fingerprint("reconcile", string(in.Account), string(in.Operation), in.BatchID, in.AttemptID, fmt.Sprint(in.ExpectedRevision), string(in.Status), in.ProviderReference, in.Evidence)
}
