package pg

import (
	"errors"
	"testing"

	billing "github.com/data-insights-ai/rho-billing"
)

func TestNewDoesNotMigrateOrRequireALiveDatabase(t *testing.T) {
	store := New(nil)
	if store == nil {
		t.Fatal("New(nil) returned nil")
	}
	if err := store.Migrate(t.Context()); !errors.Is(err, billing.ErrInvalid) {
		t.Fatalf("Migrate() = %v, want invalid without a database", err)
	}
	if got := NewWithClock(nil, nil); got == nil || got.clock == nil {
		t.Fatal("NewWithClock did not install a clock")
	}
}
