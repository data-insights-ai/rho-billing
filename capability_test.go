package billing

import (
	"errors"
	"testing"
)

func TestRequireSupportedFailsClosedForZeroAndInvalidCapabilities(t *testing.T) {
	for _, capability := range []Capability{
		{},
		{Operation: "checkout", Support: Support("unknown"), Reason: "bad status"},
		{Operation: "checkout", Support: SupportUnsupported},
		{Operation: "checkout", Support: SupportUnsupported, Reason: "contains\ncontrol"},
	} {
		if err := RequireSupported(capability); !errors.Is(err, ErrInvalid) {
			t.Fatalf("RequireSupported(%+v) = %v, want invalid", capability, err)
		}
	}
}

func TestRequireSupportedAllowsSupported(t *testing.T) {
	if err := RequireSupported(Capability{Operation: "checkout", Support: SupportSupported}); err != nil {
		t.Fatalf("supported capability rejected: %v", err)
	}
}

func TestOptionalCapabilitiesFailClosedWithTypedResults(t *testing.T) {
	for _, capability := range []Capability{
		{Operation: "member_allowance_issuance", Support: SupportUnsupported, Reason: "member_scope_requires_actor_policy"},
		{Operation: "named_zone_recurrence", Support: SupportUnsupported, Reason: "utc_microsecond_instants_only"},
		{Operation: "automatic_top_up", Support: SupportUnsupported, Reason: "requires_consent_and_collection_capability"},
		{Operation: "process_adjustment_event", Support: SupportUnresolved, Reason: "original_adjustment_lookup_required"},
	} {
		err := RequireSupported(capability)
		var capabilityErr *CapabilityError
		if !errors.As(err, &capabilityErr) {
			t.Fatalf("RequireSupported(%+v) = %v, want CapabilityError", capability, err)
		}
		if capabilityErr.Support == SupportSupported || capabilityErr.Reason == "" {
			t.Fatalf("optional capability leaked as supported: %+v", capabilityErr.Capability)
		}
	}
}

func TestRequireSupportedPreservesFailureStatusAndReason(t *testing.T) {
	for _, support := range []Support{SupportActionRequired, SupportUnsupported, SupportUnresolved} {
		want := Capability{Operation: "checkout", Support: support, Reason: "provider policy"}
		err := RequireSupported(want)
		var capabilityErr *CapabilityError
		if !errors.As(err, &capabilityErr) {
			t.Fatalf("RequireSupported(%+v) = %v, want CapabilityError", want, err)
		}
		if capabilityErr.Operation != want.Operation || capabilityErr.Support != want.Support || capabilityErr.Reason != want.Reason {
			t.Fatalf("capability error = %+v, want %+v", capabilityErr.Capability, want)
		}
	}
}
