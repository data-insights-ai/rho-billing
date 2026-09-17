package pg

import (
	"fmt"
	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/credit"
	"testing"
	"time"
)

// These benchmarks measure current account-history read cost, not provider
// throughput. Setup creates valid journal/projection rows outside timing.
func BenchmarkCreditBalanceHistory(b *testing.B) {
	for _, backend := range []string{"memory", "postgres"} {
		for _, size := range []int{100, 1000} {
			b.Run(fmt.Sprintf("%s/lots-%d", backend, size), func(b *testing.B) {
				var repo credit.Repository
				if backend == "memory" {
					repo = credit.NewMemoryRepository("bench-account")
				} else {
					store, _ := testStore(b)
					if err := store.CreateAccount(b.Context(), "bench-account", "bench-subject"); err != nil {
						b.Fatal(err)
					}
					repo = store
				}
				now := testTime()
				if err := repo.WithinAccount(b.Context(), "bench-account", func(tx credit.Tx) error {
					for i := range size {
						id := fmt.Sprintf("lot-%05d", i)
						lot := credit.Lot{ID: id, Unit: billing.Unit{Code: "credits", Scale: 1}, Source: "benchmark", SourceRef: id, ValidFrom: now, GrantedAt: now, ExpiresAt: now.Add(time.Hour), Initial: 100, Available: 100}
						if err := tx.PutLot(lot); err != nil {
							return err
						}
						if err := tx.Append(credit.Entry{OperationID: billing.OperationID(id), LotID: id, Kind: "grant", RecordedAt: now, EffectiveAt: now, Available: 100}); err != nil {
							return err
						}
					}
					return nil
				}); err != nil {
					b.Fatal(err)
				}
				engine := credit.New(repo, testTime)
				b.ReportAllocs()
				for b.Loop() {
					balance, err := engine.Balance(b.Context(), "bench-account", "credits", "")
					if err != nil || balance.Available != int64(size*100) {
						b.Fatalf("balance %+v %v", balance, err)
					}
				}
			})
		}
	}
}
