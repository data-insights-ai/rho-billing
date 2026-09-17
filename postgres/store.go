// Package postgres is the host Store constructor. SQL lives in internal/pg.
package postgres

import (
	"context"
	"database/sql"
	"time"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/catalog"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/integration"
	"github.com/data-insights-ai/rho-billing/internal/pg"
	"github.com/data-insights-ai/rho-billing/purchase"
	"github.com/data-insights-ai/rho-billing/subscription"
	"github.com/data-insights-ai/rho-billing/usage"
)

type Store struct {
	inner *pg.Store
}

type (
	repairStore      struct{ inner *pg.Store }
	projectionStore  struct{ inner *pg.Store }
	catalogPort      struct{ catalog.Repository }
	accountPort      struct{ catalog.AccountRepository }
	creditPort       struct{ credit.Repository }
	purchasePort     struct{ purchase.Repository }
	settlementPort   struct{ usage.SettlementRepository }
	allowancePort    struct{ credit.AllowanceRepository }
	subscriptionPort struct{ subscription.Repository }
	entitlementPort  struct{ catalog.EntitlementRepository }
	limitPort        struct{ credit.LimitRepository }
	usagePort        struct{ usage.Repository }
	queuePort        struct{ integration.Repository }
	opsPort          struct{ integration.OpsRepository }
	estimatePort     struct{ usage.Estimator }
	ratingPort       struct{ usage.RatingStore }
)

func New(db *sql.DB) *Store { return &Store{inner: pg.New(db)} }

func NewWithClock(db *sql.DB, clock func() time.Time) *Store {
	return &Store{inner: pg.NewWithClock(db, clock)}
}

func (s *Store) impl() *pg.Store {
	if s == nil {
		return nil
	}
	return s.inner
}

func port[T any](s *Store, get func(*pg.Store) T) T {
	var zero T
	inner := s.impl()
	if inner == nil {
		return zero
	}
	return get(inner)
}

func (s *Store) Migrate(ctx context.Context) error {
	return s.impl().Migrate(ctx)
}

func (s *Store) CreateAccount(ctx context.Context, account billing.AccountID, subject string) error {
	return s.impl().CreateAccount(ctx, account, subject)
}

func (s *Store) Atomic(ctx context.Context, account billing.AccountID, fn func(integration.Session) error) error {
	return s.impl().Atomic(ctx, account, fn)
}

func (s *Store) Credits() credit.Repository {
	return port(s, func(inner *pg.Store) credit.Repository { return creditPort{inner.Credits()} })
}

func (s *Store) Purchases() purchase.Repository {
	return port(s, func(inner *pg.Store) purchase.Repository { return purchasePort{inner.Purchases()} })
}

func (s *Store) Settlements() usage.SettlementRepository {
	return port(s, func(inner *pg.Store) usage.SettlementRepository { return settlementPort{inner.Settlements()} })
}

func (s *Store) Allowances() credit.AllowanceRepository {
	return port(s, func(inner *pg.Store) credit.AllowanceRepository { return allowancePort{inner.Allowances()} })
}

func (s *Store) Subscriptions() subscription.Repository {
	return port(s, func(inner *pg.Store) subscription.Repository { return subscriptionPort{inner.Subscriptions()} })
}

func (s *Store) Entitlements() catalog.EntitlementRepository {
	return port(s, func(inner *pg.Store) catalog.EntitlementRepository { return entitlementPort{inner.Entitlements()} })
}

func (s *Store) Limits() credit.LimitRepository {
	return port(s, func(inner *pg.Store) credit.LimitRepository { return limitPort{inner.Limits()} })
}

func (s *Store) Usage() usage.Repository {
	return port(s, func(inner *pg.Store) usage.Repository { return usagePort{inner.UsageRepository()} })
}

func (s *Store) Queue() integration.Repository {
	return port(s, func(inner *pg.Store) integration.Repository { return queuePort{inner.Queue()} })
}

func (s *Store) Ops() integration.OpsRepository {
	return port(s, func(inner *pg.Store) integration.OpsRepository { return opsPort{inner.Ops()} })
}

func (s *Store) Catalog() catalog.Repository {
	return port(s, func(inner *pg.Store) catalog.Repository { return catalogPort{inner} })
}

func (s *Store) Accounts() catalog.AccountRepository {
	return port(s, func(inner *pg.Store) catalog.AccountRepository { return accountPort{inner} })
}

func (s *Store) Estimates() usage.Estimator {
	return port(s, func(inner *pg.Store) usage.Estimator { return estimatePort{inner} })
}

func (s *Store) Ratings() usage.RatingStore {
	return port(s, func(inner *pg.Store) usage.RatingStore { return ratingPort{inner} })
}

func (s *Store) CreditRepair() credit.RepairStore {
	return port(s, func(inner *pg.Store) credit.RepairStore { return repairStore{inner: inner} })
}

func (s *Store) CreditProjection() credit.ProjectionStore {
	return port(s, func(inner *pg.Store) credit.ProjectionStore { return projectionStore{inner: inner} })
}

func (r repairStore) Status(ctx context.Context, account billing.AccountID, id string) (credit.RepairStatus, error) {
	return r.inner.CreditRepair(ctx, account, id)
}

func (r repairStore) Start(ctx context.Context, in credit.RepairRequest) (credit.RepairStatus, error) {
	return r.inner.StartCreditRepair(ctx, in)
}

func (r repairStore) Apply(ctx context.Context, account billing.AccountID, id string, revision int64) (credit.RepairStatus, error) {
	return r.inner.ApplyCreditRepair(ctx, account, id, revision)
}

func (r repairStore) Advance(ctx context.Context, account billing.AccountID, id string, revision int64, limit int) (credit.RepairStatus, error) {
	return r.inner.AdvanceCreditRepair(ctx, account, id, revision, limit)
}

func (r repairStore) EvidencePage(ctx context.Context, account billing.AccountID, id, after string, limit int) ([]credit.RepairEvidence, string, bool, error) {
	return r.inner.CreditRepairEvidencePage(ctx, account, id, after, limit)
}

func (p projectionStore) Rebuild(ctx context.Context, account billing.AccountID, limit int) (bool, error) {
	return p.inner.BuildCreditBalanceProjection(ctx, account, limit)
}

func (p projectionStore) Stored(ctx context.Context, account billing.AccountID, unit, scope string) (credit.Balance, error) {
	return p.inner.StoredCreditBalance(ctx, account, unit, scope)
}

var (
	_ credit.RepairStore        = repairStore{}
	_ credit.ProjectionStore    = projectionStore{}
	_ usage.RatingStore         = (*pg.Store)(nil)
	_ usage.Estimator           = (*pg.Store)(nil)
	_ catalog.Repository        = (*pg.Store)(nil)
	_ catalog.AccountRepository = (*pg.Store)(nil)
	_ integration.Repository    = (*pg.Store)(nil)
	_ integration.UnitOfWork    = (*Store)(nil)
)
