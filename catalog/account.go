package catalog

import (
	"context"

	billing "github.com/data-insights-ai/rho-billing"
)

type Account struct {
	ID      billing.AccountID
	Subject string
}
type AccountReference struct {
	Account billing.AccountID
	Ref     billing.Reference
	Kind    string
}

func (r AccountReference) Valid() bool {
	return billing.ValidID(string(r.Account)) && r.Ref.Valid() && billing.ValidID(r.Kind)
}

type AccountRepository interface {
	CreateAccount(context.Context, billing.AccountID, string) error
	Account(context.Context, billing.AccountID) (Account, error)
	Link(context.Context, AccountReference) error
	Resolve(context.Context, AccountReference) (Account, error)
}
