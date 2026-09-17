package credit

import (
	"context"
	"sync"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/internal/identity"
)

type memoryLimiter struct {
	mu     sync.Mutex
	counts map[string]int64
}

func NewMemoryLimiter() RateLimiter {
	return &memoryLimiter{counts: map[string]int64{}}
}

func (l *memoryLimiter) Allow(ctx context.Context, in RateLimitInput) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !billing.ValidID(in.Key) || !in.Window.Valid() || in.Limit <= 0 {
		return billing.ErrInvalid
	}
	id := identity.Fingerprint(in.Key, identity.Instant(billing.CanonicalTime(in.Window.Start)), identity.Instant(billing.CanonicalTime(in.Window.End)))
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.counts[id] >= in.Limit {
		return billing.ErrLimit
	}
	l.counts[id]++
	return nil
}
