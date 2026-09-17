package integration_test

import (
	"testing"

	"github.com/data-insights-ai/rho-billing/integration"
)

func TestProviderMessageIdentityScope(t *testing.T) {
	seen := make(map[string]bool)
	for _, scope := range [][3]string{{"paddle", "one", "sandbox"}, {"paddle", "one", "production"}, {"paddle", "two", "sandbox"}, {"other", "one", "sandbox"}} {
		id, err := integration.ProviderMessageID(scope[0], scope[1], scope[2], "event-1")
		if err != nil || seen[id] {
			t.Fatalf("identity collision or invalid scope: %v", err)
		}
		seen[id] = true
		replay, err := integration.ProviderMessageID(scope[0], scope[1], scope[2], "event-1")
		if err != nil || replay != id {
			t.Fatal("unstable scoped identity")
		}
	}
	if _, err := integration.ProviderMessageID("paddle", "", "sandbox", "event-1"); err == nil {
		t.Fatal("accepted unscoped external identity")
	}
}
