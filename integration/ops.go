package integration

// Ops constructors do not start goroutines, migrate, or contact providers.

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"

	"github.com/data-insights-ai/rho-billing/internal/identity"
	"github.com/data-insights-ai/rho-billing/usage"
)

type RetryClass string

const (
	RetryNone  RetryClass = "none"
	RetryLater RetryClass = "later"
	RetryNever RetryClass = "never"
)

var ErrBackpressure = errors.New("integration: provider backpressure")

type ProcessDueInput struct {
	Worker       string
	Limit        int
	Lease        time.Duration
	MaxAttempts  int
	RetryAfter   time.Duration
	Handler      func(Session) error
	Backpressure time.Duration
}

type ProcessDueResult struct {
	Claimed    int
	Processed  int
	Failed     int
	Unknown    int
	Canceled   bool
	RetryClass RetryClass
}

type ReconcileInput struct {
	Account          billing.AccountID
	DryRun           bool
	ExpectedRevision int64
	Actor            string
	Reason           string
	Evidence         string
	RepairID         string
}

type DriftKind string

const DriftLedger DriftKind = "ledger"

type Drift struct {
	Kind     DriftKind
	Identity string
	Detail   string
}

type ReconcileReport struct {
	Account  billing.AccountID
	DryRun   bool
	Revision int64
	Drifts   []Drift
	Audit    credit.AuditReport
}

type RepairRecord struct {
	Account   billing.AccountID
	ID        string
	Revision  int64
	Actor     string
	Reason    string
	Evidence  string
	Drifts    []Drift
	CreatedAt time.Time
}

type BackfillInput struct {
	Account billing.AccountID
	ID      string
	Limit   int
}

type BackfillState string

const (
	BackfillRunning BackfillState = "running"
	BackfillDone    BackfillState = "done"
)

type BackfillJob struct {
	Account   billing.AccountID
	ID        string
	Cursor    int64
	Processed int64
	State     BackfillState
}

type TombstoneKind string

const TombstoneUsage TombstoneKind = "usage"

type Tombstone struct {
	Kind        TombstoneKind
	ID          string
	Fingerprint string
}

type OpsSnapshot struct {
	Account    billing.AccountID
	Tombstones []Tombstone
	Usage      []usage.Record
}

type EventKind string

const (
	EventQueueLag EventKind = "queue_lag"
	EventUnknown  EventKind = "unknown_command"
	EventDrift    EventKind = "financial_drift"
	EventRecovery EventKind = "recovery"
	EventBackfill EventKind = "backfill"
)

type Event struct {
	Kind     EventKind
	Account  billing.AccountID
	Worker   string
	Reason   string
	RepairID string
	Actor    string
	JobID    string
	Value    int64
}

type Observer func(Event)

type Inbox interface {
	Claim(context.Context, Direction, string, time.Time, time.Duration) (Claim, bool, error)
	ProcessInbox(context.Context, Claim, func(Session) error) error
	Fail(context.Context, Claim, string, time.Time, int) error
}

type OpsRepository interface {
	WithinAccount(context.Context, billing.AccountID, func(OpsTx) error) error
}

type OpsTx interface {
	Repair(context.Context, string) (RepairRecord, error)
	SaveRepair(context.Context, RepairRecord) error
	Backfill(context.Context, string) (BackfillJob, error)
	SaveBackfill(context.Context, BackfillJob) error
	Tombstone(context.Context, string, string) (Tombstone, error)
	SaveTombstone(context.Context, Tombstone) error
	Tombstones(context.Context, string, int) ([]Tombstone, error)
}

type OpsService struct {
	repo    OpsRepository
	inbox   Inbox
	credits *credit.Engine
	usage   *usage.Service
	observe Observer
	now     func() time.Time
}

func NewOps(repo OpsRepository, now func() time.Time) *OpsService {
	if repo == nil {
		panic("integration: nil ops repository")
	}
	if now == nil {
		now = time.Now
	}
	return &OpsService{repo: repo, now: now}
}

