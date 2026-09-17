package purchase

import (
	"errors"
	"testing"

	billing "github.com/data-insights-ai/rho-billing"
)

func TestDisputeRecoveryPreservesGrossTaxAndNetCaps(t *testing.T) {
	original := []PaidLine{{LineID: "item", Gross: 120, Tax: 20}}
	prior := []PaidLine{{LineID: "item", Gross: 60, Tax: 5}}
	for _, tc := range []struct {
		name string
		line PaidLine
		want error
	}{
		{"exact remainder", PaidLine{LineID: "item", Gross: 60, Tax: 15}, nil},
		{"gross exceeded", PaidLine{LineID: "item", Gross: 61, Tax: 15}, billing.ErrConflict},
		{"tax exceeded", PaidLine{LineID: "item", Gross: 60, Tax: 16}, billing.ErrConflict},
		{"net exceeded within gross and tax", PaidLine{LineID: "item", Gross: 60, Tax: 14}, billing.ErrConflict},
		{"foreign line", PaidLine{LineID: "other", Gross: 1}, billing.ErrConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateRecoveryLines([]PaidLine{tc.line}, original, prior); !errors.Is(err, tc.want) {
				t.Fatalf("recovery error=%v, want %v", err, tc.want)
			}
		})
	}
}
