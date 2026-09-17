package billingtest

import "time"

// Changing a budget requires a recorded decision and before/after evidence.
const WorkloadContractVersion = "2026-09-15"

const (
	WorkloadBalanceP95    = 50 * time.Millisecond
	WorkloadBalanceP99    = 150 * time.Millisecond
	WorkloadReserveP95    = 100 * time.Millisecond
	WorkloadReserveP99    = 250 * time.Millisecond
	WorkloadLockWaitP95   = 25 * time.Millisecond
	WorkloadClose100k     = 60 * time.Second
	WorkloadCloseChunk    = 2 * time.Second
	WorkloadClaimP95      = 100 * time.Millisecond
	WorkloadMixedDuration = 30 * time.Minute
	WorkloadOfferedOps    = 100
	WorkloadInFlight      = 32
	WorkloadClientPool    = 32
	WorkloadDatabasePool  = 32
	WorkloadHotLots       = 1000
	WorkloadHotHolds      = 100
	WorkloadClosePageSize = 500
	WorkloadQueueWorkers  = 10
	WorkloadQueuePageSize = 100
)
