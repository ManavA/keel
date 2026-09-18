package geocode

import "context"

// NoopProvider always returns nil, nil: no result, no error. It lets a service
// use Cached and RateLimited without configuring a real geocoding provider.
//
// It never returns (0, 0), so a caller downstream sees the same "no coordinates
// for this address" result whether the provider is a real one with no match or
// this one because none is configured.
type NoopProvider struct{}

// Geocode implements Provider.
func (NoopProvider) Geocode(context.Context, string, string, string, string) (*Coordinates, error) {
	return nil, nil
}
