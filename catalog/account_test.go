package catalog

import (
	"errors"
	billing "github.com/data-insights-ai/rho-billing"
	"testing"
)

func TestProviderReferenceOwnership(t *testing.T) {
	r := NewMemoryAccountRepository()
	ctx := t.Context()
	if err := r.CreateAccount(ctx, "a", "org-a"); err != nil {
		t.Fatal(err)
	}
	if err := r.CreateAccount(ctx, "b", "org-b"); err != nil {
		t.Fatal(err)
	}
	ref := AccountReference{Account: "a", Ref: billing.Reference{Scope: billing.Scope{Provider: "sim", Merchant: "merchant", Environment: "sandbox"}, ID: "customer-1"}, Kind: "customer"}
	if err := r.Link(ctx, ref); err != nil {
		t.Fatal(err)
	}
	ref.Account = "b"
	if err := r.Link(ctx, ref); !errors.Is(err, billing.ErrConflict) {
		t.Fatal(err)
	}
	if _, err := r.Resolve(ctx, ref); !errors.Is(err, billing.ErrNotFound) {
		t.Fatal(err)
	}
	ref.Ref.Scope.Environment = "production"
	if err := r.Link(ctx, ref); err != nil {
		t.Fatal(err)
	}
}
