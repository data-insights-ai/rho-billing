package host_test

import (
	"testing"
	"time"

	"github.com/data-insights-ai/rho-billing/integration"
)

func TestHostConstructsOpsWorkerWithoutStartingIt(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	svc := integration.NewOps(integration.NewMemoryOpsRepository("host-ops"), func() time.Time { return now })
	if svc == nil {
		t.Fatal("nil ops service")
	}
	if integration.Redact("token=abc") != "[redacted]" {
		t.Fatal("host redaction helper missing")
	}
}