func (s *OpsService) WithInbox(inbox Inbox) *OpsService { s.inbox = inbox; return s }
func (s *OpsService) WithCredits(engine *credit.Engine) *OpsService {
	s.credits = engine
	return s
}
func (s *OpsService) WithUsage(u *usage.Service) *OpsService { s.usage = u; return s }
func (s *OpsService) WithObserver(obs Observer) *OpsService  { s.observe = obs; return s }

// Unknown financial outcomes and fence conflicts are never retried as a fresh send.
func Classify(err error) RetryClass {
	if err == nil {
		return RetryNone
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return RetryNone
	}
	if errors.Is(err, billing.ErrConflict) {
		return RetryNever
	}
	if errors.Is(err, ErrBackpressure) {
		return RetryLater
	}
	return RetryLater
}

func Redact(value string) string {
	lower := strings.ToLower(value)
	for _, secret := range []string{"password", "api-key", "apikey", "api_key", "bearer", "token", "secret", "authorization"} {
		if strings.Contains(lower, secret) {
			return "[redacted]"
		}
	}
	return value
}

func (s *OpsService) emit(ev Event) {
	if s.observe == nil {
		return
	}
	ev.Worker = Redact(ev.Worker)
	ev.Reason = Redact(ev.Reason)
	ev.RepairID = Redact(ev.RepairID)
	ev.Actor = Redact(ev.Actor)
	ev.JobID = Redact(ev.JobID)
	s.observe(ev)
}

func (s *OpsService) ProcessDue(ctx context.Context, in ProcessDueInput) (ProcessDueResult, error) {
	if s.inbox == nil || !billing.ValidID(in.Worker) || in.Limit < 1 || in.Limit > 1000 || in.Lease <= 0 || in.MaxAttempts < 1 || in.Handler == nil {
		return ProcessDueResult{}, billing.ErrInvalid
	}
	var out ProcessDueResult
	for range in.Limit {
		if err := ctx.Err(); err != nil {
			out.Canceled = true
			out.RetryClass = RetryNone
			return out, err
		}
		// Sampled per message, not once per batch: a batch of up to a thousand
		// can span minutes, and a stale instant both shortens the lease each
		// later claim is granted and schedules its retry in the past.
		now := billing.CanonicalTime(s.now())
		claim, ok, err := s.inbox.Claim(ctx, Inbound, in.Worker, now, in.Lease)
		if err != nil {
			return out, err
		}
		if !ok {
			s.emit(Event{Kind: EventQueueLag, Value: 0, Worker: in.Worker})
			return out, nil
		}
		out.Claimed++
		err = s.inbox.ProcessInbox(ctx, claim, in.Handler)
		if err == nil {
			out.Processed++
			continue
		}
		class := Classify(err)
		out.RetryClass = class
		switch class {
		case RetryNone:
			out.Canceled = errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
			return out, err
		case RetryNever:
			out.Failed++
			// A conflict is not always terminal: a lost optimistic-CAS race, an
			// outbox fingerprint mismatch and a unique-violation retry all
			// surface as ErrConflict. Hard-coding one attempt dead-lettered a
			// provider event on its first contention. The host's own budget
			// decides, and a stale claim's Fail remains a no-op regardless.
			attempts := in.MaxAttempts
			if attempts < 1 {
				attempts = 1
			}
			if failErr := s.inbox.Fail(ctx, claim, "fence-or-conflict", now.Add(in.RetryAfter), attempts); failErr != nil && !errors.Is(failErr, billing.ErrConflict) {
				return out, failErr
			}
		case RetryLater:
			out.Failed++
			retryAt := now.Add(in.RetryAfter)
			if errors.Is(err, ErrBackpressure) {
				s.emit(Event{Kind: EventUnknown, Account: claim.Message.Account, Value: int64(claim.Attempt), Reason: "backpressure"})
				if in.Backpressure > 0 {
					retryAt = now.Add(in.Backpressure)
				}
			}
			if failErr := s.inbox.Fail(ctx, claim, err.Error(), retryAt, in.MaxAttempts); failErr != nil {
				return out, failErr
			}
		}
	}
	s.emit(Event{Kind: EventQueueLag, Value: int64(out.Claimed), Worker: in.Worker})
	return out, nil
}

