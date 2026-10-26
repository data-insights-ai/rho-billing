package provider

import (
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ErrInvalidBusiness: a business without a name, or with a name or tax
// number the provider would refuse.
var ErrInvalidBusiness = errors.New("provider: a business needs a name of at most 200 characters and a tax number of at most 64")

// Business is the company a checkout bills, so the invoice names it and,
// with a tax number, can be a reverse-charge invoice.
type Business struct {
	Name string
	// TaxID is the business's VAT or tax number, if it has one. It is
	// personal data for sole traders: plugins and hosts never log it.
	TaxID string
}

// Validate checks the name and the tax number's shape; whether the number
// is registered is the host's or the provider's check.
func (b Business) Validate() error {
	name := strings.TrimSpace(b.Name)
	switch {
	case name == "" || utf8.RuneCountInString(name) > 200 || strings.ContainsFunc(name, unicode.IsControl):
		return ErrInvalidBusiness
	case len(b.TaxID) > 64 || strings.ContainsFunc(b.TaxID, unicode.IsControl):
		return ErrInvalidBusiness
	}
	return nil
}

// LogValue keeps the tax number out of logs.
func (b Business) LogValue() slog.Value {
	return slog.GroupValue(slog.String("name", b.Name), slog.Bool("tax_id", b.TaxID != ""))
}

// String keeps the tax number out of formatted output.
func (b Business) String() string {
	if b.TaxID == "" {
		return b.Name
	}
	return b.Name + " (tax number on file)"
}

// MarshalJSON keeps the tax number out of JSON, where loggers and error
// reports put structs; plugins build the provider's request themselves.
func (b Business) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Name  string `json:"name"`
		TaxID bool   `json:"taxId"`
	}{b.Name, b.TaxID != ""})
}
