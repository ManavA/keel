package geocode

import (
	"context"
	"sync"
	"time"
)

// RateLimitedOptions configures RateLimited beyond the interval passed to
// NewRateLimited. The zero value uses the real clock.
type RateLimitedOptions struct {
	// now and sleep are overridable for tests; production callers never set
	// them.
	now   func() time.Time
	sleep func(time.Duration) <-chan time.Time
}

func (o RateLimitedOptions) withDefaults() RateLimitedOptions {
	if o.now == nil {
		o.now = time.Now
	}
	if o.sleep == nil {
		o.sleep = time.After
	}
	return o
}

// RateLimited wraps a Provider so calls are spaced at least the configured
// interval apart, and honours ctx cancellation while it waits. A free-tier
// provider (Nominatim requires 1 request per second) requires this. A paid
// provider does not require it, but spacing calls has no cost when there is
// no backlog, and it avoids tripping the provider's own rate limit when
// there is a backlog.
type RateLimited struct {
	Provider
	mu       sync.Mutex
	interval time.Duration
	last     time.Time
	opts     RateLimitedOptions
}

// NewRateLimited wraps p so calls are spaced at least interval apart.
func NewRateLimited(p Provider, interval time.Duration, opts RateLimitedOptions) *RateLimited {
	return &RateLimited{Provider: p, interval: interval, opts: opts.withDefaults()}
}

// Geocode implements Provider.
func (r *RateLimited) Geocode(ctx context.Context, address, city, state, postalCode string) (*Coordinates, error) {
	if err := r.wait(ctx); err != nil {
		return nil, err
	}
	return r.Provider.Geocode(ctx, address, city, state, postalCode)
}

func (r *RateLimited) wait(ctx context.Context) error {
	r.mu.Lock()
	now := r.opts.now()
	var wait time.Duration
	if elapsed := now.Sub(r.last); elapsed < r.interval {
		wait = r.interval - elapsed
	}
	r.last = now.Add(wait)
	r.mu.Unlock()

	if wait <= 0 {
		return nil
	}
	select {
	case <-r.opts.sleep(wait):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
