package provider_test

import (
	"testing"

	"github.com/data-insights-ai/rho-billing/provider"
)

func TestOnlyApprovedRefundsCreditsAndChargebacksReturnMoney(t *testing.T) {
	for _, c := range []struct {
		action provider.AdjustmentAction
		status provider.AdjustmentStatus
		want   bool
	}{
		{provider.Refund, provider.AdjustmentApproved, true},
		{provider.Credit, provider.AdjustmentApproved, true},
		{provider.Chargeback, provider.AdjustmentApproved, true},
		{provider.Refund, provider.AdjustmentPending, false},
		{provider.Refund, provider.AdjustmentRejected, false},
		{provider.ChargebackWarning, provider.AdjustmentApproved, false},
		{provider.ChargebackReverse, provider.AdjustmentApproved, false},
		{provider.CreditReverse, provider.AdjustmentApproved, false},
	} {
		a := provider.Adjustment{Action: c.action, Status: c.status}
		if got := a.ReturnsMoney(); got != c.want {
			t.Errorf("%s %s: ReturnsMoney %v, want %v", c.action, c.status, got, c.want)
		}
	}
	if !(provider.Adjustment{Action: provider.ChargebackReverse, Status: provider.AdjustmentApproved}).Reverses() {
		t.Error("an approved chargeback reversal does not reverse")
	}
}
