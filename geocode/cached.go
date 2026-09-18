package geocode

import (
	"context"
	"log/slog"
)

// CachedOptions configures Cached. The zero value logs cache errors through
// slog.Default().
type CachedOptions struct {
	// Logger receives a warning when the Store itself errors. A Store
	// failure does not fail the lookup, since caching is an optimization
	// rather than a dependency; the error is logged instead.
	Logger *slog.Logger
}

func (o CachedOptions) withDefaults() CachedOptions {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return o
}

// Cached wraps a Provider with a Store so a repeat lookup of the same
// address costs nothing. It never caches a nil result: an address the
// Provider could not place is asked again next time, in case the Provider or
// its data has since improved, rather than being remembered as permanently
// unknown.
type Cached struct {
	provider Provider
	store    Store
	opts     CachedOptions
}

// NewCached wraps provider with store.
func NewCached(provider Provider, store Store, opts CachedOptions) *Cached {
	return &Cached{provider: provider, store: store, opts: opts.withDefaults()}
}

// Geocode implements Provider.
func (c *Cached) Geocode(ctx context.Context, address, city, state, postalCode string) (*Coordinates, error) {
	key := NormalizeKey(address, city, state, postalCode)

	if coords, ok, err := c.store.Get(ctx, key); err != nil {
		c.opts.Logger.Warn("geocode: cache get failed", "error", err)
	} else if ok {
		result := coords
		return &result, nil
	}

	coords, err := c.provider.Geocode(ctx, address, city, state, postalCode)
	if err != nil || coords == nil {
		return coords, err
	}

	if err := c.store.Set(ctx, key, *coords); err != nil {
		c.opts.Logger.Warn("geocode: cache set failed", "error", err)
	}
	return coords, nil
}