func (s *OpsService) Reconcile(ctx context.Context, in ReconcileInput) (ReconcileReport, error) {
	if !billing.ValidID(string(in.Account)) || s.credits == nil {
		return ReconcileReport{}, billing.ErrInvalid
	}
	report := ReconcileReport{Account: in.Account, DryRun: in.DryRun}
	audit, err := s.credits.VerifyLedger(ctx, in.Account)
	report.Audit = audit
	if err != nil {
		report.Drifts = append(report.Drifts, Drift{Kind: DriftLedger, Detail: err.Error()})
		s.emit(Event{Kind: EventDrift, Account: in.Account, Value: int64(len(report.Drifts)), Reason: "ledger"})
		if in.DryRun {
			return report, nil
		}
		return report, err
	}
	return report, nil
}

func (s *OpsService) RecordRepair(ctx context.Context, in ReconcileInput) (RepairRecord, error) {
	if !billing.ValidID(string(in.Account)) || !billing.ValidID(in.RepairID) || !billing.ValidID(in.Actor) || strings.TrimSpace(in.Reason) == "" || strings.TrimSpace(in.Evidence) == "" || in.ExpectedRevision < 1 {
		return RepairRecord{}, billing.ErrInvalid
	}
	now := billing.CanonicalTime(s.now())
	fp := identity.Fingerprint(string(in.Account), in.RepairID, strconv.FormatInt(in.ExpectedRevision, 10), in.Actor, in.Reason, in.Evidence)
	report, err := s.Reconcile(ctx, ReconcileInput{Account: in.Account, DryRun: true})
	if err != nil {
		return RepairRecord{}, err
	}
	record := RepairRecord{Account: in.Account, ID: in.RepairID, Revision: in.ExpectedRevision, Actor: in.Actor, Reason: in.Reason, Evidence: in.Evidence, Drifts: report.Drifts, CreatedAt: now}
	var out RepairRecord
	err = s.repo.WithinAccount(ctx, in.Account, func(tx OpsTx) error {
		old, err := tx.Repair(ctx, in.RepairID)
		if err == nil {
			oldFP := identity.Fingerprint(string(old.Account), old.ID, strconv.FormatInt(old.Revision, 10), old.Actor, old.Reason, old.Evidence)
			if oldFP != fp {
				return billing.ErrConflict
			}
			out = old
			return nil
		}
		if !errors.Is(err, billing.ErrNotFound) {
			return err
		}
		if err := tx.SaveRepair(ctx, record); err != nil {
			return err
		}
		out = record
		s.emit(Event{Kind: EventRecovery, Account: in.Account, Value: record.Revision, RepairID: in.RepairID, Actor: in.Actor})
		return nil
	})
	if err != nil {
		return RepairRecord{}, err
	}
	return out, nil
}

