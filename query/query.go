// Package query composes domain services and does not own financial mutations.
package query

import (
	"context"
	"errors"
	"strconv"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/catalog"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/purchase"
	"github.com/data-insights-ai/rho-billing/subscription"
	"github.com/data-insights-ai/rho-billing/usage"
)

// Cursor is bound to one snapshot. A later page with a different snapshot is a conflict.
type Cursor struct {
	Snapshot time.Time
	After    string
}

// CustomerOverview has no internal cost.
type CustomerOverview struct {
	Account        catalog.Account
	AsOf           time.Time
	Balance        credit.Balance
	PendingActions []PendingAction
}

// Hosts must authorize OperatorOverview separately from customer reads.
type OperatorOverview struct {
	CustomerOverview
	Costs []usage.CostRecord
}

type ActionKind string

const ActionHeldCredits ActionKind = "held_credits"

type ActionState string

const ActionHeld ActionState = "held"

type PendingAction struct {
	Kind  ActionKind
	ID    string
	State ActionState
}

type UsagePage struct {
	Snapshot  time.Time
	Period    billing.Period
	Records   []usage.Record
	NextAfter string
}

type Statement struct {
	Account    billing.AccountID
	BatchID    string
	Currency   string
	Total      int64
	State      usage.BatchState
	Lines      []usage.ChargeLine
	AsOf       time.Time
	Pending    bool
	Incomplete bool
	Watermark  string
}

type Deps struct {
	Accounts     catalog.AccountRepository
	Credits      *credit.Engine
	Usage        *usage.Service
	Estimates    usage.Estimator
	Purchases    *purchase.Service
	Entitlements *catalog.EntitlementService
	Settlements  *usage.SettlementService
	// Subscriptions and Catalog back Entitlements, the read model a host uses
	// on the request path to decide what an account may do.
	Subscriptions *subscription.Service
	Catalog       catalog.Repository
	CreditUnit    string
}

type Service struct {
	deps Deps
	now  func() time.Time
}

func New(deps Deps, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{deps: deps, now: now}
}

func (s *Service) CustomerOverview(ctx context.Context, id billing.AccountID) (CustomerOverview, error) {
	if err := ctx.Err(); err != nil {
		return CustomerOverview{}, err
	}
	acct, err := s.deps.Accounts.Account(ctx, id)
	if err != nil {
		return CustomerOverview{}, err
	}
	asOf := billing.CanonicalTime(s.now())
	out := CustomerOverview{Account: acct, AsOf: asOf}
	if s.deps.Credits != nil && s.deps.CreditUnit != "" {
		bal, err := s.deps.Credits.Balance(ctx, id, s.deps.CreditUnit, "")
		if err != nil {
			return CustomerOverview{}, err
		}
		out.Balance = bal
	}
	if s.deps.Credits != nil {
		held, err := s.deps.Credits.ActiveReservations(ctx, id)
		if err != nil {
			return CustomerOverview{}, err
		}
		for _, reservation := range held {
			if reservation.State != credit.ReservationHeld {
				continue
			}
			out.PendingActions = append(out.PendingActions, PendingAction{Kind: ActionHeldCredits, ID: reservation.ID, State: ActionHeld})
		}
	}
	return out, nil
}

func (s *Service) OperatorOverview(ctx context.Context, id billing.AccountID, costIDs []string) (OperatorOverview, error) {
	customer, err := s.CustomerOverview(ctx, id)
	if err != nil {
		return OperatorOverview{}, err
	}
	out := OperatorOverview{CustomerOverview: customer}
	if s.deps.Usage == nil {
		return out, nil
	}
	for _, costID := range costIDs {
		cost, err := s.deps.Usage.Cost(ctx, id, costID)
		if err != nil {
			return OperatorOverview{}, err
		}
		out.Costs = append(out.Costs, cost)
	}
	return out, nil
}

