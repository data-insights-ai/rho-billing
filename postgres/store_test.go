package postgres

import (
	"errors"
	"reflect"
	"slices"
	"testing"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/catalog"
	"github.com/data-insights-ai/rho-billing/credit"
	"github.com/data-insights-ai/rho-billing/integration"
	"github.com/data-insights-ai/rho-billing/internal/pg"
	"github.com/data-insights-ai/rho-billing/purchase"
	"github.com/data-insights-ai/rho-billing/subscription"
	"github.com/data-insights-ai/rho-billing/usage"
)

var hostStoreMethods = []string{
	"Accounts",
	"Allowances",
	"Atomic",
	"Catalog",
	"CreateAccount",
	"CreditProjection",
	"CreditRepair",
	"Credits",
	"Entitlements",
	"Estimates",
	"Limits",
	"Migrate",
	"Ops",
	"Purchases",
	"Queue",
	"Ratings",
	"Settlements",
	"Subscriptions",
	"Usage",
}

func TestStorePublicMethodsAreHostPorts(t *testing.T) {
	typ := reflect.TypeFor[*Store]()
	got := make([]string, 0, typ.NumMethod())
	for i := range typ.NumMethod() {
		got = append(got, typ.Method(i).Name)
	}
	slices.Sort(got)
	want := slices.Clone(hostStoreMethods)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("postgres.Store methods = %q, want host ports %q", got, want)
	}
}

func TestStoreIsNotADomainRepository(t *testing.T) {
	store := New(nil)
	if _, ok := any(store).(credit.Repository); ok {
		t.Fatal("postgres.Store must not implement credit.Repository; use Credits()")
	}
	if _, ok := any(store).(integration.Repository); ok {
		t.Fatal("postgres.Store must not implement integration.Repository; use Queue()")
	}
	if _, ok := any(store).(usage.RatingStore); ok {
		t.Fatal("postgres.Store must not implement usage.RatingStore; use Ratings()")
	}
	if _, ok := any(store).(usage.Estimator); ok {
		t.Fatal("postgres.Store must not implement usage.Estimator; use Estimates()")
	}
	if _, ok := any(store).(catalog.Repository); ok {
		t.Fatal("postgres.Store must not implement catalog.Repository; use Catalog()")
	}
	if _, ok := any(store).(catalog.AccountRepository); ok {
		t.Fatal("postgres.Store must not implement catalog.AccountRepository; use Accounts()")
	}
	if _, ok := any(store).(purchase.Repository); ok {
		t.Fatal("postgres.Store must not implement purchase.Repository; use Purchases()")
	}
	if _, ok := any(store).(subscription.Repository); ok {
		t.Fatal("postgres.Store must not implement subscription.Repository; use Subscriptions()")
	}
	if store.Credits() == nil || store.Queue() == nil || store.Ratings() == nil || store.CreditRepair() == nil || store.CreditProjection() == nil {
		t.Fatal("accessors must return ports for a constructed store")
	}
	if _, ok := any(store.Catalog()).(credit.Repository); ok {
		t.Fatal("Catalog() must not be a credit repository")
	}
	if _, ok := any(store.Credits()).(integration.Repository); ok {
		t.Fatal("Credits() must not be a queue repository")
	}
	if _, ok := store.Catalog().(*pg.Store); ok {
		t.Fatal("Catalog() leaked *pg.Store")
	}
	if _, ok := store.Credits().(*pg.Store); ok {
		t.Fatal("Credits() leaked *pg.Store")
	}
	if _, ok := store.Queue().(*pg.Store); ok {
		t.Fatal("Queue() leaked *pg.Store")
	}
	if _, ok := store.Ratings().(*pg.Store); ok {
		t.Fatal("Ratings() leaked *pg.Store")
	}
}

func TestNewDoesNotMigrateOrRequireALiveDatabase(t *testing.T) {
	store := New(nil)
	if store == nil {
		t.Fatal("New(nil) returned nil")
	}
	if err := store.Migrate(t.Context()); !errors.Is(err, billing.ErrInvalid) {
		t.Fatalf("Migrate() = %v, want invalid without a database", err)
	}
	if NewWithClock(nil, nil) == nil {
		t.Fatal("NewWithClock returned nil")
	}
}
