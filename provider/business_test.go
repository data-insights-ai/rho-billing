package provider_test

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/data-insights-ai/rho-billing/provider"
)

func TestBusinessValidate(t *testing.T) {
	for name, b := range map[string]provider.Business{
		"name only":    {Name: "Hopper GmbH"},
		"with tax ID":  {Name: "Hopper GmbH", TaxID: "ATU12345678"},
		"long name ok": {Name: strings.Repeat("ä", 200)},
	} {
		if err := b.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, b := range map[string]provider.Business{
		"no name":          {TaxID: "ATU12345678"},
		"blank name":       {Name: "  "},
		"long name":        {Name: strings.Repeat("a", 201)},
		"control in name":  {Name: "a\nb"},
		"long tax ID":      {Name: "a", TaxID: strings.Repeat("1", 65)},
		"control in taxID": {Name: "a", TaxID: "AT\n1"},
	} {
		if err := b.Validate(); !errors.Is(err, provider.ErrInvalidBusiness) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A tax number never reaches a log line or formatted output.
func TestABusinessHidesItsTaxNumber(t *testing.T) {
	b := provider.Business{Name: "Hopper GmbH", TaxID: "ATU12345678"}
	var out bytes.Buffer
	slog.New(slog.NewJSONHandler(&out, nil)).Info("checkout", "business", b, "request", provider.CheckoutRequest{Business: &b})
	for _, s := range []string{out.String(), fmt.Sprint(b), fmt.Sprintf("%v %s %+v %+v", b, b, provider.CheckoutRequest{Business: &b}, []provider.Business{b})} {
		if strings.Contains(s, "ATU12345678") {
			t.Errorf("the tax number leaked: %s", s)
		}
	}
	if !strings.Contains(out.String(), "Hopper GmbH") {
		t.Errorf("the name is missing: %s", out.String())
	}
}