func (s *Service) UsagePage(ctx context.Context, account billing.AccountID, period billing.Period, cursor Cursor, limit int) (UsagePage, error) {
	if s.deps.Usage == nil || !period.Valid() || limit < 1 || limit > 1000 {
		return UsagePage{}, billing.ErrInvalid
	}
	if cursor.After != "" && cursor.Snapshot.IsZero() {
		return UsagePage{}, billing.ErrInvalid
	}
	snapshot := billing.CanonicalTime(cursor.Snapshot)
	if snapshot.IsZero() {
		snapshot = billing.CanonicalTime(s.now())
	}
	after := cursor.After
	out := UsagePage{Snapshot: snapshot, Period: period}
	for {
		rows, err := s.deps.Usage.UsagePage(ctx, account, period, after, limit)
		if err != nil {
			return UsagePage{}, err
		}
		if len(rows) == 0 {
			return out, nil
		}
		lastFetched := rows[len(rows)-1].Observation.ID
		for _, row := range rows {
			if row.ReceivedAt.After(snapshot) {
				continue
			}
			out.Records = append(out.Records, row)
			out.NextAfter = row.Observation.ID
			if len(out.Records) == limit {
				return out, nil
			}
		}
		if len(rows) < limit {
			return out, nil
		}
		if lastFetched == "" || lastFetched == after {
			return UsagePage{}, billing.ErrState
		}
		after = lastFetched
	}
}

func (s *Service) Purchase(ctx context.Context, account billing.AccountID, intentID string) (purchase.Intent, error) {
	if s.deps.Purchases == nil {
		return purchase.Intent{}, billing.ErrInvalid
	}
	return s.deps.Purchases.Intent(ctx, account, intentID)
}

func (s *Service) Entitlement(ctx context.Context, account billing.AccountID, assignmentID string) (catalog.Assignment, error) {
	if s.deps.Entitlements == nil {
		return catalog.Assignment{}, billing.ErrInvalid
	}
	return s.deps.Entitlements.Assignment(ctx, account, assignmentID)
}

func (s *Service) Statement(ctx context.Context, account billing.AccountID, batchID string) (Statement, error) {
	if s.deps.Settlements == nil || !billing.ValidID(string(account)) || !billing.ValidID(batchID) {
		return Statement{}, billing.ErrInvalid
	}
	summary, err := s.deps.Settlements.BatchSummary(ctx, account, batchID)
	if err != nil {
		return Statement{}, err
	}
	var lines []usage.ChargeLine
	after := ""
	for {
		page, next, more, err := s.deps.Settlements.BatchLinesPage(ctx, account, batchID, after, 1000)
		if err != nil {
			return Statement{}, err
		}
		lines = append(lines, page...)
		if !more {
			break
		}
		if next == "" || next == after {
			return Statement{}, billing.ErrState
		}
		after = next
	}
	pending := summary.State != usage.BatchReady && summary.State != usage.BatchConfirmed
	incomplete := pending || int64(len(lines)) != summary.LineCount
	watermark := ""
	if job, err := s.deps.Settlements.CloseJob(ctx, account, batchID); err == nil {
		watermark = strconv.FormatInt(job.Watermark, 10)
	} else if !errors.Is(err, billing.ErrNotFound) {
		return Statement{}, err
	}
	return Statement{
		Account: account, BatchID: summary.ID, Currency: summary.Currency, Total: summary.Total,
		State: summary.State, Lines: lines, AsOf: billing.CanonicalTime(s.now()),
		Pending: pending, Incomplete: incomplete, Watermark: watermark,
	}, nil
}

func (s *Service) Estimate(ctx context.Context, in usage.EstimateInput) (usage.Estimate, error) {
	if s.deps.Estimates == nil {
		return usage.Estimate{}, billing.ErrInvalid
	}
	return s.deps.Estimates.EstimateUsage(ctx, in)
}

func (s *Service) OperatorCost(ctx context.Context, account billing.AccountID, id string) (usage.CostRecord, error) {
	if s.deps.Usage == nil {
		return usage.CostRecord{}, billing.ErrInvalid
	}
	return s.deps.Usage.Cost(ctx, account, id)
}
