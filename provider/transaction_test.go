package provider

import (
	"testing"
	"time"
)

func TestATransactionIsProratedByItsOriginOrALine(t *testing.T) {
	half := Proration{Rate: "0.5", PeriodStart: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), PeriodEnd: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
	for name, tt := range map[string]struct {
		tx   Transaction
		want bool
	}{
		"renewal":             {Transaction{Origin: OriginSubscriptionRecurring, Lines: []Line{{ID: "l1"}}}, false},
		"checkout":            {Transaction{Origin: OriginCheckout}, false},
		"subscription update": {Transaction{Origin: OriginSubscriptionUpdate}, true},
		"prorated line":       {Transaction{Origin: OriginAPI, Lines: []Line{{ID: "l1"}, {ID: "l2", Proration: &half}}}, true},
	} {
		if got := tt.tx.Prorated(); got != tt.want {
			t.Errorf("%s: prorated %v, want %v", name, got, tt.want)
		}
	}
}
