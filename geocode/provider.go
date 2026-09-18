package geocode

import "context"

// How precisely a Coordinates value locates the address.
const (
	// PrecisionAddress is an exact street-address match.
	PrecisionAddress = "address"
	// PrecisionPostcode is a postal-code centroid — right neighbourhood, not
	// the house.
	PrecisionPostcode = "postcode"
	// PrecisionPlace is a city or town centroid, the last stop before giving
	// up entirely.
	PrecisionPlace = "place"
)

// Coordinates is a lat/lng point plus which tier of Provider degradation
// produced it.
type Coordinates struct {
	Latitude  float64
	Longitude float64
	Precision string
}

// Approximate reports whether these coordinates are a centroid rather than a
// resolved street address.
func (c Coordinates) Approximate() bool {
	return c.Precision != PrecisionAddress
}

// Provider converts an address to coordinates. A nil *Coordinates with a nil
// error means the provider could place the address at no precision at all —
// implementations must never return the zero Coordinates value as though it
// were a real result.
type Provider interface {
	Geocode(ctx context.Context, address, city, state, postalCode string) (*Coordinates, error)
}
