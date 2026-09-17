package credit

import (
	"context"
	"fmt"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/internal/checked"
	"github.com/data-insights-ai/rho-billing/internal/creditledger"
)

type AuditReport struct {
	Lots, Reservations int
	Entries            int64
}

func (e *Engine) VerifyLedger(ctx context.Context, account billing.AccountID) (AuditReport, error) {
	var report AuditReport
	err := e.repo.WithinAccount(ctx, account, func(tx Tx) error {
		reconstructed := map[string]creditledger.State{}
		var after int64
		for {
			page, err := tx.History(after, 1000)
			if err != nil {
				return err
			}
			for _, entry := range page {
				if entry.Sequence != after+1 {
					return fmt.Errorf("%w: journal sequence gap", billing.ErrState)
				}
				after = entry.Sequence
				if entry.LotID == "" {
					return fmt.Errorf("%w: journal lot missing", billing.ErrState)
				}
				p, err := creditledger.Apply(reconstructed[entry.LotID], creditledger.Delta{Kind: entry.Kind, Available: entry.Available, Held: entry.Held, Consumed: entry.Consumed, Expired: entry.Expired, Revoked: entry.Revoked, PendingRevocation: entry.PendingRevocation})
				if err != nil {
					return fmt.Errorf("%w for lot %s", err, entry.LotID)
				}
				reconstructed[entry.LotID] = p
			}
			if len(page) < 1000 {
				break
			}
		}
		lots, err := tx.Lots()
		if err != nil {
			return err
		}
		for _, lot := range lots {
			p, ok := reconstructed[lot.ID]
			b := Balance{Available: lot.Available, Held: lot.Held, Consumed: lot.Consumed, Expired: lot.Expired, Revoked: lot.Revoked}
			projected := Balance{Available: p.Available, Held: p.Held, Consumed: p.Consumed, Expired: p.Expired, Revoked: p.Revoked}
			if !ok || p.Granted != lot.Initial || projected != b || p.PendingRevocation != lot.PendingRevocation {
				return fmt.Errorf("%w: projection drift for lot %s", billing.ErrState, lot.ID)
			}
		}
		if len(lots) != len(reconstructed) {
			return fmt.Errorf("%w: missing lot projection", billing.ErrState)
		}
		reservations, err := tx.Reservations()
		if err != nil {
			return err
		}
		held := map[string]int64{}
		for _, r := range reservations {
			var allocated int64
			for _, a := range r.Allocations {
				if _, ok := reconstructed[a.LotID]; !ok || a.Amount <= 0 {
					return fmt.Errorf("%w: invalid reservation allocation", billing.ErrState)
				}
				allocated, err = checked.Add(allocated, a.Amount)
				if err != nil {
					return err
				}
				if r.State == "held" {
					held[a.LotID], err = checked.Add(held[a.LotID], a.Amount)
					if err != nil {
						return err
					}
				}
			}
			if allocated != r.Authorized {
				return fmt.Errorf("%w: reservation allocation drift", billing.ErrState)
			}
		}
		for id, p := range reconstructed {
			if held[id] != p.Held {
				return fmt.Errorf("%w: held allocation drift for lot %s", billing.ErrState, id)
			}
		}
		report = AuditReport{Lots: len(lots), Reservations: len(reservations), Entries: after}
		return nil
	})
	if err != nil {
		return AuditReport{}, err
	}
	return report, nil
}
