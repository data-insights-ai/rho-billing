package provider_test

import (
	"errors"
	"testing"

	"github.com/data-insights-ai/rho-billing/provider"
)

func TestAChargeNamesExactlyOneOneTimePrice(t *testing.T) {
	usage := &provider.CustomItem{Name: "Overage", Amount: 250, Currency: "EUR", Quantity: 1}
	for name, r := range map[string]provider.ChargeRequest{
		"catalog": {Subscription: "sub_1", PriceID: "pri_1", Quantity: 3},
		"custom":  {Subscription: "sub_1", Custom: usage},
	} {
		if err := r.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	monthly := *usage
	monthly.Interval, monthly.Frequency = "month", 1
	for name, r := range map[string]provider.ChargeRequest{
		"no subscription": {PriceID: "pri_1", Quantity: 1},
		"no price":        {Subscription: "sub_1"},
		"both":            {Subscription: "sub_1", PriceID: "pri_1", Quantity: 1, Custom: usage},
		"recurring":       {Subscription: "sub_1", Custom: &monthly},
		"free":            {Subscription: "sub_1", Custom: &provider.CustomItem{Name: "Free", Currency: "EUR", Quantity: 1}},
	} {
		if err := r.Validate(); !errors.Is(err, provider.ErrInvalidItem) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
