package billing

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/data-insights-ai/rho-billing/internal/checked"
	"github.com/data-insights-ai/rho-billing/internal/identity"
)

func TestFingerprintUnambiguous(t *testing.T) {
	if identity.Fingerprint("a|b", "c") == identity.Fingerprint("a", "b|c") {
		t.Fatal("ambiguous key")
	}
	if identity.Fingerprint("a", "") == identity.Fingerprint("a") {
		t.Fatal("empty field omitted")
	}
}
func TestAddOverflow(t *testing.T) {
	for _, pair := range [][2]int64{{math.MaxInt64, 1}, {math.MinInt64, -1}} {
		if _, err := checked.Add(pair[0], pair[1]); !errors.Is(err, ErrOverflow) {
			t.Fatal(err)
		}
	}
}
func TestPeriodBoundary(t *testing.T) {
	now := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)
	p := Period{now, now.Add(time.Hour)}
	if !p.Valid() || !p.Contains(now) || p.Contains(p.End) {
		t.Fatal(p)
	}
}

func TestCanonicalClockAcrossDSTAndMidnight(t *testing.T) {
	// These offset-bearing instants are the two occurrences of 02:30 at the
	// Brussels autumn transition; the billing clock must never merge them.
	first, err := time.Parse(time.RFC3339Nano, "2026-10-25T02:30:00.123456789+02:00")
	if err != nil {
		t.Fatal(err)
	}
	second, err := time.Parse(time.RFC3339Nano, "2026-10-25T02:30:00.123456789+01:00")
	if err != nil {
		t.Fatal(err)
	}
	if CanonicalTime(second).Sub(CanonicalTime(first)) != time.Hour {
		t.Fatal("DST repeated wall time lost distinct instants")
	}
	if identity.Instant(first) != identity.Instant(first.UTC()) {
		t.Fatal("zone changed operation identity")
	}
	midnight, err := time.Parse(time.RFC3339Nano, "2026-03-29T00:00:00+01:00")
	if err != nil {
		t.Fatal(err)
	}
	if got := CanonicalTime(midnight).Format(time.RFC3339); got != "2026-03-28T23:00:00Z" {
		t.Fatalf("midnight conversion %s", got)
	}
	period := Period{Start: CanonicalTime(first), End: CanonicalTime(second)}
	if !period.Contains(first.UTC()) || period.Contains(second.UTC()) {
		t.Fatal("DST half-open interval inconsistent")
	}
}
