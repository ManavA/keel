package geocode

import "context"

// NoopProvider always returns nil, nil: no result, no error. It lets a
// service use Cached and RateLimited without configuring a real geocoding
// provider. A caller downstream of a Provider sees the same result — no
// coordinates for this address — whether the underlying Provider is Mapbox
// with no match, or NoopProvider because none is configured.
//
// NoopProvider never returns (0, 0). Code using it during local development
// or in an environment with no geocoding provider gets the same "no
// location for this address" result that a real Provider's unresolved
// address produces.
type NoopProvider struct{}

// Geocode implements Provider by always returning nil, nil.
func (NoopProvider) Geocode(context.Context, string, string, string, string) (*Coordinates, error) {
	return nil, nil
}