func (s *OpsService) AdvanceBackfill(ctx context.Context, in BackfillInput) (BackfillJob, error) {
	if !billing.ValidID(string(in.Account)) || !billing.ValidID(in.ID) || in.Limit < 1 || in.Limit > 1000 {
		return BackfillJob{}, billing.ErrInvalid
	}
	var out BackfillJob
	err := s.repo.WithinAccount(ctx, in.Account, func(tx OpsTx) error {
		job, err := tx.Backfill(ctx, in.ID)
		if errors.Is(err, billing.ErrNotFound) {
			job = BackfillJob{Account: in.Account, ID: in.ID, State: BackfillRunning}
		} else if err != nil {
			return err
		}
		if job.State == BackfillDone {
			out = job
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		job.Cursor += int64(in.Limit)
		job.Processed += int64(in.Limit)
		job.State = BackfillRunning
		if err := tx.SaveBackfill(ctx, job); err != nil {
			return err
		}
		out = job
		s.emit(Event{Kind: EventBackfill, Account: in.Account, Value: job.Processed, JobID: in.ID})
		return nil
	})
	if err != nil {
		return BackfillJob{}, err
	}
	return out, nil
}

func (s *OpsService) FinishBackfill(ctx context.Context, account billing.AccountID, id string) (BackfillJob, error) {
	var out BackfillJob
	err := s.repo.WithinAccount(ctx, account, func(tx OpsTx) error {
		job, err := tx.Backfill(ctx, id)
		if err != nil {
			return err
		}
		job.State = BackfillDone
		if err := tx.SaveBackfill(ctx, job); err != nil {
			return err
		}
		out = job
		return nil
	})
	if err != nil {
		return BackfillJob{}, err
	}
	return out, nil
}

func (s *OpsService) Archive(ctx context.Context, account billing.AccountID, period billing.Period) (OpsSnapshot, error) {
	if s.usage == nil || !period.Valid() {
		return OpsSnapshot{}, billing.ErrInvalid
	}
	snap := OpsSnapshot{Account: account}
	after := ""
	for {
		page, err := s.usage.UsagePage(ctx, account, period, after, 1000)
		if err != nil {
			return OpsSnapshot{}, err
		}
		for _, row := range page {
			stone := Tombstone{Kind: TombstoneUsage, ID: row.Observation.ID, Fingerprint: row.Fingerprint}
			if err := s.repo.WithinAccount(ctx, account, func(tx OpsTx) error {
				return tx.SaveTombstone(ctx, stone)
			}); err != nil {
				return OpsSnapshot{}, err
			}
			snap.Tombstones = append(snap.Tombstones, stone)
			snap.Usage = append(snap.Usage, usage.Copy(row))
			after = row.Observation.ID
		}
		if len(page) < 1000 {
			break
		}
	}
	return snap, nil
}

func (s *OpsService) Restore(ctx context.Context, snap OpsSnapshot) error {
	if !billing.ValidID(string(snap.Account)) {
		return billing.ErrInvalid
	}
	for _, stone := range snap.Tombstones {
		if !billing.ValidID(string(stone.Kind)) || !billing.ValidID(stone.ID) {
			return billing.ErrInvalid
		}
		if err := s.repo.WithinAccount(ctx, snap.Account, func(tx OpsTx) error {
			old, err := tx.Tombstone(ctx, string(stone.Kind), stone.ID)
			if err == nil {
				if old.Fingerprint != stone.Fingerprint {
					return billing.ErrConflict
				}
				return nil
			}
			if !errors.Is(err, billing.ErrNotFound) {
				return err
			}
			return tx.SaveTombstone(ctx, stone)
		}); err != nil {
			return err
		}
	}
	if s.usage == nil {
		return nil
	}
	for _, record := range snap.Usage {
		// Record routes purely by the record's own account, so a snapshot
		// carrying another tenant's rows would write billable usage into that
		// tenant while every tombstone landed here.
		if record.Observation.Account != snap.Account {
			return billing.ErrConflict
		}
		if _, err := s.usage.Record(ctx, record); err != nil {
			return err
		}
	}
	return nil
}

func (s *OpsService) OpsSnapshot(ctx context.Context, account billing.AccountID) (OpsSnapshot, error) {
	var out OpsSnapshot
	err := s.repo.WithinAccount(ctx, account, func(tx OpsTx) error {
		stones, err := tx.Tombstones(ctx, "", 1000)
		if err != nil {
			return err
		}
		out = OpsSnapshot{Account: account, Tombstones: stones}
		return nil
	})
	if err != nil {
		return OpsSnapshot{}, err
	}
	return out, nil
}
