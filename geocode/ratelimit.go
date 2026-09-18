package geocode

import (
	"context"
	"sync"
	"time"
)

// RateLimited wraps a Provider so calls are spaced at least the configured
// interval apart, and honours ctx cancellation while it waits. A free-tier
// provider (Nominatim requires 1 request per second) requires this. A paid
// provider does not require it, but spacing calls has no cost when there is
// no backlog, and it avoids tripping the provider's own rate limit when
// there is a backlog.
type RateLimited struct {
	provider Provider
	mu       sync.Mutex
	interval time.Duration
	last     time.Time
	// now and sleep are overridable only from within this package's own
	// tests, by constructing a RateLimited literal directly; production
	// callers always get the real clock through NewRateLimited.
	now   func() time.Time
	sleep func(time.Duration) <-chan time.Time
}

// NewRateLimited wraps p so calls are spaced at least interval apart.
func NewRateLimited(p Provider, interval time.Duration) *RateLimited {
	return &RateLimited{provider: p, interval: interval, now: time.Now, sleep: time.After}
}

// Geocode implements Provider.
func (r *RateLimited) Geocode(ctx context.Context, address, city, state, postalCode string) (*Coordinates, error) {
	if err := r.wait(ctx); err != nil {
		return nil, err
	}
	return r.provider.Geocode(ctx, address, city, state, postalCode)
}

func (r *RateLimited) wait(ctx context.Context) error {
	r.mu.Lock()
	now := r.now()
	var wait time.Duration
	if elapsed := now.Sub(r.last); elapsed < r.interval {
		wait = r.interval - elapsed
	}
	reservedUntil := now.Add(wait)
	r.last = reservedUntil
	r.mu.Unlock()

	if wait <= 0 {
		return nil
	}
	select {
	case <-r.sleep(wait):
		return nil
	case <-ctx.Done():
		// This call never actually consumed the interval it reserved —
		// give it back, so a cancelled wait does not push out how long the
		// next real caller has to wait.
		r.release(reservedUntil)
		return ctx.Err()
	}
}

// release undoes a reservation made by wait that its caller abandoned via
// ctx cancellation, provided nothing has reserved a later slot since. If a
// later call has already reserved past this one, releasing would corrupt
// that reservation, so release leaves r.last alone in that case; the cost
// is bounded to at most one interval of extra wait, not compounding.
func (r *RateLimited) release(reservedUntil time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.last.Equal(reservedUntil) {
		r.last = reservedUntil.Add(-r.interval)
	}
}
