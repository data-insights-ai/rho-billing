package provider_test

import (
	"errors"
	"testing"

	"github.com/data-insights-ai/rho-billing/provider"
)

func TestCustomItemValidate(t *testing.T) {
	ok := provider.CustomItem{Name: "Tier L", Amount: 1_000_000, Currency: "EUR", Interval: "month", Frequency: 1, Quantity: 1}
	oneTime := provider.CustomItem{Name: "Setup", Amount: 1_000_000, Currency: "EUR", Quantity: 1}
	for name, c := range map[string]provider.CustomItem{"recurring": ok, "one-time": oneTime} {
		if err := c.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	bad := map[string]func(*provider.CustomItem){
		"no name":        func(c *provider.CustomItem) { c.Name = "" },
		"zero amount":    func(c *provider.CustomItem) { c.Amount = 0 },
		"currency":       func(c *provider.CustomItem) { c.Currency = "EURO" },
		"no quantity":    func(c *provider.CustomItem) { c.Quantity = 0 },
		"interval":       func(c *provider.CustomItem) { c.Interval = "decade" },
		"zero frequency": func(c *provider.CustomItem) { c.Frequency = 0 },
		"frequency only": func(c *provider.CustomItem) { c.Interval = ""; c.Frequency = 3 },
	}
	for name, mutate := range bad {
		c := ok
		mutate(&c)
		if err := c.Validate(); !errors.Is(err, provider.ErrInvalidItem) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
