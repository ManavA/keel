package geocode

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/ManavA/keel/metrics"
)

// defaultMissTTL is how long Cached remembers an address its Provider could
// not place. It is short next to a hit, which lives in the Store for as long
// as the Store keeps it: a miss may mean the Provider's data has a gap that
// closes soon, so a miss must not harden into a long-lived answer, only shed
// the repeat provider calls from a batch that contains a few bad addresses.
const defaultMissTTL = 5 * time.Minute

// CachedOptions configures Cached. The zero value logs cache errors through
// slog.Default() and remembers misses for defaultMissTTL.
type CachedOptions struct {
	// Logger receives a warning when the Store itself errors. A Store
	// failure does not fail the lookup, since caching is an optimization
	// rather than a dependency; the error is logged instead.
	Logger *slog.Logger
	// MissTTL is how long a nil result is remembered before the Provider is
	// asked again. Zero uses the default. A miss is kept only in memory,
	// never written to the Store, so it is not shared across replicas and
	// does not survive a restart.
	MissTTL time.Duration
	// Metrics receives one lookup count per Geocode call, with the source
	// that answered it and, when the lookup resolved, the precision tier.
	// Nil records nothing.
	Metrics *metrics.Metrics
}

func (o CachedOptions) withDefaults() CachedOptions {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.MissTTL <= 0 {
		o.MissTTL = defaultMissTTL
	}
	return o
}

// Cached wraps a Provider with a Store so a repeat lookup of the same
// address costs nothing. A hit lives in the Store for as long as the Store
// keeps it; a nil result is remembered in memory for MissTTL, so a batch
// containing a bad address pays one provider call per window instead of one
// per lookup. A Provider error is never cached.
type Cached struct {
	provider Provider
	store    Store
	opts     CachedOptions

	mu     sync.Mutex
	misses map[string]time.Time
	// now is the clock for miss expiry. It is overridable only from within
	// this package's own tests, by assigning after NewCached; production
	// callers always get the real clock. It mirrors the RateLimited pattern.
	now func() time.Time
}

// NewCached wraps provider with store.
func NewCached(provider Provider, store Store, opts CachedOptions) *Cached {
	return &Cached{
		provider: provider,
		store:    store,
		opts:     opts.withDefaults(),
		misses:   make(map[string]time.Time),
		now:      time.Now,
	}
}

// Geocode implements Provider.
func (c *Cached) Geocode(ctx context.Context, address, city, state, postalCode string) (*Coordinates, error) {
	key := NormalizeKey(address, city, state, postalCode)

	if coords, ok, err := c.store.Get(ctx, key); err != nil {
		c.opts.Logger.Warn("geocode: cache get failed", "error", err)
	} else if ok {
		result := coords
		c.forgetMiss(key)
		c.opts.Metrics.ObserveGeocode(ctx, metrics.GeocodeStore, result.Precision)
		return &result, nil
	}

	if c.isMiss(key) {
		c.opts.Metrics.ObserveGeocode(ctx, metrics.GeocodeMiss, "")
		return nil, nil
	}

	coords, err := c.provider.Geocode(ctx, address, city, state, postalCode)
	if err != nil {
		return nil, err
	}
	if coords == nil {
		c.recordMiss(key)
		c.opts.Metrics.ObserveGeocode(ctx, metrics.GeocodeProvider, "")
		return nil, nil
	}

	c.forgetMiss(key)
	if err := c.store.Set(ctx, key, *coords); err != nil {
		c.opts.Logger.Warn("geocode: cache set failed", "error", err)
	}
	c.opts.Metrics.ObserveGeocode(ctx, metrics.GeocodeProvider, coords.Precision)
	return coords, nil
}

// isMiss reports whether key was recorded as unresolvable within the miss
// window. An expired entry is dropped on read so the map does not hold onto
// addresses nobody asks about anymore.
func (c *Cached) isMiss(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	expiry, ok := c.misses[key]
	if !ok {
		return false
	}
	if !c.clock().Before(expiry) {
		delete(c.misses, key)
		return false
	}
	return true
}

// recordMiss remembers key as unresolvable for the miss window.
func (c *Cached) recordMiss(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.misses == nil {
		c.misses = make(map[string]time.Time)
	}
	c.misses[key] = c.clock().Add(c.missTTL())
}

// forgetMiss drops any miss entry for key, so an address that becomes
// resolvable — a Store hit, or a Provider hit now that its data improved —
// answers immediately instead of waiting out the window.
func (c *Cached) forgetMiss(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.misses, key)
}

func (c *Cached) missTTL() time.Duration {
	if c.opts.MissTTL > 0 {
		return c.opts.MissTTL
	}
	return defaultMissTTL
}

// clock is the miss-expiry clock, tolerating a Cached built without
// NewCached.
func (c *Cached) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}
