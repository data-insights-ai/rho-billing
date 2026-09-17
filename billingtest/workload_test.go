package billingtest

import (
	"testing"
	"time"
)

func TestWorkloadContractIsVersionedAndDocumented(t *testing.T) {
	if WorkloadContractVersion != "2026-09-15" {
		t.Fatalf("WorkloadContractVersion = %q", WorkloadContractVersion)
	}
	if WorkloadBalanceP95 != 50*time.Millisecond || WorkloadBalanceP99 != 150*time.Millisecond {
		t.Fatalf("balance budgets p95=%s p99=%s", WorkloadBalanceP95, WorkloadBalanceP99)
	}
	if WorkloadReserveP95 != 100*time.Millisecond || WorkloadReserveP99 != 250*time.Millisecond {
		t.Fatalf("reserve budgets p95=%s p99=%s", WorkloadReserveP95, WorkloadReserveP99)
	}
	if WorkloadLockWaitP95 != 25*time.Millisecond || WorkloadClose100k != 60*time.Second {
		t.Fatalf("lock/close budgets lock=%s close=%s", WorkloadLockWaitP95, WorkloadClose100k)
	}
	if WorkloadDatabasePool != 32 || WorkloadInFlight != 32 || WorkloadOfferedOps != 100 {
		t.Fatalf("pool=%d in-flight=%d offered=%d", WorkloadDatabasePool, WorkloadInFlight, WorkloadOfferedOps)
	}
}
