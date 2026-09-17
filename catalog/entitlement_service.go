package catalog

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	billing "github.com/data-insights-ai/rho-billing"

	"github.com/data-insights-ai/rho-billing/internal/identity"
)

type Assignment struct {
	Account                  billing.AccountID
	Plan                     PlanAssignment
	SourceRef, Actor, Reason string
	CreatedAt                time.Time
}

type EntitlementRepository interface {
	WithinAccount(context.Context, billing.AccountID, func(EntitlementTx) error) error
}

type EntitlementTx interface {
	Assignment(context.Context, string) (Assignment, error)
	// AssignmentsPage lists an account's assignments in id order, starting
	// after the supplied id. Without it an assignment can only be fetched by an
	// id the host already knows, so "what does this account hold?" — the
	// question entitlement resolution exists to answer — is unanswerable.
	AssignmentsPage(ctx context.Context, after string, limit int) ([]Assignment, error)
	RevocationsPage(ctx context.Context, after string, limit int) ([]Revocation, error)
	InsertAssignment(context.Context, Assignment) error
	Revocation(context.Context, string) (Revocation, error)
	InsertRevocation(context.Context, Revocation) error
	Plan(context.Context, string) (PlanVersion, error)
}

type EntitlementService struct {
	repo EntitlementRepository
	now  func() time.Time
}

func NewEntitlement(repo EntitlementRepository, now func() time.Time) *EntitlementService {
	if repo == nil {
		panic("catalog: nil entitlement repository")
	}
	if now == nil {
		now = time.Now
	}
	return &EntitlementService{repo: repo, now: now}
}

func (a Assignment) Validate() error {
	if a.CreatedAt.IsZero() || !billing.ValidID(string(a.Account)) || !billing.ValidID(a.Plan.ID) || !a.Plan.Valid() || !billing.ValidID(a.SourceRef) || !billing.ValidID(a.Actor) || strings.TrimSpace(a.Reason) == "" || len(a.Reason) > 2000 {
		return billing.ErrInvalid
	}
	switch a.Plan.Source {
	case SourceManual, SourceFree, SourcePurchase:
	default:
		return billing.ErrInvalid
	}
	return nil
}

// Receipt time is excluded so exact retries recover the original assignment.
func (a Assignment) Fingerprint() string {
	return identity.Fingerprint("independent-assignment", string(a.Account), a.Plan.ID, a.Plan.PlanVersionID, strconv.FormatInt(a.Plan.Quantity, 10), identity.Instant(a.Plan.Effective.Start), identity.Instant(a.Plan.Effective.End), strconv.FormatBool(a.Plan.Perpetual), strconv.FormatInt(int64(a.Plan.Source), 10), a.SourceRef, a.Actor, a.Reason)
}

func (s *EntitlementService) Assign(ctx context.Context, in Assignment) (Assignment, error) {
	if err := ctx.Err(); err != nil {
		return Assignment{}, err
	}
	in.Plan.Effective.Start = billing.CanonicalTime(in.Plan.Effective.Start)
	in.Plan.Effective.End = billing.CanonicalTime(in.Plan.Effective.End)
	in.CreatedAt = billing.CanonicalTime(s.now())
	if in.CreatedAt.IsZero() {
		return Assignment{}, billing.ErrInvalid
	}
	if err := in.Validate(); err != nil {
		return Assignment{}, err
	}
	var out Assignment
	err := s.repo.WithinAccount(ctx, in.Account, func(tx EntitlementTx) error {
		old, err := tx.Assignment(ctx, in.Plan.ID)
		if err == nil {
			if err = old.Validate(); err != nil {
				return err
			}
			if old.Account != in.Account || old.Plan.ID != in.Plan.ID || old.Fingerprint() != in.Fingerprint() {
				return billing.ErrConflict
			}
			out = old
			return nil
		}
		if !errors.Is(err, billing.ErrNotFound) {
			return err
		}
		plan, err := tx.Plan(ctx, in.Plan.PlanVersionID)
		if err != nil {
			return err
		}
		if plan.ID != in.Plan.PlanVersionID || !ValidPlan(plan) {
			return billing.ErrState
		}
		if err = tx.InsertAssignment(ctx, in); err != nil {
			return err
		}
		out = in
		return nil
	})
	if err != nil {
		return Assignment{}, err
	}
	return out, nil
}

func (s *EntitlementService) Assignment(ctx context.Context, account billing.AccountID, id string) (Assignment, error) {
	if err := ctx.Err(); err != nil {
		return Assignment{}, err
	}
	if !billing.ValidID(string(account)) || !billing.ValidID(id) {
		return Assignment{}, billing.ErrInvalid
	}
	var out Assignment
	err := s.repo.WithinAccount(ctx, account, func(tx EntitlementTx) error {
		value, err := tx.Assignment(ctx, id)
		if err != nil {
			return err
		}
		if value.Account != account || value.Plan.ID != id {
			return billing.ErrState
		}
		if err = value.Validate(); err != nil {
			return err
		}
		out = value
		return nil
	})
	if err != nil {
		return Assignment{}, err
	}
	return out, nil
}

// assignmentPageSize bounds one page of an account's entitlement history.
const assignmentPageSize = 500

// maxAssignments bounds the whole walk. An account beyond this has a modelling
// problem, and silently truncating its entitlements would understate what it
// holds, so it is an error rather than a partial answer.
const maxAssignments = 50_000

// Holdings is everything an account has been assigned and everything that has
// been revoked, which together are the input to ResolveEntitlements.
type Holdings struct {
	Assignments []PlanAssignment
	Revocations []Revocation
}

// Holdings lists an account's entitlement assignments and revocations.
func (s *EntitlementService) Holdings(ctx context.Context, account billing.AccountID) (Holdings, error) {
	if s == nil || s.repo == nil || !billing.ValidID(string(account)) {
		return Holdings{}, billing.ErrInvalid
	}
	var out Holdings
	err := s.repo.WithinAccount(ctx, account, func(tx EntitlementTx) error {
		after := ""
		for {
			page, err := tx.AssignmentsPage(ctx, after, assignmentPageSize)
			if err != nil {
				return err
			}
			for _, assignment := range page {
				if assignment.Account != account {
					return billing.ErrConflict
				}
				out.Assignments = append(out.Assignments, assignment.Plan)
				after = assignment.Plan.ID
			}
			if len(page) < assignmentPageSize {
				break
			}
			if len(out.Assignments) > maxAssignments {
				return billing.ErrState
			}
		}
		after = ""
		for {
			page, err := tx.RevocationsPage(ctx, after, assignmentPageSize)
			if err != nil {
				return err
			}
			for _, revocation := range page {
				out.Revocations = append(out.Revocations, revocation)
				after = revocation.AssignmentID
			}
			if len(page) < assignmentPageSize {
				break
			}
			if len(out.Revocations) > maxAssignments {
				return billing.ErrState
			}
		}
		return nil
	})
	if err != nil {
		return Holdings{}, err
	}
	return out, nil
}
